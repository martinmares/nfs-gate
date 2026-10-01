package backend

import (
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-git/go-billy/v5"
)

type s3File struct {
	mu     sync.Mutex
	fs     *S3
	name   string
	flag   int
	offset int64
	closed bool
}

func (h *s3File) Name() string { return h.name }
func (h *s3File) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return os.ErrClosed
	}
	h.closed = true
	return nil
}
func (h *s3File) Lock() error   { return billy.ErrNotSupported }
func (h *s3File) Unlock() error { return billy.ErrNotSupported }
func (h *s3File) readAt(p []byte, offset int64) (int, error) {
	if h.closed {
		return 0, os.ErrClosed
	}
	if h.flag&os.O_WRONLY != 0 {
		return 0, os.ErrPermission
	}
	if offset < 0 {
		return 0, os.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	h.fs.state.mu.Lock()
	defer h.fs.state.mu.Unlock()
	i, err := h.fs.stat(h.name)
	if err != nil {
		return 0, err
	}
	if offset >= i.Size() {
		return 0, io.EOF
	}
	count := int64(len(p))
	if count > i.Size()-offset {
		count = i.Size() - offset
	}
	ctx, cancel := h.fs.request()
	defer cancel()
	out, err := h.fs.state.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(h.fs.state.cfg.Bucket), Key: aws.String(h.fs.key(h.name)), Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+count-1))})
	if err != nil {
		return 0, s3Error(err)
	}
	defer out.Body.Close()
	n, err := io.ReadFull(out.Body, p[:int(count)])
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}
func (h *s3File) ReadAt(p []byte, offset int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readAt(p, offset)
}
func (h *s3File) Read(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.readAt(p, h.offset)
	h.offset += int64(n)
	return n, err
}
func (h *s3File) Seek(offset int64, whence int) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, os.ErrClosed
	}
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = h.offset
	case io.SeekEnd:
		i, e := h.fs.Stat(h.name)
		if e != nil {
			return 0, e
		}
		base = i.Size()
	default:
		return 0, os.ErrInvalid
	}
	next := base + offset
	if next < 0 || (offset > 0 && next < base) {
		return 0, os.ErrInvalid
	}
	h.offset = next
	return next, nil
}
func resize(data []byte, size int64) []byte {
	if size <= int64(len(data)) {
		return data[:int(size)]
	}
	return append(data, make([]byte, int(size)-len(data))...)
}
func (h *s3File) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, os.ErrClosed
	}
	if h.flag&(os.O_RDWR|os.O_WRONLY) == 0 {
		return 0, os.ErrPermission
	}
	if len(p) == 0 {
		return 0, nil
	}
	h.fs.state.mu.Lock()
	defer h.fs.state.mu.Unlock()
	offset := h.offset
	err := h.fs.mutate(h.name, func(data []byte, m map[string]string) ([]byte, error) {
		if h.flag&os.O_APPEND != 0 {
			offset = int64(len(data))
		}
		if offset > h.fs.state.cfg.MaxFileSize-int64(len(p)) {
			return nil, syscall.EFBIG
		}
		end := offset + int64(len(p))
		if end > int64(len(data)) {
			data = resize(data, end)
		}
		copy(data[int(offset):], p)
		m["nfs-mtime"] = time.Now().UTC().Format(time.RFC3339Nano)
		return data, nil
	})
	h.fs.record("write", h.name, err)
	if err != nil {
		return 0, err
	}
	h.offset = offset + int64(len(p))
	return len(p), nil
}
func (h *s3File) Truncate(size int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return os.ErrClosed
	}
	if h.flag&(os.O_RDWR|os.O_WRONLY) == 0 {
		return os.ErrPermission
	}
	if size < 0 {
		return os.ErrInvalid
	}
	if size > h.fs.state.cfg.MaxFileSize {
		return syscall.EFBIG
	}
	h.fs.state.mu.Lock()
	defer h.fs.state.mu.Unlock()
	err := h.fs.mutate(h.name, func(data []byte, m map[string]string) ([]byte, error) {
		m["nfs-mtime"] = time.Now().UTC().Format(time.RFC3339Nano)
		return resize(data, size), nil
	})
	h.fs.record("truncate", h.name, err)
	return err
}
