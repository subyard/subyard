package ownerinventory

import (
	"os"
	"path/filepath"
	"syscall"
)

// lock serializes journal recovery and mutations across independent yard
// processes. The process-local mutex remains useful for goroutines because
// flock locks are associated with a process rather than a goroutine.
func (store Connections) lock() (func(), error) {
	if err := os.MkdirAll(store.Root, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(store.Root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
