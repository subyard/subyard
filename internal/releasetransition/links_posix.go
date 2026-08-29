package releasetransition

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type RuntimeLinkStore struct {
	root  string
	fault func(string) error
}

type runtimeLinkObservation struct {
	currentPresent  bool
	current         ReleaseID
	previousPresent bool
	previous        ReleaseID
}

func NewRuntimeLinkStore(root string) (*RuntimeLinkStore, error) {
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return nil, invalid("runtime root must be an absolute non-root path")
	}
	return &RuntimeLinkStore{root: clean}, nil
}

func (store *RuntimeLinkStore) Observe() (ReleaseLinks, error) {
	root, err := store.openRoot()
	if err != nil {
		return ReleaseLinks{}, err
	}
	defer unix.Close(root)
	observed, err := observeRuntimeLinksAt(root)
	if err != nil {
		return ReleaseLinks{}, err
	}
	return observed.public()
}

// Activate converges one exact release pair. Forward activation stages the
// target in previous and then atomically exchanges current/previous, so the old
// runtime remains active until one activation point. Rollback is the same
// atomic exchange from the already-staged pair.
func (store *RuntimeLinkStore) Activate(pair ReleasePair) (ReleaseLinks, error) {
	if store == nil {
		return ReleaseLinks{}, errors.New("runtime link store is required")
	}
	if err := pair.Validate(); err != nil {
		return ReleaseLinks{}, err
	}
	root, err := store.openRoot()
	if err != nil {
		return ReleaseLinks{}, err
	}
	defer unix.Close(root)
	if err := requireReleaseDirectoryAt(root, pair.Target); err != nil {
		return ReleaseLinks{}, err
	}
	observed, err := observeRuntimeLinksAt(root)
	if err != nil {
		return ReleaseLinks{}, err
	}
	if observed.activated(pair) {
		return observed.public()
	}
	if pair.Previous != nil && pair.Target == *pair.Previous {
		if !observed.initial(pair) || !observed.previousPresent {
			return ReleaseLinks{}, ErrProtectedStoreStale
		}
		if err := unix.Renameat2(root, "current", root, "previous", unix.RENAME_EXCHANGE); err != nil {
			return ReleaseLinks{}, err
		}
		if err := unix.Fsync(root); err != nil {
			return ReleaseLinks{}, err
		}
		if err := store.inject("after-link-exchange"); err != nil {
			return ReleaseLinks{}, err
		}
		return store.observeAt(root, pair)
	}
	if observed.initial(pair) {
		if err := replaceRuntimeLinkAt(
			root, "previous", pair.Target, observed.previousPresent,
		); err != nil {
			return ReleaseLinks{}, err
		}
		if err := store.inject("after-staged-link"); err != nil {
			return ReleaseLinks{}, err
		}
		observed, err = observeRuntimeLinksAt(root)
		if err != nil {
			return ReleaseLinks{}, err
		}
	}
	if !observed.staged(pair) && !observed.activated(pair) {
		return ReleaseLinks{}, ErrProtectedStoreStale
	}
	if !observed.activated(pair) {
		if err := unix.Renameat2(root, "current", root, "previous", unix.RENAME_EXCHANGE); err != nil {
			return ReleaseLinks{}, err
		}
		if err := unix.Fsync(root); err != nil {
			return ReleaseLinks{}, err
		}
		if err := store.inject("after-link-exchange"); err != nil {
			return ReleaseLinks{}, err
		}
	}
	return store.observeAt(root, pair)
}

func (store *RuntimeLinkStore) observeAt(root int, pair ReleasePair) (ReleaseLinks, error) {
	observed, err := observeRuntimeLinksAt(root)
	if err != nil {
		return ReleaseLinks{}, err
	}
	if !observed.activated(pair) {
		return ReleaseLinks{}, ErrProtectedStoreStale
	}
	return observed.public()
}

