package codexdesktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"golang.org/x/sys/unix"
)

var errStale = errors.New("desktop config changed after assessment; retry the export action")

// Reuse the established protected-file identity contract; traversal here also
// supports an absent GUI home, unlike the settings-only persistent CAS helper.
type snapshot = config.PersistentFileSnapshot
type identity = config.PersistentFileIdentity

func statIdentity(st unix.Stat_t) identity {
	return identity{Device: uint64(st.Dev), Inode: st.Ino, Mode: st.Mode, UID: st.Uid, GID: st.Gid, Links: uint64(st.Nlink)}
}

func inspect(target string) (snapshot, []identity, error) {
	fd, dirs, err := walkParent(target, false, nil)
	if err != nil {
		return snapshot{}, nil, err
	}
	if fd < 0 {
		return snapshot{}, dirs, nil
	}
	defer unix.Close(fd)
	observed, err := readAt(fd, filepath.Base(target))
	return observed, dirs, err
}

// Walk from / with nofollow descriptors. Root-owned system ancestors and
// root-owned sticky temporary roots are allowed; every application directory
// must be operator-owned and must not permit group/other writes.
func walkParent(target string, create bool, expected []identity) (result int, dirs []identity, resultErr error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, nil, err
	}
	defer func() {
		if result < 0 {
			_ = unix.Close(fd)
		}
	}()
	parts := strings.Split(strings.TrimPrefix(filepath.Dir(target), "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		parts = nil
	}
	check := func(final bool) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return errors.New("unsafe desktop config ancestor type")
		}
		operator := st.Uid == uint32(os.Getuid())
		if (!operator && st.Uid != 0) || (final && !operator) {
			return errors.New("desktop config directory is not operator-owned")
		}
		stickyRoot := !final && st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
		if st.Mode&0o022 != 0 && !stickyRoot {
			return errors.New("desktop config ancestor is group/world writable")
		}
		id := statIdentity(st)
		// Directory link count changes normally as child directories are added.
		id.Links = 0
		if len(dirs) < len(expected) && id != expected[len(dirs)] {
			return errStale
		}
		dirs = append(dirs, id)
		return nil
	}
	if err := check(len(parts) == 0); err != nil {
		return -1, nil, err
	}
	for i, part := range parts {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			if len(dirs) < len(expected) {
				return -1, nil, errStale
			}
			if !create {
				return -1, dirs, nil
			}
			mkdirErr := unix.Mkdirat(fd, part, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return -1, nil, mkdirErr
			}
			if mkdirErr == nil {
				// Exact creation mode is independent of umask, even if it masks
				// owner permissions. Existing directories are never repaired.
				if err := unix.Fchmodat(fd, part, 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					return -1, nil, err
				}
			}
			next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			return -1, nil, fmt.Errorf("open desktop config ancestor: %w", err)
		}
		_ = unix.Close(fd)
		fd = next
		if err := check(i == len(parts)-1); err != nil {
			return -1, nil, err
		}
	}
	return fd, dirs, nil
}

func readAt(parent int, name string) (snapshot, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return snapshot{}, nil
	}
	if err != nil {
		return snapshot{}, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return snapshot{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return snapshot{}, errors.New("desktop config must be a regular file without symbolic or hard links")
	}
	if st.Uid != uint32(os.Getuid()) || st.Mode&0o022 != 0 {
		return snapshot{}, errors.New("desktop config has unsafe ownership or write permissions")
	}
	if st.Size > maxConfigBytes {
		return snapshot{}, errors.New("desktop config exceeds its size bound")
	}
	content, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return snapshot{}, err
	}
	if len(content) > maxConfigBytes {
		return snapshot{}, errors.New("desktop config exceeds its size bound")
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return snapshot{}, err
	}
	if statIdentity(st) != statIdentity(after) || st.Size != after.Size || st.Mtim != after.Mtim || st.Ctim != after.Ctim {
		return snapshot{}, errStale
	}
	return snapshot{Exists: true, Content: content, Identity: statIdentity(st)}, nil
}

func sameSnapshot(a, b snapshot) bool {
	return a.Exists == b.Exists && a.Identity == b.Identity && bytes.Equal(a.Content, b.Content)
}

func apply(ctx context.Context, target string, baseline, backup snapshot, ancestors []identity, desired []byte, fault func(string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, dirs, err := walkParent(target, true, ancestors)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	// One directory lock serializes exports to the same desktop configuration home.
	for {
		err = unix.Flock(parent, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("desktop export lock: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer unix.Flock(parent, unix.LOCK_UN)
	name := filepath.Base(target)
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		reopened, _, err := walkParent(target, false, dirs)
		if err != nil {
			return err
		}
		if reopened < 0 {
			return errStale
		}
		_ = unix.Close(reopened)
		observed, err := readAt(parent, name)
		if err != nil {
			return err
		}
		if !sameSnapshot(observed, baseline) {
			return errStale
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	if baseline.Exists {
		observed, err := readAt(parent, name+".subyard-backup")
		if err != nil {
			return err
		}
		if !sameSnapshot(observed, backup) {
			return errStale
		}
	}
	pending, err := stageAt(parent, name, desired)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(parent, pending, 0)
	if fault != nil {
		if err := fault("after-pending-fsync"); err != nil {
			return err
		}
	}
	if err := check(); err != nil {
		return err
	}
	if baseline.Exists {
		if fault != nil {
			if err := fault("before-backup"); err != nil {
				return err
			}
		}
		backupPending, err := stageAt(parent, name+".subyard-backup", baseline.Content)
		if err != nil {
			return err
		}
		defer unix.Unlinkat(parent, backupPending, 0)
		observed, err := readAt(parent, name+".subyard-backup")
		if err != nil {
			return err
		}
		if !sameSnapshot(observed, backup) {
			return errStale
		}
		if err := publishAt(parent, backupPending, name+".subyard-backup", backup.Exists); err != nil {
			return fmt.Errorf("save desktop config backup: %w", err)
		}
		if err := unix.Fsync(parent); err != nil {
			return err
		}
	}
	if fault != nil {
		if err := fault("before-publish"); err != nil {
			return err
		}
	}
	if err := check(); err != nil {
		return err
	}
	if err := publishAt(parent, pending, name, baseline.Exists); err != nil {
		return fmt.Errorf("publish desktop config: %w", err)
	}
	return unix.Fsync(parent)
}

func publishAt(parent int, pending, name string, replace bool) error {
	if replace {
		return unix.Renameat(parent, pending, parent, name)
	}
	return unix.Renameat2(parent, pending, parent, name, unix.RENAME_NOREPLACE)
}

func stageAt(parent int, name string, content []byte) (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	pending := "." + name + ".subyard-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(parent, pending, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), pending)
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = unix.Unlinkat(parent, pending, 0)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return pending, nil
}
