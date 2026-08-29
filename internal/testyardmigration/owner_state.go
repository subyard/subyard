package testyardmigration

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

type boundObject struct {
	parentFD int
	fd       int
	name     string
}

func openBoundObject(path string, flags int) (boundObject, error) {
	parentFD, name, err := openParent(path)
	if err != nil {
		return boundObject{}, err
	}
	fd, err := unix.Openat(parentFD, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = unix.Close(parentFD)
	}
	return boundObject{parentFD: parentFD, fd: fd, name: name}, err
}

func (object *boundObject) close() {
	_ = unix.Close(object.fd)
	_ = unix.Close(object.parentFD)
}

func (object *boundObject) requireNamed() error {
	return requireBoundName(object.parentFD, object.name, object.fd)
}

type boundOverrides struct {
	boundObject
	digest string
}

func openBoundOverrides(options Options, path string) (*boundOverrides, error) {
	object, err := openBoundObject(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	state := &boundOverrides{boundObject: object}
	if err := inject(options, "after-overrides-root-open-for-digest"); err != nil {
		state.close()
		return nil, err
	}
	state.digest, err = digestOverridesFD(state.fd)
	if err == nil {
		err = state.requireNamed()
	}
	if err != nil {
		state.close()
		return nil, fmt.Errorf("owner overrides state changed while inspected: %w", err)
	}
	return state, nil
}

func overridesStateDigestWithOptions(options Options, path string) (string, error) {
	state, err := openBoundOverrides(options, path)
	if errors.Is(err, unix.ENOENT) {
		return absentStateDigest(), nil
	}
	if err != nil {
		return "", err
	}
	defer state.close()
	return state.digest, nil
}

func digestOverridesFD(rootFD int) (string, error) {
	digest := sha256.New()
	_, _ = io.WriteString(digest, "subyard-owner-state:tree-v2")
	if err := appendOverridesDigest(digest, rootFD, "."); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func appendOverridesDigest(digest hash.Hash, directoryFD int, relative string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(directoryFD, &stat); err != nil {
		return err
	}
	if err := validateOverrideStat(relative, stat); err != nil {
		return err
	}
	_, _ = io.WriteString(digest, fmt.Sprintf("\x00%s\x00%o", relative, stat.Mode))
	if stat.Mode&unix.S_IFMT == unix.S_IFREG {
		payload, err := readRetainedFile(directoryFD, relative, stat.Size)
		if err != nil {
			return err
		}
		content := sha256.Sum256(payload)
		_, _ = digest.Write(content[:])
		return nil
	}
	children, err := readDirectoryEntries(directoryFD, relative)
	if err != nil {
		return err
	}
	slices.SortFunc(children, func(first, second os.DirEntry) int {
		return strings.Compare(first.Name(), second.Name())
	})
	for _, child := range children {
		childFD, err := unix.Openat(directoryFD, child.Name(),
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		childPath := child.Name()
		if relative != "." {
			childPath = relative + "/" + child.Name()
		}
		err = appendOverridesDigest(digest, childFD, childPath)
		_ = unix.Close(childFD)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateOverrideStat(name string, stat unix.Stat_t) error {
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%q is not owned real state", name)
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if stat.Mode&0o022 != 0 {
			return fmt.Errorf("%q is a writable shared directory", name)
		}
	case unix.S_IFREG:
		if stat.Nlink != 1 || stat.Mode&0o7177 != 0 {
			return fmt.Errorf("%q is not safe private override state", name)
		}
	default:
		return fmt.Errorf("%q is not a safe regular state file", name)
	}
	return nil
}

type boundRegistration struct {
	boundObject
	publishedMode bool
}

func openBoundRegistration(path string, prepared Prepared) (*boundRegistration, error) {
	return openBoundRegistrationWithMode(path, prepared, false)
}

func openBoundPublishedRegistration(path string, prepared Prepared) (*boundRegistration, error) {
	return openBoundRegistrationWithMode(path, prepared, true)
}

func openBoundRegistrationWithMode(
	path string,
	prepared Prepared,
	publishedMode bool,
) (*boundRegistration, error) {
	object, err := openBoundObject(path, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	state := &boundRegistration{boundObject: object, publishedMode: publishedMode}
	if err := state.matches(prepared); err != nil {
		state.close()
		return nil, err
	}
	if err := state.requireNamed(); err != nil {
		state.close()
		return nil, fmt.Errorf("owner registration changed while inspected: %w", err)
	}
	return state, nil
}

func (state *boundRegistration) matches(prepared Prepared) error {
	if state.publishedMode {
		return publishedRegistrationFDMatches(state.fd, state.name, prepared)
	}
	return registrationFDMatches(state.fd, state.name, prepared)
}

func registrationFDMatches(fd int, name string, prepared Prepared) error {
	return registrationFDMatchesMode(fd, name, prepared, false)
}

func publishedRegistrationFDMatches(fd int, name string, prepared Prepared) error {
	return registrationFDMatchesMode(fd, name, prepared, true)
}

func registrationFDMatchesMode(fd int, name string, prepared Prepared, publishedMode bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	modeMatches := stat.Mode&0o7777 == 0o600
	requiredMode := "mode-0600"
	if publishedMode {
		modeMatches = safePublishedRegistrationMode(stat.Mode)
		requiredMode = "mode-0600/0640/0644"
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || !modeMatches || stat.Nlink != 1 ||
		stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("test-yard registration is not an owned %s regular file", requiredMode)
	}
	payload, err := readRetainedFile(fd, name, stat.Size)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	if fmt.Sprintf("%x", digest[:]) != prepared.RegistrationDigest {
		return errors.New("owner registration bytes changed outside the authorized transition")
	}
	return nil
}

func (state *boundRegistration) payload(prepared Prepared) ([]byte, error) {
	if err := state.matches(prepared); err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(state.fd, &stat); err != nil {
		return nil, err
	}
	return readRetainedFile(state.fd, state.name, stat.Size)
}

func parkBoundRegistration(
	options Options,
	state *boundRegistration,
	current string,
	prepared Prepared,
) error {
	scratch, err := ownerRecoveryName("registration-scratch", options.RecoveryToken)
	if err != nil {
		return err
	}
	destinationParent, destinationName, err := openParent(filepath.Join(
		registrationStagingParent(current), scratch,
	))
	if err != nil {
		return err
	}
	defer unix.Close(destinationParent)
	return renameBoundObject(
		options, &state.boundObject, destinationParent, destinationName,
		unix.O_RDONLY|unix.O_NONBLOCK,
		func(fd int) error {
			return registrationFDMatches(fd, "compensated owner registration", prepared)
		},
		"after-compensation-current-registration-validation-before-removal",
		"current-registration-removal", "after-compensation-current-registration-removal",
	)
}

func moveBoundOverrides(
	options Options,
	state *boundOverrides,
	destination string,
	wantDigest string,
	casPoint string,
) error {
	destinationParent, destinationName, err := openParent(destination)
	if err != nil {
		return err
	}
	defer unix.Close(destinationParent)
	if err := inject(options, casPoint); err != nil {
		return err
	}
	digest, err := digestOverridesFD(state.fd)
	if err != nil {
		return err
	}
	if digest != wantDigest {
		return errors.New("owner overrides state changed before move")
	}
	if err := state.requireNamed(); err != nil {
		return fmt.Errorf("owner overrides state changed before move: %w", err)
	}
	return renameBoundObject(
		options, &state.boundObject, destinationParent, destinationName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK,
		func(fd int) error {
			digest, err := digestOverridesFD(fd)
			if err != nil {
				return err
			}
			if digest != wantDigest {
				return errors.New("owner overrides state changed during move")
			}
			return nil
		},
		"after-auxiliary-state-validation-before-rename", "override-move", "",
	)
}

func repairMovedOwnerState(options Options, source, destination, role string) error {
	destinationParent, _, err := openParent(destination)
	if err != nil {
		return err
	}
	defer unix.Close(destinationParent)
	if err := syncOwnerDirectory(options, role+"-destination", destinationParent); err != nil {
		return err
	}
	sourceParent, _, err := openParent(source)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	return syncOwnerDirectory(options, role+"-source", sourceParent)
}

func renameBoundObject(
	options Options,
	source *boundObject,
	destinationParent int,
	destinationName string,
	openFlags int,
	validate func(int) error,
	casPoint, syncRole, postPoint string,
) error {
	if err := validateRetainedDirectoryFD(destinationParent, "owner mutation destination parent"); err != nil {
		return err
	}
	if err := source.requireNamed(); err != nil {
		return err
	}
	var destinationStat unix.Stat_t
	if err := unix.Fstatat(
		destinationParent, destinationName, &destinationStat, unix.AT_SYMLINK_NOFOLLOW,
	); err == nil {
		return errors.New("owner mutation destination already exists")
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := inject(options, casPoint); err != nil {
		return err
	}
	if err := unix.Renameat2(
		source.parentFD, source.name, destinationParent, destinationName, unix.RENAME_NOREPLACE,
	); err != nil {
		return err
	}
	destinationFD, err := unix.Openat(
		destinationParent, destinationName, openFlags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err == nil {
		var same bool
		same, err = sameRetainedObject(source.fd, destinationFD)
		if err == nil && !same {
			err = errors.New("owner mutation path changed and moved a different object")
		}
		if err == nil {
			err = validate(destinationFD)
		}
		_ = unix.Close(destinationFD)
	}
	if err != nil {
		return restoreMisdirectedRename(
			options, source, destinationParent, destinationName, syncRole, err,
		)
	}
	if err := syncOwnerDirectory(options, syncRole+"-destination", destinationParent); err != nil {
		return err
	}
	sameParent, err := sameRetainedObject(source.parentFD, destinationParent)
	if err != nil {
		return err
	}
	if !sameParent {
		if err := syncOwnerDirectory(options, syncRole+"-source", source.parentFD); err != nil {
			return err
		}
	}
	if postPoint == "" {
		return nil
	}
	return inject(options, postPoint)
}

func restoreMisdirectedRename(
	options Options,
	source *boundObject,
	destinationParent int,
	destinationName, syncRole string,
	cause error,
) error {
	var sourceStat unix.Stat_t
	if err := unix.Fstatat(
		source.parentFD, source.name, &sourceStat, unix.AT_SYMLINK_NOFOLLOW,
	); err == nil {
		return fmt.Errorf("%w; source was recreated, preserving both names", cause)
	} else if !errors.Is(err, unix.ENOENT) {
		return errors.Join(cause, err)
	}
	if err := unix.Renameat2(
		destinationParent, destinationName, source.parentFD, source.name, unix.RENAME_NOREPLACE,
	); err != nil {
		return errors.Join(cause, fmt.Errorf("restore misdirected owner mutation: %w", err))
	}
	sameParent, err := sameRetainedObject(source.parentFD, destinationParent)
	if err != nil {
		return errors.Join(cause, err)
	}
	if !sameParent {
		if err := syncOwnerDirectory(options, syncRole+"-restore-destination", destinationParent); err != nil {
			return errors.Join(cause, err)
		}
	}
	if err := syncOwnerDirectory(options, syncRole+"-restore", source.parentFD); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// cleanupOwnerArchive removes warning-only recovery data after the verified fixed point.
func cleanupOwnerArchive(
	options Options,
	path string,
	role string,
) error {
	parentFD, _, err := openParent(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	if err := syncOwnerDirectory(options, role+"-parent-repair", parentFD); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return persistOwnerMutation(options, role+"-cleanup", "after-"+role+"-cleanup", parentFD)
}

func openParent(path string) (int, string, error) {
	parentFD, err := unix.Open(
		filepath.Dir(path),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err == nil {
		err = validateRetainedDirectoryFD(parentFD, "owner mutation parent")
		if err != nil {
			_ = unix.Close(parentFD)
			parentFD = -1
		}
	}
	return parentFD, filepath.Base(path), err
}

func validateRetainedDirectoryFD(fd int, label string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) ||
		stat.Mode&0o022 != 0 {
		return fmt.Errorf("%s is not an owned non-shared-writable directory", label)
	}
	return nil
}

func sameRetainedObject(first, second int) (bool, error) {
	var firstStat, secondStat unix.Stat_t
	if err := unix.Fstat(first, &firstStat); err != nil {
		return false, err
	}
	if err := unix.Fstat(second, &secondStat); err != nil {
		return false, err
	}
	return firstStat.Dev == secondStat.Dev && firstStat.Ino == secondStat.Ino, nil
}

func readDirectoryEntries(fd int, name string) ([]os.DirEntry, error) {
	duplicate, err := unix.Openat(
		fd,
		".",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), name)
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, fmt.Errorf("read retained %s", name)
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func readRetainedFile(fd int, name string, size int64) ([]byte, error) {
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), name)
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, fmt.Errorf("open retained %s", name)
	}
	payload, readErr := io.ReadAll(io.NewSectionReader(file, 0, size))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	return payload, closeErr
}

func requireBoundName(parentFD int, name string, heldFD int) error {
	var held, named unix.Stat_t
	if err := unix.Fstat(heldFD, &held); err != nil {
		return err
	}
	if err := unix.Fstatat(parentFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if held.Dev != named.Dev || held.Ino != named.Ino {
		return errors.New("path no longer names the retained object")
	}
	return nil
}

func digestStrings(domain string, values ...string) string {
	digest := sha256.New()
	_, _ = io.WriteString(digest, domain)
	for _, value := range values {
		_, _ = io.WriteString(digest, "\x00"+value)
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}
