package backend

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/martinmares/nfs-gate/internal/activity"
)

// Local implements billy.Filesystem on top of os.Root. os.Root prevents
// symlink traversal outside the export, including concurrent path changes.
type Local struct {
	root     *os.Root
	name     string
	activity *activity.Store
}

var _ billy.Filesystem = (*Local)(nil)
var _ billy.Change = (*Local)(nil)

func Open(path string, events *activity.Store) (*Local, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Local{root: root, name: path, activity: events}, nil
}

func (f *Local) Close() error { return f.root.Close() }
func (f *Local) Root() string { return f.name }

func clean(name string) (string, error) {
	if name == "" || name == "/" {
		return ".", nil
	}
	if filepath.IsAbs(name) {
		return "", os.ErrPermission
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return "", os.ErrPermission
		}
	}
	return filepath.Clean(name), nil
}

func (f *Local) record(op, path string, err error) {
	if f.activity != nil {
		f.activity.Add(op, path, err)
	}
}

func (f *Local) Create(name string) (billy.File, error) {
	// go-nfs calls Create for NFSv3 UNCHECKED CREATE even when the file
	// already exists. NFS requires that case to preserve existing data;
	// an explicit SETATTR carries O_TRUNC when the client requested it.
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE, 0666)
}

func (f *Local) Open(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

func (f *Local) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	p, err := clean(name)
	if err != nil {
		f.record("open", name, err)
		return nil, err
	}
	file, err := f.root.OpenFile(p, flag, perm)
	op := "open"
	if flag&os.O_CREATE != 0 {
		op = "create"
	}
	if err != nil {
		f.record(op, p, err)
		return nil, err
	}
	f.record(op, p, nil)
	return &localFile{File: file, path: p, owner: f}, nil
}

func (f *Local) Stat(name string) (os.FileInfo, error) {
	p, err := clean(name)
	if err != nil {
		return nil, err
	}
	return f.root.Stat(p)
}
func (f *Local) Lstat(name string) (os.FileInfo, error) {
	p, err := clean(name)
	if err != nil {
		return nil, err
	}
	return f.root.Lstat(p)
}
func (f *Local) Readlink(name string) (string, error) {
	p, err := clean(name)
	if err != nil {
		return "", err
	}
	return f.root.Readlink(p)
}
func (f *Local) Symlink(target, link string) error {
	p, err := clean(link)
	if err != nil {
		f.record("symlink", link, err)
		return err
	}
	if filepath.IsAbs(target) {
		err = os.ErrPermission
	} else {
		err = f.root.Symlink(target, p)
	}
	f.record("symlink", p, err)
	return err
}
func (f *Local) Rename(from, to string) error {
	a, err := clean(from)
	if err != nil {
		return err
	}
	b, err := clean(to)
	if err != nil {
		return err
	}
	if a == "." || b == "." {
		return os.ErrPermission
	}
	err = f.root.Rename(a, b)
	f.record("rename", a+" → "+b, err)
	return err
}
func (f *Local) Remove(name string) error {
	p, err := clean(name)
	if err != nil {
		return err
	}
	if p == "." {
		return os.ErrPermission
	}
	err = f.root.Remove(p)
	f.record("remove", p, err)
	return err
}
func (f *Local) Join(elem ...string) string { return filepath.Join(elem...) }
func (f *Local) ReadDir(name string) ([]os.FileInfo, error) {
	p, err := clean(name)
	if err != nil {
		return nil, err
	}
	file, err := f.root.Open(p)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	items, err := file.Readdir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name() < items[j].Name() })
	return items, nil
}
func (f *Local) MkdirAll(name string, perm os.FileMode) error {
	p, err := clean(name)
	if err != nil {
		return err
	}
	err = f.root.MkdirAll(p, perm)
	f.record("mkdir", p, err)
	return err
}
func (f *Local) TempFile(dir, prefix string) (billy.File, error) {
	p, err := clean(dir)
	if err != nil {
		return nil, err
	}
	for i := 0; i < 10; i++ {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(p, prefix+hex.EncodeToString(random[:]))
		file, err := f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, os.ErrExist
}
func (f *Local) Chroot(name string) (billy.Filesystem, error) {
	p, err := clean(name)
	if err != nil {
		return nil, err
	}
	root, err := f.root.OpenRoot(p)
	if err != nil {
		return nil, err
	}
	return &Local{root: root, name: filepath.Join(f.name, p), activity: f.activity}, nil
}
func (f *Local) Chmod(name string, mode os.FileMode) error {
	p, err := clean(name)
	if err != nil {
		return err
	}
	err = f.root.Chmod(p, mode)
	f.record("chmod", p, err)
	return err
}
func (f *Local) Chown(name string, uid, gid int) error {
	p, err := clean(name)
	if err != nil {
		return err
	}
	err = f.root.Chown(p, uid, gid)
	f.record("chown", p, err)
	return err
}
func (f *Local) Lchown(name string, uid, gid int) error {
	p, err := clean(name)
	if err != nil {
		return err
	}
	err = f.root.Lchown(p, uid, gid)
	f.record("lchown", p, err)
	return err
}
func (f *Local) Chtimes(name string, atime, mtime time.Time) error {
	p, err := clean(name)
	if err != nil {
		return err
	}
	err = f.root.Chtimes(p, atime, mtime)
	f.record("chtimes", p, err)
	return err
}

type localFile struct {
	*os.File
	path  string
	owner *Local
}

func (f *localFile) Name() string { return f.path }

func (f *localFile) Write(p []byte) (int, error) {
	n, err := f.File.Write(p)
	f.owner.record("write", f.path, err)
	return n, err
}
func (f *localFile) Truncate(size int64) error {
	err := f.File.Truncate(size)
	f.owner.record("truncate", f.path, err)
	return err
}
func (f *localFile) Lock() error   { return billy.ErrNotSupported }
func (f *localFile) Unlock() error { return billy.ErrNotSupported }

// ListPage reads one directory at a time. Symlink directory targets are never followed.
func (f *Local) ListPage(name string, page, limit int) ([]os.FileInfo, bool, error) {
	p, err := clean(name)
	if err != nil {
		return nil, false, err
	}
	info, err := f.root.Lstat(p)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, os.ErrInvalid
	}
	if page < 0 || limit < 1 || limit > 200 {
		return nil, false, os.ErrInvalid
	}
	file, err := f.root.Open(p)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	// Directory order is filesystem order, stable enough for an inspection UI.
	skip := page * limit
	for skip > 0 {
		batch := skip
		if batch > 200 {
			batch = 200
		}
		items, e := file.ReadDir(batch)
		skip -= len(items)
		if e == io.EOF {
			return nil, false, nil
		}
		if e != nil {
			return nil, false, e
		}
	}
	entries, err := file.ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	items := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := f.root.Lstat(filepath.Join(p, entry.Name()))
		if err == nil {
			items = append(items, info)
		}
	}
	return items, more, nil
}
