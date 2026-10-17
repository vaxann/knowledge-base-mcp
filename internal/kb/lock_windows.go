//go:build windows

package kb

// instanceLock is a no-op on Windows (no flock); one instance per clone is
// still required.
type instanceLock struct{}

func acquireInstanceLock(string) (*instanceLock, error) { return &instanceLock{}, nil }

func (l *instanceLock) release() {}

// DirLock is an exclusive lock held on a directory for the life of a process.
type DirLock = instanceLock

// LockDataDir is a no-op on Windows.
func LockDataDir(string) (*DirLock, error) { return &instanceLock{}, nil }

// Release gives the lock up.
func (l *instanceLock) Release() {}
