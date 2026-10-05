package releaseruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

// Rename relative to verified directory descriptors: an ancestor replacement
// cannot redirect publication to a different runtime tree.
func publishApprovedDraft(candidate publishedCandidate, root string, rootBefore, releasesBefore os.FileInfo) error {
	releasesFD, err := openApprovedReleaseDirectory(root, rootBefore, releasesBefore)
	if err != nil {
		return err
	}
	defer unix.Close(releasesFD)
	draftInfo, err := os.Lstat(filepath.Dir(candidate.root))
	if err != nil {
		return err
	}
	draftFD, err := openApprovedDirectory(filepath.Dir(candidate.root), draftInfo)
	if err != nil {
		return err
	}
	defer unix.Close(draftFD)
	if err := unix.Renameat2(draftFD, string(candidate.release), releasesFD, string(candidate.release), unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return errors.Join(unix.Fsync(releasesFD), unix.Fsync(draftFD))
}

func openApprovedReleaseDirectory(root string, rootBefore, releasesBefore os.FileInfo) (int, error) {
	rootFD, err := openApprovedDirectory(root, rootBefore)
	if err != nil {
		return -1, err
	}
	defer unix.Close(rootFD)
	releasesFD, err := unix.Openat(rootFD, "releases", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	info, err := approvedDirectoryInfo(releasesFD)
	if err != nil || !os.SameFile(info, releasesBefore) || info.Mode() != releasesBefore.Mode() {
		return -1, errors.Join(fmt.Errorf("%w: runtime release directory changed", domain.ErrPlanStale), unix.Close(releasesFD))
	}
	return releasesFD, nil
}

func openApprovedDirectory(path string, expected os.FileInfo) (int, error) {
	if expected == nil || !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return -1, fmt.Errorf("%w: approved directory is invalid", domain.ErrPlanStale)
	}
	stat, ok := expected.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || expected.Mode().Perm()&0o022 != 0 {
		return -1, fmt.Errorf("%w: unsafe approved directory", domain.ErrPlanStale)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	info, err := approvedDirectoryInfo(fd)
	if err != nil || !os.SameFile(info, expected) || info.Mode() != expected.Mode() {
		return -1, errors.Join(fmt.Errorf("%w: approved directory changed", domain.ErrPlanStale), unix.Close(fd))
	}
	return fd, nil
}

func approvedDirectoryInfo(fd int) (os.FileInfo, error) {
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "approved-directory")
	info, statErr := file.Stat()
	return info, errors.Join(statErr, file.Close())
}
