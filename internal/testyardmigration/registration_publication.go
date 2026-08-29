package testyardmigration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

const ownerRecoveryPrefix = ".owner-migration-"

func validateRecoveryToken(token string) error {
	if len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" {
		return errors.New("owner recovery token is invalid")
	}
	return nil
}

func ownerRecoveryName(kind, token string) (string, error) {
	if err := validateRecoveryToken(token); err != nil {
		return "", err
	}
	return ownerRecoveryPrefix + kind + "." + token, nil
}

func ownerArchivePath(parent, kind, token string) (string, error) {
	name, err := ownerRecoveryName(kind+"-archive", token)
	return filepath.Join(parent, name), err
}

func validateBoundedOwnerEntries(directory string, allowed ...string) error {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ownerRecoveryPrefix) &&
			!slices.Contains(allowed, entry.Name()) {
			return fmt.Errorf("unbound owner recovery entry %q", entry.Name())
		}
	}
	return nil
}

func validateCurrentStateDirectory(current string, adopt bool) error {
	directory := filepath.Dir(current)
	if filepath.Base(current) != "config.env" {
		return validateBoundedOwnerEntries(directory)
	}
	exists, err := pathExists(directory)
	if err != nil || !exists {
		return err
	}
	if !adopt {
		return errors.New("test-yard directory already exists; refusing to replace it")
	}
	if err := ownedDirectory(directory); err != nil {
		return fmt.Errorf("existing test-yard state directory is unsafe: %w", err)
	}
	return validateBoundedOwnerEntries(directory)
}

func validateAuthorizedCurrentStateDirectory(current string, adopt bool, token string) error {
	if filepath.Base(current) != "config.env" {
		scratch, err := ownerRecoveryName("registration-scratch", token)
		if err != nil {
			return err
		}
		return validateBoundedOwnerEntries(filepath.Dir(current), scratch)
	}
	directory := filepath.Dir(current)
	exists, err := pathExists(directory)
	if err != nil || !exists {
		return err
	}
	if adopt {
		return ownedDirectory(directory)
	}
	if err := ownedDirectory(directory); err != nil {
		return fmt.Errorf("interrupted test-yard state directory is unsafe: %w", err)
	}
	entries, err := os.ReadDir(directory)
	if err == nil && len(entries) != 0 {
		err = errors.New("test-yard directory contains state outside the authorized migration")
	}
	return err
}

type registrationStaging struct {
	parentFD int
	name     string
	file     *os.File
}

