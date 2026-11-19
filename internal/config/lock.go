package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

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
	return lockOpenedRoot(directory, mode, func() bool { return true })
}

// ErrConfigRootBusy reports a configuration root a writer holds.
var ErrConfigRootBusy = errors.New("configuration is changing")

// ObserveRoot takes the shared configuration lock for a read-only observer
// without blocking: it retries until ctx ends and then reports ErrConfigRootBusy.
// Like LockRoot, it never creates a missing root.
func ObserveRoot(ctx context.Context, root string) (func(), error) {
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
	return lockOpenedRoot(directory, syscall.LOCK_SH|syscall.LOCK_NB, func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
			return true
		}
	})
}

// lockOpenedRoot locks directory; a busy non-blocking request retries while
// retry allows it.
func lockOpenedRoot(directory *os.File, mode int, retry func() bool) (func(), error) {
	for {
		err := syscall.Flock(int(directory.Fd()), mode)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(directory.Fd()), syscall.LOCK_UN)
				_ = directory.Close()
			}, nil
		}
		if mode&syscall.LOCK_NB != 0 && errors.Is(err, syscall.EWOULDBLOCK) {
			if retry() {
				continue
			}
			err = ErrConfigRootBusy
		}
		_ = directory.Close()
		if errors.Is(err, ErrConfigRootBusy) {
			return nil, err
		}
		return nil, fmt.Errorf("lock configuration root: %w", err)
	}
}
