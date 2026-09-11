//go:build windows

package kb

// instanceLock is a no-op on Windows (no flock); one instance per clone is
// still required.
type instanceLock struct{}

func acquireInstanceLock(string) (*instanceLock, error) { return &instanceLock{}, nil }

func (l *instanceLock) release() {}
