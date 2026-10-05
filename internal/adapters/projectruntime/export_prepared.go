package projectruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

// PreparedPatchStore captures an exact controller destination without creating it.
type PreparedPatchStore struct {
	projectID                 string
	path, directory, ancestor string
	before, ancestorBefore    os.FileInfo
	created                   bool
}

func (store PatchStore) Prepare(projectID string) (*PreparedPatchStore, error) {
	if !domain.SafeID(projectID) || !filepath.IsAbs(store.Directory) {
		return nil, errors.New("invalid project export target")
	}
	prepared := &PreparedPatchStore{projectID: projectID, directory: filepath.Clean(store.Directory)}
	var err error
	prepared.before, err = os.Lstat(prepared.directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && (!prepared.before.IsDir() || prepared.before.Mode()&os.ModeSymlink != 0 || prepared.before.Mode().Perm() != 0o700) {
		return nil, errors.New("export directory must be a protected real directory")
	}
	prepared.ancestor = filepath.Dir(prepared.directory)
	for {
		prepared.ancestorBefore, err = os.Lstat(prepared.ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) || prepared.ancestor == "/" {
			return nil, err
		}
		prepared.ancestor = filepath.Dir(prepared.ancestor)
	}
	if !prepared.ancestorBefore.IsDir() || prepared.ancestorBefore.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("export ancestor must be a real directory")
	}
	root, err := openExportDirectory(prepared.ancestor)
	if err != nil {
		return nil, err
	}
	opened, openedErr := root.Stat(".")
	root.Close()
	if openedErr != nil || !sameExportDirectory(opened, prepared.ancestorBefore) {
		return nil, fmt.Errorf("%w: export ancestor changed", domain.ErrPlanStale)
	}
	now := time.Now()
	if store.Now != nil {
		now = store.Now()
	}
	prepared.path = filepath.Join(prepared.directory, projectID+"-"+now.UTC().Format("20060102T150405.000000000Z")+".patch")
	if _, err := os.Lstat(prepared.path); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("export destination already exists or cannot be inspected")
	}
	return prepared, nil
}

func (store *PreparedPatchStore) Path() string { return store.path }

func (store *PreparedPatchStore) Binding() string {
	facts := func(info os.FileInfo) any {
		if info == nil {
			return nil
		}
		stat, _ := info.Sys().(*syscall.Stat_t)
		if stat == nil {
			return info.Mode().String()
		}
		return []uint64{stat.Dev, stat.Ino, uint64(stat.Uid), uint64(stat.Gid), uint64(info.Mode())}
	}
	payload, _ := json.Marshal([]any{store.path, facts(store.before), store.ancestor, facts(store.ancestorBefore)})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func (store *PreparedPatchStore) Check() error {
	ancestor, err := os.Lstat(store.ancestor)
	if err != nil || !sameExportDirectory(ancestor, store.ancestorBefore) {
		return fmt.Errorf("%w: export ancestor changed", domain.ErrPlanStale)
	}
	current, err := os.Lstat(store.directory)
	if store.before == nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: export directory appeared", domain.ErrPlanStale)
		}
	} else if err != nil || !sameExportDirectory(current, store.before) {
		return fmt.Errorf("%w: export directory changed", domain.ErrPlanStale)
	}
	if _, err := os.Lstat(store.path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: export destination appeared", domain.ErrPlanStale)
	}
	return nil
}

func sameExportDirectory(current, before os.FileInfo) bool {
	if !os.SameFile(current, before) || current.Mode() != before.Mode() {
		return false
	}
	currentStat, currentOK := current.Sys().(*syscall.Stat_t)
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	return currentOK && beforeOK && currentStat.Uid == beforeStat.Uid && currentStat.Gid == beforeStat.Gid
}

// Open each directory relative to its verified parent and refuse every symlink.
func openExportDirectory(path string) (*os.Root, error) {
	root, err := os.OpenRoot("/")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		info, err := root.Lstat(component)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, errors.New("export directory prefix is unavailable or a symlink")
		}
		child, err := root.OpenRoot(component)
		if err != nil {
			root.Close()
			return nil, err
		}
		opened, err := child.Stat(".")
		root.Close()
		if err != nil || !os.SameFile(info, opened) {
			child.Close()
			return nil, fmt.Errorf("%w: export directory prefix changed", domain.ErrPlanStale)
		}
		root = child
	}
	return root, nil
}

func (store *PreparedPatchStore) Publish(ctx context.Context, projectID string, patch []byte) (string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	if store.created || projectID != store.projectID {
		return "", errors.New("export destination is not available for this project")
	}
	if err := store.Check(); err != nil {
		return "", err
	}
	root, err := openExportDirectory(store.ancestor)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	opened, err := root.Stat(".")
	if err != nil || !sameExportDirectory(opened, store.ancestorBefore) {
		return "", fmt.Errorf("%w: export ancestor changed", domain.ErrPlanStale)
	}
	relative, err := filepath.Rel(store.ancestor, store.directory)
	if err != nil {
		return "", err
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "." {
			continue
		}
		if store.before == nil {
			if err := root.Mkdir(component, 0o700); err != nil {
				return "", err
			}
		}
		info, err := root.Lstat(component)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: export directory changed", domain.ErrPlanStale)
		}
		child, err := root.OpenRoot(component)
		if err != nil {
			return "", err
		}
		root.Close()
		root = child
		opened, err := root.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			return "", fmt.Errorf("%w: export directory changed", domain.ErrPlanStale)
		}
		if store.before == nil {
			if err := root.Chmod(".", 0o700); err != nil {
				return "", err
			}
		}
	}
	directoryBefore, err := root.Stat(".")
	if err != nil || directoryBefore.Mode().Perm() != 0o700 || store.before != nil && !sameExportDirectory(directoryBefore, store.before) {
		return "", fmt.Errorf("%w: export directory changed", domain.ErrPlanStale)
	}
	basename := filepath.Base(store.path)
	file, err := root.OpenFile(basename, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	store.created = true
	info, statErr := file.Stat()
	removeOwned := func() {
		if current, err := root.Lstat(basename); err == nil && info != nil && os.SameFile(current, info) {
			_ = root.Remove(basename)
		}
	}
	fail := func(err error) (string, error) { _ = file.Close(); removeOwned(); return "", err }
	if err := errors.Join(statErr, file.Chmod(0o600)); err != nil {
		return fail(err)
	}
	if _, err := file.Write(patch); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	actual, readErr := io.ReadAll(io.LimitReader(file, int64(len(patch))+1))
	current, pathErr := root.Lstat(basename)
	verified, verifyErr := file.Stat()
	directory, dirErr := os.Lstat(store.directory)
	if readErr != nil || !bytes.Equal(actual, patch) || pathErr != nil || verifyErr != nil || !os.SameFile(current, verified) || verified.Mode().Perm() != 0o600 || dirErr != nil || !sameExportDirectory(directory, directoryBefore) {
		return fail(errors.New("exported patch failed native bytes, identity or permission verification"))
	}
	beforeOwnership, _ := info.Sys().(*syscall.Stat_t)
	afterOwnership, _ := verified.Sys().(*syscall.Stat_t)
	if beforeOwnership == nil || afterOwnership == nil || beforeOwnership.Uid != afterOwnership.Uid || beforeOwnership.Gid != afterOwnership.Gid {
		return fail(errors.New("exported patch ownership changed"))
	}
	if err := errors.Join(context.Cause(ctx), file.Close()); err != nil {
		removeOwned()
		return "", err
	}
	return store.path, nil
}