func copyBoundRegistration(options Options, source *boundRegistration, destination string, prepared Prepared) (resultErr error) {
	payload, err := source.payload(prepared)
	if err != nil {
		return err
	}
	parentPath := registrationStagingParent(destination)
	parent, err := openBoundObject(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer parent.close()
	scratch, err := ownerRecoveryName("registration-scratch", options.RecoveryToken)
	if err != nil {
		return err
	}
	if err := validateBoundedOwnerEntries(parentPath, scratch); err != nil {
		return err
	}
	staging, err := createRegistrationStaging(options, parent.fd, scratch, payload)
	if err != nil {
		return fmt.Errorf("stage owner registration: %w", err)
	}
	defer func() {
		if resultErr != nil && !isInjectedFault(resultErr) {
			removed, cleanupErr := staging.unlink()
			if removed && cleanupErr == nil {
				cleanupErr = syncOwnerDirectory(options, "registration-staging-cleanup", parent.fd)
			}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
		staging.close()
	}()
	if err := inject(options, "after-registration-staging-fsync"); err != nil {
		return err
	}

	directory := filepath.Dir(destination)
	created := false
	if directory != parentPath {
		if err := unix.Mkdirat(parent.fd, filepath.Base(directory), 0o700); err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			return err
		}
		if err := syncOwnerDirectory(options, "destination-parent", parent.fd); err != nil {
			return err
		}
	}
	destinationDirectory, err := openBoundObject(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer destinationDirectory.close()
	if created {
		if err := inject(options, "after-destination-directory-creation"); err != nil {
			return err
		}
	}
	if err := source.matches(prepared); err != nil {
		return err
	}
	if err := source.requireNamed(); err != nil {
		return fmt.Errorf("owner registration changed before publication: %w", err)
	}
	if err := inject(options, "before-registration-publication-cas"); err != nil {
		return err
	}
	if err := destinationDirectory.requireNamed(); err != nil {
		return fmt.Errorf("owner registration destination directory changed: %w", err)
	}
	if err := validateRetainedDirectoryFD(destinationDirectory.fd, "registration destination directory"); err != nil {
		return err
	}
	if err := staging.validate(payload); err != nil {
		return err
	}
	return renameBoundObject(
		options,
		&boundObject{parentFD: staging.parentFD, fd: int(staging.file.Fd()), name: staging.name},
		destinationDirectory.fd,
		filepath.Base(destination),
		unix.O_RDONLY|unix.O_NONBLOCK,
		func(fd int) error {
			return registrationFDMatches(fd, "published owner registration", prepared)
		},
		"", "registration-publication", "after-registration-publication",
	)
}

func registrationStagingParent(destination string) string {
	directory := filepath.Dir(destination)
	if filepath.Base(destination) == "config.env" {
		return filepath.Dir(directory)
	}
	return directory
}

func createRegistrationStaging(options Options, parentFD int, name string, payload []byte) (*registrationStaging, error) {
	fd, err := openRegistrationStaging(options, parentFD, name)
	existing := errors.Is(err, unix.EEXIST)
	if existing {
		fd, err = unix.Openat(parentFD, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open owner registration staging file")
	}
	staging := &registrationStaging{parentFD: parentFD, name: name, file: file}
	if existing {
		actual, readErr := staging.payload()
		if readErr != nil || len(actual) > len(payload) || !bytes.Equal(actual, payload[:len(actual)]) {
			staging.close()
			if readErr != nil {
				return nil, readErr
			}
			return nil, errors.New("bound owner registration staging bytes mismatch")
		}
		if len(actual) == len(payload) {
			if err := syncRegistrationFile(options, file); err != nil {
				return nil, staging.discard(options, err)
			}
			return staging, nil
		}
		if err := file.Truncate(0); err != nil {
			staging.close()
			return nil, err
		}
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		err = file.Chmod(0o600)
	}
	if err == nil {
		_, err = file.Write(payload)
	}
	if err == nil {
		err = syncRegistrationFile(options, file)
	}
	if err != nil {
		return nil, staging.discard(options, err)
	}
	return staging, nil
}

func openRegistrationStaging(options Options, parentFD int, name string) (int, error) {
	if options.openRegistrationStaging != nil {
		return options.openRegistrationStaging(parentFD, name)
	}
	return unix.Openat(parentFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
}

func (staging *registrationStaging) close() {
	if staging.file != nil {
		_ = staging.file.Close()
		staging.file = nil
	}
}

func (staging *registrationStaging) discard(options Options, cause error) error {
	removed, err := staging.unlink()
	if removed && err == nil {
		err = syncOwnerDirectory(options, "registration-staging-cleanup", staging.parentFD)
	}
	staging.close()
	return errors.Join(cause, err)
}

func (staging *registrationStaging) payload() ([]byte, error) {
	var stat unix.Stat_t
	fd := int(staging.file.Fd())
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, errors.New("expected an owned mode-0600 regular staging file")
	}
	return readRetainedFile(fd, staging.name, stat.Size)
}

func (staging *registrationStaging) validate(payload []byte) error {
	actual, err := staging.payload()
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, payload) {
		return errors.New("staging state does not match the authorized registration")
	}
	return nil
}

func (staging *registrationStaging) unlink() (bool, error) {
	if err := requireBoundName(staging.parentFD, staging.name, int(staging.file.Fd())); errors.Is(err, unix.ENOENT) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, unix.Unlinkat(staging.parentFD, staging.name, 0)
}

func repairAuthorizedRegistrationPublication(options Options, destination string, prepared Prepared) error {
	canonical, err := openBoundObject(destination, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer canonical.close()
	if err := publishedRegistrationFDMatches(canonical.fd, canonical.name, prepared); err != nil {
		return err
	}
	if err := canonical.requireNamed(); err != nil {
		return err
	}
	scratch, err := ownerRecoveryName("registration-scratch", options.RecoveryToken)
	if err != nil {
		return err
	}
	return repairMovedOwnerState(
		options, filepath.Join(registrationStagingParent(destination), scratch),
		destination, "registration-publication",
	)
}

type registrationArchive struct {
	registration, source, archive string
	directory                     bool
}

func preparedRegistrationArchive(options Options, prepared Prepared) (registrationArchive, error) {
	shape, _, migrates := preparedLegacyState(prepared.State)
	if !migrates {
		return registrationArchive{}, errors.New("owner registration archive requires a legacy state")
	}
	registration, _ := registrationPaths(options, shape)
	source := registration
	directory := filepath.Base(registration) == "config.env"
	if directory {
		source = filepath.Dir(source)
	}
	archive, err := ownerArchivePath(filepath.Dir(source), "registration", options.RecoveryToken)
	if err == nil {
		var scratch string
		scratch, err = ownerRecoveryName("registration-scratch", options.RecoveryToken)
		if err == nil {
			err = validateBoundedOwnerEntries(
				filepath.Dir(source), filepath.Base(archive), scratch,
			)
		}
	}
	return registrationArchive{registration, source, archive, directory}, err
}

func (archive registrationArchive) flags() int {
	flags := unix.O_RDONLY | unix.O_NONBLOCK
	if archive.directory {
		flags |= unix.O_DIRECTORY
	}
	return flags
}

func (archive registrationArchive) validateFD(fd int, prepared Prepared, retained int) error {
	registrationFD := fd
	if archive.directory {
		if err := validateRetainedDirectoryFD(fd, "owner registration archive"); err != nil {
			return err
		}
		entries, err := readDirectoryEntries(fd, "owner registration archive")
		if err != nil || len(entries) != 1 || entries[0].Name() != "config.env" {
			if err != nil {
				return err
			}
			return errors.New("owner registration archive contains unexpected state")
		}
		registrationFD, err = unix.Openat(fd, "config.env", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer unix.Close(registrationFD)
	}
	if retained >= 0 {
		same, err := sameRetainedObject(registrationFD, retained)
		if err != nil {
			return err
		}
		if !same {
			return errors.New("owner registration changed before archive")
		}
	}
	return publishedRegistrationFDMatches(registrationFD, "owner registration", prepared)
}

func (archive registrationArchive) open(path string, prepared Prepared, retained int) (*boundObject, error) {
	object, err := openBoundObject(path, archive.flags())
	if err == nil {
		err = archive.validateFD(object.fd, prepared, retained)
	}
	if err == nil {
		err = object.requireNamed()
	}
	if err != nil {
		object.close()
		return nil, err
	}
	return &object, nil
}

func validatePreparedRegistrationArchive(options Options, oldRegistration string, prepared Prepared, stage auxiliaryValidationStage) error {
	if options.RecoveryToken == "" {
		if stage != auxiliaryAtSource {
			return errors.New("owner recovery token is unavailable for registration archive")
		}
		return registrationMatches(oldRegistration, prepared)
	}
	archive, err := preparedRegistrationArchive(options, prepared)
	if err != nil {
		return err
	}
	if archive.registration != oldRegistration {
		return errors.New("owner registration archive path changed")
	}
	sourceExists, err := pathExists(archive.source)
	if err != nil {
		return err
	}
	archiveExists, err := pathExists(archive.archive)
	if err != nil {
		return err
	}
	if sourceExists {
		err = registrationMatches(oldRegistration, prepared)
	}
	if err != nil {
		return err
	}
	if !archive.directory {
		legacyDirectory := filepath.Join(filepath.Dir(archive.source), LegacyYard)
		if err := validateFlatLegacyDirectory(legacyDirectory, prepared, stage); err != nil {
			return err
		}
	}
	if options.TerminalCleanup && stage != auxiliaryAtSource && !sourceExists {
		return nil
	}
	if archiveExists {
		var object *boundObject
		object, err = archive.open(archive.archive, prepared, -1)
		if object != nil {
			object.close()
		}
	}
	if err != nil {
		return err
	}
	valid := stage == auxiliaryAtSource && sourceExists && !archiveExists ||
		stage == auxiliaryInProgress && sourceExists != archiveExists ||
		stage == auxiliaryDesired && !sourceExists && archiveExists
	if valid {
		return nil
	}
	return errors.New("owner registration archive is not an authorized recovery fact")
}

func validateFlatLegacyDirectory(
	path string,
	prepared Prepared,
	stage auxiliaryValidationStage,
) error {
	exists, err := pathExists(path)
	if err != nil || !exists {
		return err
	}
	if prepared.State != StateLegacyFlat || stage == auxiliaryDesired {
		return errors.New("flat owner migration retained an unexpected legacy directory")
	}
	recognized, err := validateSourceLinkedProjectState(path)
	if err != nil {
		return err
	}
	if recognized {
		return nil
	}
	if stage == auxiliaryInProgress {
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return readErr
		}
		if len(entries) == 0 {
			return nil
		}
	}
	return errors.New("flat owner migration retained an unexpected legacy directory")
}

func archivePreparedRegistration(options Options, registration *boundRegistration, prepared Prepared) error {
	archive, err := preparedRegistrationArchive(options, prepared)
	if err != nil {
		return err
	}
	source, err := archive.open(archive.source, prepared, registration.fd)
	if errors.Is(err, unix.ENOENT) {
		return repairPreparedRegistrationArchive(options, prepared)
	}
	if err != nil {
		return err
	}
	defer source.close()
	return renameBoundObject(options, source, source.parentFD, filepath.Base(archive.archive), archive.flags(),
		func(fd int) error { return archive.validateFD(fd, prepared, registration.fd) },
		"after-legacy-registration-validation-before-archive", "registration-archive", "after-legacy-registration-archive")
}

func repairPreparedRegistrationArchive(options Options, prepared Prepared) error {
	archive, err := preparedRegistrationArchive(options, prepared)
	if err != nil {
		return err
	}
	if exists, err := pathExists(archive.source); err != nil || exists {
		if err != nil {
			return err
		}
		return errors.New("legacy registration source remains beside its archive")
	}
	object, err := archive.open(archive.archive, prepared, -1)
	if err != nil {
		return err
	}
	defer object.close()
	return persistOwnerMutation(options, "registration-archive-destination", "after-legacy-registration-archive", object.parentFD)
}

func restorePreparedRegistration(options Options, prepared Prepared) error {
	archive, err := preparedRegistrationArchive(options, prepared)
	if err != nil {
		return err
	}
	if exists, err := pathExists(archive.source); err != nil {
		return err
	} else if exists {
		return validatePreparedRegistrationArchive(options, archive.registration, prepared, auxiliaryAtSource)
	}
	object, err := archive.open(archive.archive, prepared, -1)
	if err != nil {
		return err
	}
	defer object.close()
	return renameBoundObject(options, object, object.parentFD, filepath.Base(archive.source), archive.flags(),
		func(fd int) error { return archive.validateFD(fd, prepared, -1) },
		"after-compensation-registration-validation-before-restore", "registration-restore", "after-compensation-registration-recreation")
}

func cleanupPreparedOwnerArchives(options Options, prepared Prepared) error {
	archive, err := preparedRegistrationArchive(options, prepared)
	if err != nil {
		return err
	}
	if err := cleanupOwnerArchive(options, archive.archive, "registration-archive"); err != nil {
		return err
	}
	controller, err := ownerArchivePath(filepath.Join(options.DataHome, "e2e", "controllers"), "controller", options.RecoveryToken)
	if err != nil {
		return err
	}
	return cleanupOwnerArchive(options, controller, "controller-archive")
}

func syncRegistrationFile(options Options, file *os.File) error {
	if options.syncRegistrationFile != nil {
		return options.syncRegistrationFile(file)
	}
	return file.Sync()
}

func syncOwnerDirectory(options Options, point string, fd int) error {
	if options.syncOwnerDirectory != nil {
		return options.syncOwnerDirectory(point, fd)
	}
	return unix.Fsync(fd)
}

func persistOwnerMutation(options Options, role, point string, fd int) error {
	if err := syncOwnerDirectory(options, role, fd); err != nil {
		return err
	}
	return inject(options, point)
}
