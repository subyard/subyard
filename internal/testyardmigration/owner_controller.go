package testyardmigration

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

var controllerArtifactNames = [...]string{
	"agent-access.pub",
	"known_hosts",
	"route.tsv",
	".operator-enrollment-v1",
}

type boundController struct {
	boundObject
	digest string
}

func openBoundController(options Options, path string) (*boundController, error) {
	object, err := openBoundObject(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	controller := &boundController{boundObject: object}
	if err := inject(options, "after-controller-root-open-for-digest"); err != nil {
		controller.close()
		return nil, err
	}
	controller.digest, err = inspectControllerFD(controller.fd)
	if err == nil {
		err = controller.requireNamed()
	}
	if err != nil {
		controller.close()
		return nil, fmt.Errorf("legacy controller state changed while inspected: %w", err)
	}
	return controller, nil
}

func controllerStateDigest(options Options, path string) (string, error) {
	controller, err := openBoundController(options, path)
	if errors.Is(err, unix.ENOENT) {
		return absentStateDigest(), nil
	}
	if err != nil {
		return "", err
	}
	defer controller.close()
	return controller.digest, nil
}

func inspectControllerFD(rootFD int) (string, error) {
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return "", err
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Mode&0o077 != 0 ||
		rootStat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("legacy controller path is not an owned private directory")
	}
	entries, err := readDirectoryEntries(rootFD, "controller")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !slices.Contains(controllerArtifactNames[:], entry.Name()) {
			return "", fmt.Errorf("unexpected legacy controller artifact %q", entry.Name())
		}
	}
	artifacts := make([]string, len(controllerArtifactNames))
	present, marker := false, false
	for index, name := range controllerArtifactNames {
		artifacts[index] = absentStateDigest()
		artifactFD, err := unix.Openat(
			rootFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
		)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return "", err
		}
		artifacts[index], err = controllerArtifactDigest(artifactFD, name)
		if err == nil {
			err = requireBoundName(rootFD, name, artifactFD)
		}
		_ = unix.Close(artifactFD)
		if err != nil {
			return "", err
		}
		present = true
		marker = marker || name == ".operator-enrollment-v1"
	}
	if present && !marker {
		return "", errors.New("legacy controller marker is missing")
	}
	return digestStrings(
		"subyard-owner-controller-tree-v2", fmt.Sprintf("%o", rootStat.Mode),
		artifacts[0], artifacts[1], artifacts[2], artifacts[3],
	), nil
}

func controllerArtifactDigest(fileFD int, name string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 ||
		stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("legacy controller artifact %q is unsafe", name)
	}
	payload, err := readRetainedFile(fileFD, name, stat.Size)
	if err != nil {
		return "", err
	}
	if name == ".operator-enrollment-v1" &&
		(string(payload) != "managed\n" || stat.Mode&0o7777 != 0o600) {
		return "", errors.New("legacy controller marker is invalid")
	}
	content := sha256.Sum256(payload)
	return digestStrings(
		"subyard-owner-controller-artifact-v1", name,
		fmt.Sprintf("%o", stat.Mode), fmt.Sprintf("%x", content[:]),
	), nil
}

func validatePreparedControllerState(
	options Options,
	prepared Prepared,
	stage auxiliaryValidationStage,
) error {
	source := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	if options.RecoveryToken == "" {
		if stage != auxiliaryAtSource {
			return errors.New("owner recovery token is unavailable for controller archive")
		}
		if err := validateBoundedOwnerEntries(filepath.Dir(source)); err != nil {
			return err
		}
		digest, err := controllerStateDigest(options, source)
		if err != nil {
			return err
		}
		if digest != prepared.ControllerDigest {
			return errors.New("legacy controller state changed outside the authorized transition")
		}
		return nil
	}
	archive, err := ownerArchivePath(filepath.Dir(source), "controller", options.RecoveryToken)
	if err != nil {
		return err
	}
	if err := validateBoundedOwnerEntries(filepath.Dir(source), filepath.Base(archive)); err != nil {
		return err
	}
	sourceDigest, err := controllerStateDigest(options, source)
	if err != nil {
		return err
	}
	want, absent := prepared.ControllerDigest, absentStateDigest()
	if options.TerminalCleanup &&
		(stage == auxiliaryInProgress || stage == auxiliaryDesired) && sourceDigest == absent {
		if want == absent {
			if exists, existsErr := pathExists(archive); existsErr != nil {
				return existsErr
			} else if exists {
				return errors.New("unexpected owner controller cleanup archive")
			}
			return nil
		}
		return nil
	}
	archiveDigest, err := controllerStateDigest(options, archive)
	if err != nil {
		return err
	}
	if want == absent {
		if sourceDigest != absent || archiveDigest != absent {
			return errors.New("unexpected legacy controller state")
		}
		return nil
	}
	valid := stage == auxiliaryAtSource && sourceDigest == want && archiveDigest == absent ||
		stage == auxiliaryInProgress && ((sourceDigest == want && archiveDigest == absent) ||
			(sourceDigest == absent && archiveDigest == want)) ||
		stage == auxiliaryDesired && sourceDigest == absent && archiveDigest == want
	if valid {
		return nil
	}
	switch stage {
	case auxiliaryAtSource:
		return errors.New("legacy controller state changed outside the authorized transition")
	case auxiliaryInProgress:
		return errors.New("legacy controller archive is not an authorized recovery fact")
	case auxiliaryDesired:
		return errors.New("legacy controller state did not converge to its archive")
	default:
		return errors.New("unknown controller validation stage")
	}
}

func archivePreparedController(options Options, path string, prepared Prepared) error {
	if prepared.ControllerDigest == absentStateDigest() {
		return validatePreparedControllerState(options, prepared, auxiliaryInProgress)
	}
	archive, err := ownerArchivePath(filepath.Dir(path), "controller", options.RecoveryToken)
	if err != nil {
		return err
	}
	controller, err := openBoundController(options, path)
	if errors.Is(err, unix.ENOENT) {
		digest, digestErr := controllerStateDigest(options, archive)
		if digestErr != nil {
			return digestErr
		}
		if digest != prepared.ControllerDigest {
			return errors.New("legacy controller archive changed")
		}
		parentFD, _, openErr := openParent(archive)
		if openErr != nil {
			return openErr
		}
		defer unix.Close(parentFD)
		return persistOwnerMutation(
			options, "controller-archive-destination", "after-legacy-controller-archive", parentFD,
		)
	}
	if err != nil {
		return err
	}
	defer controller.close()
	if controller.digest != prepared.ControllerDigest {
		return errors.New("legacy controller state changed before archive")
	}
	return renameBoundObject(
		options, &controller.boundObject, controller.parentFD, filepath.Base(archive),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK,
		func(fd int) error {
			digest, err := inspectControllerFD(fd)
			if err != nil {
				return err
			}
			if digest != prepared.ControllerDigest {
				return errors.New("legacy controller changed during archive")
			}
			return nil
		},
		"after-legacy-controller-validation-before-archive",
		"controller-archive", "after-legacy-controller-archive",
	)
}
