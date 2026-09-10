package config

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

// YardRegistrationRepair preserves a shadowed flat registration while keeping
// the canonical nested registration active.
type YardRegistrationRepair struct {
	NestedPath  string
	FlatPath    string
	ArchivePath string

	root   string
	name   string
	nested PersistentFileSnapshot
	flat   PersistentFileSnapshot
}

// PlanYardRegistrationRepair observes the exact files used by a later Apply.
// It never creates the configuration root or recovery directories.
func PlanYardRegistrationRepair(configHome, name string) (*YardRegistrationRepair, error) {
	if name == "default" || !domain.SafeName(name) {
		return nil, errors.New("yard registration repair requires a safe non-default yard name")
	}
	root := filepath.Clean(configHome)
	repair := &YardRegistrationRepair{
		NestedPath:  filepath.Join(root, "yards", name, "config.env"),
		FlatPath:    filepath.Join(root, "yards", name+".env"),
		ArchivePath: filepath.Join(root, "recovery", "yard-registrations", name+".env"),
		root:        root,
		name:        name,
	}
	var err error
	repair.nested, err = ReadPersistentFileSnapshot(root, repair.NestedPath)
	if err != nil {
		return nil, err
	}
	if !repair.nested.Exists {
		return nil, errors.New("canonical nested yard registration does not exist")
	}
	repair.flat, err = ReadPersistentFileSnapshot(root, repair.FlatPath)
	if err != nil || !repair.flat.Exists {
		return repair, err
	}
	archive, err := ReadPersistentFileSnapshot(root, repair.ArchivePath)
	if err != nil {
		return nil, err
	}
	if archive.Exists {
		return nil, fmt.Errorf(
			"cannot preserve %s because recovery archive %s already exists",
			repair.FlatPath, repair.ArchivePath,
		)
	}
	if err := validateRegistrationRepairRecoveryDirectories(root); err != nil {
		return nil, err
	}
	return repair, nil
}

// Changed reports whether Apply will preserve an active flat registration.
func (repair *YardRegistrationRepair) Changed() bool {
	return repair != nil && repair.flat.Exists
}

// Apply moves the exact flat registration observed by the plan into recovery.
func (repair *YardRegistrationRepair) Apply() error {
	return repair.apply(nil)
}

func (repair *YardRegistrationRepair) apply(fault func() error) error {
	if repair == nil || repair.name == "default" || !domain.SafeName(repair.name) ||
		!filepath.IsAbs(repair.root) {
		return errors.New("yard registration repair plan is invalid")
	}
	root, err := unix.Open(
		repair.root,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := validatePersistentDirectoryFD(root); err != nil {
		return err
	}
	if err := unix.Flock(root, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(root, unix.LOCK_UN)

	yards, err := openRegistrationRepairDirectoryAt(root, "yards")
	if err != nil {
		return err
	}
	defer unix.Close(yards)
	nestedDirectory, err := openRegistrationRepairDirectoryAt(yards, repair.name)
	if err != nil {
		return err
	}
	defer unix.Close(nestedDirectory)
	nested, err := readPersistentSnapshotAt(nestedDirectory, "config.env")
	if err != nil {
		return err
	}
	flat, err := readPersistentSnapshotAt(yards, repair.name+".env")
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(nested, repair.nested) ||
		!samePersistentFileSnapshotExact(flat, repair.flat) {
		return ErrPersistentTargetStale
	}
	if !repair.flat.Exists {
		return nil
	}

	recovery, err := openOrCreatePrivateRegistrationRepairDirectoryAt(root, "recovery")
	if err != nil {
		return err
	}
	defer unix.Close(recovery)
	archiveDirectory, err := openOrCreatePrivateRegistrationRepairDirectoryAt(
		recovery, "yard-registrations",
	)
	if err != nil {
		return err
	}
	defer unix.Close(archiveDirectory)
	archive, err := readPersistentSnapshotAt(archiveDirectory, repair.name+".env")
	if err != nil {
		return err
	}
	if archive.Exists {
		return ErrPersistentTargetStale
	}
	if fault != nil {
		if err := fault(); err != nil {
			return err
		}
	}
	nested, err = readPersistentSnapshotAt(nestedDirectory, "config.env")
	if err != nil {
		return err
	}
	flat, err = readPersistentSnapshotAt(yards, repair.name+".env")
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(nested, repair.nested) ||
		!samePersistentFileSnapshotExact(flat, repair.flat) {
		return ErrPersistentTargetStale
	}
	if err := unix.Renameat2(
		yards, repair.name+".env",
		archiveDirectory, repair.name+".env",
		unix.RENAME_NOREPLACE,
	); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOENT) {
			return ErrPersistentTargetStale
		}
		return err
	}
	if err := unix.Fsync(yards); err != nil {
		return err
	}
	return unix.Fsync(archiveDirectory)
}

func openRegistrationRepairDirectoryAt(parent int, name string) (int, error) {
	fd, err := unix.Openat(
		parent, name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return -1, err
	}
	if err := validatePersistentDirectoryFD(fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func openPrivateRegistrationRepairDirectoryAt(parent int, name string) (int, error) {
	fd, err := openRegistrationRepairDirectoryAt(parent, name)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if stat.Mode&0o777 != 0o700 {
		_ = unix.Close(fd)
		return -1, errors.New("yard registration recovery directory must have mode 0700")
	}
	return fd, nil
}

func validateRegistrationRepairRecoveryDirectories(configHome string) error {
	root, err := unix.Open(
		configHome,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := validatePersistentDirectoryFD(root); err != nil {
		return err
	}
	recovery, err := openPrivateRegistrationRepairDirectoryAt(root, "recovery")
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(recovery)
	archive, err := openPrivateRegistrationRepairDirectoryAt(recovery, "yard-registrations")
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	return unix.Close(archive)
}

func openOrCreatePrivateRegistrationRepairDirectoryAt(parent int, name string) (int, error) {
	if err := unix.Mkdirat(parent, name, 0o700); err == nil {
		if err := unix.Fsync(parent); err != nil {
			return -1, err
		}
	} else if !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	return openPrivateRegistrationRepairDirectoryAt(parent, name)
}
