package backend

import (
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
)

// Filesystem is shared by NFS, readiness checks and the inspection UI.
type Filesystem interface {
	billy.Filesystem
	billy.Change
	Kind() string
	Close() error
	Check(context.Context) error
	ListPage(string, int, int) ([]os.FileInfo, bool, error)
}

func (f *Local) Check(_ context.Context) error {
	_, err := f.root.Stat(".")
	return err
}

func (f *Local) Kind() string { return "local" }
