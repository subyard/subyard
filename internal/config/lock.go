package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
)

// LockRoot holds a process-scoped advisory lock on the configuration directory.
// Readers never create state: a missing root has nothing for a writer to replace.
func LockRoot(root string, exclusive bool) (func(), error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, domain.NewConfigRefusal(domain.ConfigRootNotDirectory)
	}
	directory, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(directory.Fd()), mode); err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("lock configuration root: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(directory.Fd()), syscall.LOCK_UN)
		_ = directory.Close()
	}, nil
}