func (store *RuntimeLinkStore) openRoot() (int, error) {
	if store == nil {
		return -1, errors.New("runtime link store is required")
	}
	fd, err := unix.Open(
		store.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return -1, err
	}
	if err := validateRuntimeDirectoryFD(fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func observeRuntimeLinksAt(root int) (runtimeLinkObservation, error) {
	current, currentPresent, err := readRuntimeLinkAt(root, "current")
	if err != nil {
		return runtimeLinkObservation{}, err
	}
	if !currentPresent {
		return runtimeLinkObservation{}, errors.New("runtime current link is missing")
	}
	previous, previousPresent, err := readRuntimeLinkAt(root, "previous")
	if err != nil {
		return runtimeLinkObservation{}, err
	}
	return runtimeLinkObservation{
		currentPresent: true, current: current,
		previousPresent: previousPresent, previous: previous,
	}, nil
}

func readRuntimeLinkAt(root int, name string) (ReleaseID, bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(root, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK || stat.Uid != uint32(os.Getuid()) {
		return "", false, errors.New("runtime link has unsafe type or ownership")
	}
	buffer := make([]byte, 4096)
	length, err := unix.Readlinkat(root, name, buffer)
	if err != nil {
		return "", false, err
	}
	target := string(buffer[:length])
	if !strings.HasPrefix(target, "releases/") || strings.Count(target, "/") != 1 {
		return "", false, errors.New("runtime link target is unsafe")
	}
	release := ReleaseID(strings.TrimPrefix(target, "releases/"))
	if err := validateReleaseID(release, "runtime link release"); err != nil {
		return "", false, err
	}
	if err := requireReleaseDirectoryAt(root, release); err != nil {
		return "", false, err
	}
	return release, true, nil
}

func requireReleaseDirectoryAt(root int, release ReleaseID) error {
	if err := validateReleaseID(release, "runtime release"); err != nil {
		return err
	}
	releases, err := unix.Openat(
		root, "releases", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	defer unix.Close(releases)
	if err := validateRuntimeDirectoryFD(releases); err != nil {
		return err
	}
	target, err := unix.Openat(
		releases, string(release),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	defer unix.Close(target)
	return validateRuntimeDirectoryFD(target)
}

func replaceRuntimeLinkAt(root int, name string, release ReleaseID, replace bool) error {
	if err := requireReleaseDirectoryAt(root, release); err != nil {
		return err
	}
	pending := "." + name + ".release-transition.pending"
	target := "releases/" + string(release)
	if err := unix.Symlinkat(target, root, pending); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
		buffer := make([]byte, 4096)
		length, readErr := unix.Readlinkat(root, pending, buffer)
		if readErr != nil || string(buffer[:length]) != target {
			return ErrProtectedStoreStale
		}
	}
	if replace {
		if err := unix.Renameat(root, pending, root, name); err != nil {
			return err
		}
	} else if err := unix.Renameat2(root, pending, root, name, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return unix.Fsync(root)
}

func validateRuntimeDirectoryFD(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o022 != 0 ||
		stat.Uid != uint32(os.Getuid()) {
		return errors.New("runtime directory has unsafe type, mode, or ownership")
	}
	return nil
}

func (observation runtimeLinkObservation) public() (ReleaseLinks, error) {
	links := ReleaseLinks{Active: observation.current}
	if observation.previousPresent {
		links.Previous = releaseIDPointer(observation.previous)
	}
	return links, links.Validate()
}

func (observation runtimeLinkObservation) initial(pair ReleasePair) bool {
	return observation.currentPresent && observation.current == pair.From &&
		optionalReleaseEqual(observation.previousPresent, observation.previous, pair.Previous)
}

func (observation runtimeLinkObservation) staged(pair ReleasePair) bool {
	return observation.currentPresent && observation.current == pair.From &&
		observation.previousPresent && observation.previous == pair.Target
}

func (observation runtimeLinkObservation) activated(pair ReleasePair) bool {
	return observation.currentPresent && observation.current == pair.Target &&
		observation.previousPresent && observation.previous == pair.From
}

func optionalReleaseEqual(present bool, actual ReleaseID, expected *ReleaseID) bool {
	if expected == nil {
		return !present
	}
	return present && actual == *expected
}

func (store *RuntimeLinkStore) inject(point string) error {
	if store.fault != nil {
		return store.fault(point)
	}
	return nil
}
