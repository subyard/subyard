package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/config"
)

// Serialize desired-state publication and apply across processes, only after
// consent. A separate lock avoids holding the configuration reader/CAS lock
// during potentially slow guest installation.
func lockIntegrationYard(ctx context.Context, loaded config.Loaded) (func(), error) {
	path := filepath.Join(loaded.Context.Paths.ConfigHome, "generated", "integration-locks", loaded.Context.YardName+".lock")
	snapshot, err := config.ReadPersistentFileSnapshot(loaded.Context.Paths.ConfigHome, path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !snapshot.Exists {
		if createErr := config.CreatePersistentFile(loaded.Context.Paths.ConfigHome, path, nil); createErr != nil {
			// A competing operation may have created the stable lock first.
			snapshot, err = config.ReadPersistentFileSnapshot(loaded.Context.Paths.ConfigHome, path)
			if err != nil || !snapshot.Exists {
				return nil, createErr
			}
		}
	}

	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat syscall.Stat_t
	if err = syscall.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0o077 != 0 {
		file.Close()
		return nil, errors.New("unsafe integration lock ownership or permissions")
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, fmt.Errorf("lock integrations: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
