package uninstall

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// PublishRuntime delegates exact public asset copying to teardown's pinned
// filesystem guard; the retained runtime survives all installation deletions.
func (store RecoveryStore) PublishRuntime(plan RecoveryPlan) error {
	if err := store.validateDirectory(); err != nil {
		return err
	}
	return nativeArtifact("retain-runtime", struct {
		Plan        RecoveryPlan
		Destination string
	}{plan, filepath.Join(store.Directory, "runtime")}, nil)
}

func privateDirectory(path string) error {
	err := os.Mkdir(path, 0700)
	if err == nil {
		return os.Chmod(path, 0700)
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
		return errors.New("unsafe recovery runtime directory")
	}
	return nil
}
