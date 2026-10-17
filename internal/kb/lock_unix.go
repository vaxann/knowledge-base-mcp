//go:build !windows

package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// instanceLock guards a clone+index pair against a second server process.
type instanceLock struct{ f *os.File }

func acquireInstanceLock(indexDir string) (*instanceLock, error) {
	return lockFile(indexDir, "instance.lock", "another knowledge-base-mcp instance is already using this index (%s); run one server per clone, or attach clients to the running instance's socket")
}

func lockFile(dir, name, busy string) (*instanceLock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf(busy, p)
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &instanceLock{f: f}, nil
}

// DirLock is an exclusive lock held on a directory for the life of a process.
type DirLock = instanceLock

// LockDataDir guards a data directory (state file and repositories) against
// a second server process.
func LockDataDir(dir string) (*DirLock, error) {
	return lockFile(dir, "data.lock", "another knowledge-base-mcp instance is already using this data directory (%s)")
}

// Release gives the lock up.
func (l *instanceLock) Release() { l.release() }

func (l *instanceLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
