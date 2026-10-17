package releasetransition

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Owner receipts contain a newly minted grant. Keep every byte private until
// the complete canonical record is durable; a killed writer leaves no prefix.
// Ordinary ledger, evidence and CAS publication retain their existing contract.
func (store *POSIXV2Store) createOwnerImmutable(parts []string, name string, payload []byte) error {
	if err := validateProtectedPayload(payload); err != nil {
		return err
	}
	if err := validateSafeID(name, "owner receipt filename"); err != nil {
		return err
	}
	parent, present, err := store.openParent(parts, false)
	if err != nil {
		return err
	}
	if present {
		current, readErr := readProtectedAt(parent, name)
		if readErr != nil {
			unix.Close(parent)
			return readErr
		}
		if current.Exists {
			defer unix.Close(parent)
			if !bytes.Equal(current.Payload, payload) {
				return ErrProtectedStoreExists
			}
			return unix.Fsync(parent)
		}
		pending, readErr := readProtectedAt(parent, "."+name+".pending")
		unix.Close(parent)
		if readErr != nil {
			return readErr
		}
		if pending.Exists {
			if !bytes.Equal(pending.Payload, payload) {
				return ErrProtectedStoreExists
			}
			// Preserve the old full-canonical pending contract. It needs no new
			// bytes or nonce reconstruction, only the existing atomic rename.
			return store.createImmutable(parts, name, payload)
		}
	}
	// Admit the filesystem before creating missing protected directories.
	// The nearest existing ancestor determines the future target filesystem.
	var anchor int
	for length := len(parts); ; length-- {
		anchor, present, err = store.openParent(parts[:length], false)
		if err != nil {
			return err
		}
		if present {
			break
		}
		if length == 0 {
			return invalid("owner receipt parent is unavailable")
		}
	}
	defer unix.Close(anchor)
	if err := store.inject("before-private-create"); err != nil {
		return err
	}
	fd, err := unix.Openat(anchor, ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("owner receipt requires private unnamed publication on this filesystem: %w", err)
	}
	file := os.NewFile(uintptr(fd), "private-owner-receipt")
	defer file.Close()
	if err := unix.Fchmod(fd, 0600); err != nil {
		return err
	}
	var private unix.Stat_t
	if err := unix.Fstat(fd, &private); err != nil {
		return err
	}
	if private.Mode&unix.S_IFMT != unix.S_IFREG || private.Mode&0777 != 0600 || private.Uid != uint32(os.Getuid()) || private.Nlink != 0 || private.Size != 0 {
		return invalid("unsafe private owner receipt")
	}
	proc := "/proc/self/fd/" + strconv.Itoa(fd)
	var procInfo unix.Stat_t
	if err := unix.Stat(proc, &procInfo); err != nil || procInfo.Dev != private.Dev || procInfo.Ino != private.Ino {
		return invalid("owner receipt requires the held procfd publication path")
	}
	if err := store.inject("after-private-create"); err != nil {
		return err
	}
	remaining := payload
	for len(remaining) > 0 {
		n, err := file.Write(remaining)
		if err != nil {
			return err
		}
		remaining = remaining[n:]
	}
	if err := file.Sync(); err != nil {
		return err
	}
	readback := make([]byte, len(payload))
	if _, err := file.ReadAt(readback, 0); err != nil {
		return err
	}
	var complete unix.Stat_t
	if err := unix.Fstat(fd, &complete); err != nil {
		return err
	}
	if complete.Dev != private.Dev || complete.Ino != private.Ino || complete.Mode != private.Mode || complete.Uid != private.Uid || complete.Nlink != 0 || complete.Size != int64(len(payload)) || !bytes.Equal(readback, payload) {
		return invalid("private owner receipt changed before publication")
	}
	if err := store.inject("after-pending-fsync"); err != nil {
		return err
	}
	parent, present, err = store.openParent(parts, true)
	if err != nil {
		return err
	}
	if !present {
		return invalid("owner receipt parent is unavailable")
	}
	defer unix.Close(parent)
	var targetParent unix.Stat_t
	if err := unix.Fstat(parent, &targetParent); err != nil {
		return err
	}
	if targetParent.Dev != private.Dev {
		return invalid("owner receipt target filesystem changed")
	}
	current, err := readProtectedAt(parent, name)
	if err != nil {
		return err
	}
	if current.Exists {
		return ErrProtectedStoreExists
	}
	if err := store.inject("before-publish"); err != nil {
		return err
	}
	// The procfd route works for the ordinary owning UID; AT_EMPTY_PATH
	// would require CAP_DAC_READ_SEARCH. linkat never replaces a destination.
	err = unix.Linkat(unix.AT_FDCWD, proc, parent, name, unix.AT_SYMLINK_FOLLOW)
	if errors.Is(err, unix.EEXIST) {
		return ErrProtectedStoreExists
	}
	if err != nil {
		return err
	}
	var linked unix.Stat_t
	if err := unix.Fstatat(parent, name, &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	actual, err := readProtectedAt(parent, name)
	if err != nil {
		return err
	}
	if linked.Dev != private.Dev || linked.Ino != private.Ino || linked.Nlink != 1 || !bytes.Equal(actual.Payload, payload) {
		return invalid("owner receipt identity changed at publication")
	}
	if err := store.inject("after-publish-before-dir-fsync"); err != nil {
		return err
	}
	return unix.Fsync(parent)
}
