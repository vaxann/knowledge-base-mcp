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
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(indexDir, "instance.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another knowledge-base-mcp instance is already using this index (%s); run one server per clone, or attach clients to the running instance's socket", p)
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &instanceLock{f: f}, nil
}

func (l *instanceLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
