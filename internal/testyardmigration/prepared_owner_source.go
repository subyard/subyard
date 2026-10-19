package testyardmigration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

type PreparedOwnerMember = ports.NativeOwnerMember

func retainedOwnerMember(fd int, before Prepared) (PreparedOwnerMember, error) {
	if err := publishedRegistrationFDMatches(fd, "original owner member", before); err != nil {
		return PreparedOwnerMember{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return PreparedOwnerMember{}, err
	}
	return PreparedOwnerMember{Device: st.Dev, Inode: st.Ino, UID: st.Uid, GID: st.Gid, Mode: st.Mode, Links: uint64(st.Nlink), Size: st.Size, ModifiedSeconds: st.Mtim.Sec, ModifiedNanos: st.Mtim.Nsec, ChangedSeconds: st.Ctim.Sec, ChangedNanos: st.Ctim.Nsec, Digest: before.RegistrationDigest}, nil
}

func retainedArchiveMember(object *boundObject, directory bool, before Prepared) (int, error) {
	var fd int
	var err error
	if directory {
		fd, err = unix.Openat(object.fd, "config.env", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	} else {
		fd, err = unix.FcntlInt(uintptr(object.fd), unix.F_DUPFD_CLOEXEC, 0)
	}
	if err != nil {
		return -1, err
	}
	if _, err := retainedOwnerMember(fd, before); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func (p PreparedOwnerSource) matchesRestored(st unix.Stat_t, result PreparedOwnerMember) bool {
	return p.matches(st, true) && p.originalMember(result) && result.ChangedSeconds == st.Ctim.Sec && result.ChangedNanos == st.Ctim.Nsec
}

func (p PreparedOwnerSource) originalMember(result PreparedOwnerMember) bool {
	digest := sha256.Sum256(p.Content)
	return result.Device == p.Device && result.Inode == p.Inode && result.UID == p.UID && result.GID == p.GID && result.Mode == p.Mode && result.Links == p.Links && result.Size == p.Size && result.ModifiedSeconds == p.Modified.Sec && result.ModifiedNanos == p.Modified.Nsec && result.Digest == hex.EncodeToString(digest[:])
}

func (p PreparedOwnerSource) ValidateRestoredMember(result PreparedOwnerMember) error {
	if !p.originalMember(result) || result.ChangedNanos < 0 || result.ChangedNanos >= 1e9 {
		return errors.New("restored owner registration differs from original member")
	}
	return nil
}

// PreparedOwnerSource names only the registration selected by Before. It is
// an original read fact, never a caller-selected alias or a grant to mutate it.
type PreparedOwnerSource struct {
	Before                Prepared
	Content               []byte
	RootDevice, RootInode uint64
	RootUID, RootMode     uint32
	Device, Inode         uint64
	UID, GID, Mode        uint32
	Links                 uint64
	Size                  int64
	Modified, Changed     unix.Timespec
}

type CheckedOwnerSource struct {
	registration   *boundRegistration
	path           string
	before         PreparedOwnerSource
	archived       bool
	root           *os.File
	options        Options
	restored       *PreparedOwnerMember
	checkedChanged unix.Timespec
}

func CaptureOwnerChildSource(options Options, before Prepared, pinnedRoot *os.File) (PreparedOwnerSource, error) {
	var result PreparedOwnerSource
	if err := validateOptions(&options); err != nil {
		return result, err
	}
	if err := validatePrepared(before); err != nil {
		return result, err
	}
	shape, _, legacy := preparedLegacyState(before.State)
	if !legacy {
		return result, errors.New("original owner registration source is not legacy")
	}
	path, _ := registrationPaths(options, shape)
	registration, err := openBoundPublishedRegistration(path, before)
	if err != nil {
		return result, err
	}
	defer registration.close()
	root, err := ownerSourceRoot(options, pinnedRoot)
	if err != nil {
		return result, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(registration.fd, &st); err != nil {
		return result, err
	}
	payload, err := readRetainedFile(registration.fd, path, st.Size)
	if err != nil {
		return result, err
	}
	result = PreparedOwnerSource{Before: before, Content: payload, RootDevice: root.Dev, RootInode: root.Ino, RootUID: root.Uid, RootMode: root.Mode, Device: st.Dev, Inode: st.Ino, UID: st.Uid, GID: st.Gid, Mode: st.Mode, Links: uint64(st.Nlink), Size: st.Size, Modified: st.Mtim, Changed: st.Ctim}
	return result, registration.requireNamed()
}

func ownerSourceRoot(options Options, pinned *os.File) (unix.Stat_t, error) {
	var st unix.Stat_t
	if pinned == nil {
		return st, errors.New("owner source root descriptor unavailable")
	}
	if err := unix.Fstat(int(pinned.Fd()), &st); err != nil {
		return st, err
	}
	info, err := os.Lstat(options.ConfigHome)
	if err != nil {
		return st, err
	}
	retained, err := pinned.Stat()
	if err != nil {
		return st, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, retained) {
		return st, errors.New("owner source root changed")
	}
	return st, nil
}

// CheckOwnerChildSource accepts an archive only when the owning migration's
// original token and source/archive exclusivity prove that exact rename. The
// original inode, ownership, mode, size, mtime and bytes must all remain exact.
func CheckOwnerChildSource(options Options, before Prepared, p PreparedOwnerSource, pinned *os.File) (*CheckedOwnerSource, error) {
	return checkOwnerChildSource(options, before, p, pinned, nil)
}

// CheckRestoredOwnerChildSource requires the result from the original bound
// native invocation. The caller must authenticate that immutable fact before
// supplying it; the owning component still checks the full original member.
func CheckRestoredOwnerChildSource(options Options, before Prepared, p PreparedOwnerSource, pinned *os.File, result PreparedOwnerMember) (*CheckedOwnerSource, error) {
	return checkOwnerChildSource(options, before, p, pinned, &result)
}

func checkOwnerChildSource(options Options, before Prepared, p PreparedOwnerSource, pinned *os.File, restored *PreparedOwnerMember) (*CheckedOwnerSource, error) {
	if err := validateOptions(&options); err != nil {
		return nil, err
	}
	if p.Before != before {
		return nil, errors.New("owner child original registration differs")
	}
	root, err := ownerSourceRoot(options, pinned)
	if err != nil {
		return nil, err
	}
	if root.Dev != p.RootDevice || root.Ino != p.RootInode || root.Uid != p.RootUID || root.Mode != p.RootMode {
		return nil, errors.New("owner child original root differs")
	}
	shape, _, legacy := preparedLegacyState(before.State)
	if !legacy {
		return nil, errors.New("owner source scope unavailable")
	}
	path, current := registrationPaths(options, shape)
	physical := path
	archived := false
	if exists, err := pathExists(path); err != nil {
		return nil, err
	} else if !exists {
		if err := validateRecoveryToken(options.RecoveryToken); err != nil {
			return nil, err
		}
		if err := registrationMatches(current, before); err != nil {
			return nil, err
		}
		if err := validatePreparedRegistrationArchive(options, path, before, auxiliaryInProgress); err != nil {
			return nil, err
		}
		archive, err := preparedRegistrationArchive(options, before)
		if err != nil {
			return nil, err
		}
		physical = archive.archive
		if archive.directory {
			physical = filepath.Join(physical, "config.env")
		}
		archived = true
	}
	registration, err := openBoundPublishedRegistration(physical, before)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*CheckedOwnerSource, error) { registration.close(); return nil, err }
	var st unix.Stat_t
	if err := unix.Fstat(registration.fd, &st); err != nil {
		return fail(err)
	}
	if !p.matches(st, archived) && (restored == nil || archived || !p.matchesRestored(st, *restored)) {
		return fail(errors.New("owner child original registration identity changed"))
	}
	payload, err := readRetainedFile(registration.fd, physical, st.Size)
	if err != nil || !bytes.Equal(payload, p.Content) {
		return fail(errors.New("owner child original registration bytes changed"))
	}
	if _, err := ownerSourceRoot(options, pinned); err != nil {
		return fail(err)
	}
	return &CheckedOwnerSource{registration: registration, path: path, before: p, archived: archived, root: pinned, options: options, restored: restored, checkedChanged: st.Ctim}, nil
}

func (p PreparedOwnerSource) matches(st unix.Stat_t, archived bool) bool {
	return st.Dev == p.Device && st.Ino == p.Inode && st.Uid == p.UID && st.Gid == p.GID && st.Mode == p.Mode && uint64(st.Nlink) == p.Links && st.Size == p.Size && st.Mtim == p.Modified && (archived || st.Ctim == p.Changed)
}

func (s *CheckedOwnerSource) check() error {
	root, err := ownerSourceRoot(s.options, s.root)
	if err != nil {
		return err
	}
	if root.Dev != s.before.RootDevice || root.Ino != s.before.RootInode || root.Uid != s.before.RootUID || root.Mode != s.before.RootMode {
		return errors.New("owner child original root differs")
	}
	if err := s.registration.requireNamed(); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstat(s.registration.fd, &st); err != nil {
		return err
	}
	if !s.before.matches(st, s.archived) && (s.restored == nil || s.archived || !s.before.matchesRestored(st, *s.restored)) {
		return errors.New("owner child original registration identity changed")
	}
	if st.Ctim != s.checkedChanged {
		return errors.New("owner child checked registration changed")
	}
	return nil
}

func (s *CheckedOwnerSource) Close() error {
	if s != nil && s.registration != nil {
		s.registration.close()
		s.registration = nil
	}
	return nil
}

func (s *CheckedOwnerSource) OriginalPath() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *CheckedOwnerSource) ReadOriginal(path string) (config.PersistentFileSnapshot, bool, error) {
	if s == nil || s.registration == nil || path != s.path {
		return config.PersistentFileSnapshot{}, false, nil
	}
	if err := s.check(); err != nil {
		return config.PersistentFileSnapshot{}, true, err
	}
	payload, err := readRetainedFile(s.registration.fd, s.path, s.before.Size)
	if err != nil || !bytes.Equal(payload, s.before.Content) {
		return config.PersistentFileSnapshot{}, true, errors.New("owner child original registration bytes changed")
	}
	if err := s.check(); err != nil {
		return config.PersistentFileSnapshot{}, true, err
	}
	return config.PersistentFileSnapshot{Exists: true, Content: bytes.Clone(s.before.Content), Identity: config.PersistentFileIdentity{Device: s.before.Device, Inode: s.before.Inode, Mode: s.before.Mode, UID: s.before.UID, GID: s.before.GID, Links: s.before.Links}}, true, nil
}
