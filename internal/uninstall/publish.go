package uninstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// writePending writes or resumes a private sidecar below its pinned parent.
func writePending(parent int, name string, body []byte, mode, uid uint32) (Entry, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err == nil && os.Getuid() == 0 {
		if err = unix.Fchown(fd, int(uid), -1); err != nil {
			unix.Close(fd)
		}
	} else if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(parent, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return Entry{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return Entry{}, err
	}
	// Never read a sidecar before its type and owner are proven: a FIFO would block.
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 && info.Mode&07777 != mode || info.Uid != uid {
		return Entry{}, guardRefusal("unsafe interrupted file publication")
	}
	// One byte past the body proves a longer sidecar is not its prefix.
	existing, err := io.ReadAll(io.LimitReader(file, int64(len(body))+1))
	if err != nil {
		return Entry{}, err
	}
	if !bytes.HasPrefix(body, existing) {
		return Entry{}, guardRefusal("unsafe interrupted file publication")
	}
	if _, err := file.WriteAt(body, 0); err != nil {
		return Entry{}, err
	}
	if err := file.Chmod(os.FileMode(mode)); err != nil {
		return Entry{}, err
	}
	if err := file.Sync(); err != nil {
		return Entry{}, err
	}
	current, err := statAt(parent, name, ".")
	if err == nil && (info.Dev != current.Device || info.Ino != current.Inode) {
		err = guardRefusal("pending file changed during publication")
	}
	return current, err
}

func publishAt(parent int, pending, name string) error {
	if err := unix.Renameat(parent, pending, parent, name); err != nil {
		return err
	}
	return unix.Fsync(parent)
}

func publishPlan(path string, uid uint32, body []byte, expected *Tree) error {
	parent, name, err := pinParent(path)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	var directory unix.Stat_t
	if err := unix.Fstat(parent, &directory); err != nil {
		return err
	}
	if directory.Mode&unix.S_IFMT != unix.S_IFDIR || directory.Mode&07777 != 0700 || directory.Uid != uid || uint32(os.Getuid()) != uid {
		return guardRefusal("unsafe recovery publication parent")
	}
	unchanged := func() error {
		if expected != nil {
			if expected.Path != path {
				return guardRefusal("different recovery publication path")
			}
			return expected.check()
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return guardRefusal("recovery plan already exists")
	}
	if err := unchanged(); err != nil {
		return err
	}
	if _, err := writePending(parent, ".plan-next", body, 0600, uid); err != nil {
		return err
	}
	if err := unchanged(); err != nil {
		return err
	}
	return publishAt(parent, ".plan-next", name)
}

func advanceEpoch(path string, before, after uint64, uid uint32) error {
	if uint32(os.Getuid()) != uid || after != before+1 {
		return guardRefusal("invalid owner epoch advancement")
	}
	parent, name, err := pinParent(path)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	var directory unix.Stat_t
	if err := unix.Fstat(parent, &directory); err != nil {
		return err
	}
	if directory.Uid != uid || directory.Mode&07777 != 0700 {
		return guardRefusal("unsafe owner epoch directory")
	}
	current := uint64(0)
	if fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err == nil {
		file := os.NewFile(uintptr(fd), name)
		var info unix.Stat_t
		body := make([]byte, 33)
		err = unix.Fstat(fd, &info)
		size, readErr := file.Read(body)
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		file.Close()
		if err = errors.Join(err, readErr); err != nil {
			return err
		}
		if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 || info.Uid != uid {
			return guardRefusal("unsafe owner epoch")
		}
		current, err = strconv.ParseUint(strings.TrimSuffix(string(body[:size]), "\n"), 10, 64)
		if err != nil || string(body[:size]) != strconv.FormatUint(current, 10)+"\n" {
			return guardRefusal("invalid owner epoch")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if current == after {
		return nil
	}
	if current != before {
		return guardRefusal("owner epoch changed outside approved cleanup")
	}
	if _, err := writePending(parent, name+"-next", []byte(strconv.FormatUint(after, 10)+"\n"), 0600, uid); err != nil {
		return err
	}
	return publishAt(parent, name+"-next", name)
}

// readPinned reads a regular file only while its fstat facts stay approved.
func readPinned(parent int, name string, approved Entry, limit int64, changed guardRefusal) ([]byte, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if statFact(&stat, ".") != approved {
		return nil, changed
	}
	return io.ReadAll(io.LimitReader(file, limit+1))
}

var sidecarName = regexp.MustCompile(`^\.subyard-uninstall-[a-f0-9]{24}$`)

func (edit StartupEdit) prepare() (StartupEdit, error) {
	before := edit.Before
	err := withPinned(before.Path, func(parent int, name string) error {
		current, err := before.capture()
		if err == nil {
			err = checkEntries(before.Entries, current.Entries)
		}
		if err != nil {
			return err
		}
		if len(current.Entries) != 1 {
			return guardRefusal("managed shell file is missing")
		}
		content, err := readPinned(parent, name, before.Entries[0], 1<<20, "managed shell file changed before reading")
		if err != nil {
			return err
		}
		if len(content) > 1<<20 {
			return guardRefusal("managed shell file is oversized")
		}
		for _, block := range edit.Blocks {
			if bytes.Count(content, []byte(block)) != 1 {
				return guardRefusal("managed shell block changed")
			}
			content = bytes.Replace(content, []byte(block), nil, 1)
		}
		if err := before.check(); err != nil {
			return err
		}
		planned := edit.Temporary
		if planned == "" || filepath.Dir(planned) != filepath.Dir(before.Path) || !sidecarName.MatchString(filepath.Base(planned)) {
			return guardRefusal("original shell sidecar intent is missing")
		}
		temporary := filepath.Base(planned)
		fact, err := writePending(parent, temporary, content, before.Entries[0].Mode&0777, uint32(os.Getuid()))
		if err != nil {
			return err
		}
		if err := before.check(); err != nil {
			var refusal guardRefusal
			if errors.As(err, &refusal) {
				if cleanup := removeEntries(parent, temporary, []Entry{fact}, false, nil, false, nil); cleanup != nil {
					return cleanup
				}
			}
			return err
		}
		edit.Pending = &Tree{Path: filepath.Join(filepath.Dir(before.Path), temporary), Entries: []Entry{fact}}
		return nil
	})
	return edit, err
}

func (edit StartupEdit) commit() (Tree, error) {
	before, pending := edit.Before, edit.Pending
	if pending == nil || pending.Path != edit.Temporary || len(pending.Entries) != 1 || filepath.Dir(pending.Path) != filepath.Dir(before.Path) {
		return Tree{}, guardRefusal("invalid prepared shell edit")
	}
	updated := Tree{Path: before.Path}
	err := withPinned(before.Path, func(parent int, name string) error {
		temporary := filepath.Base(pending.Path)
		current, err := statAt(parent, temporary, ".")
		if errors.Is(err, os.ErrNotExist) {
			original := pending.Entries[0]
			destination, err := statAt(parent, name, ".")
			if err != nil {
				return err
			}
			original.Changed, destination.Changed = 0, 0
			if original != destination {
				return guardRefusal("interrupted shell destination changed")
			}
		} else if err != nil {
			return err
		} else {
			if current != pending.Entries[0] {
				return guardRefusal("prepared shell edit changed")
			}
			if err := before.check(); err != nil {
				return err
			}
			if err := publishAt(parent, temporary, name); err != nil {
				return err
			}
		}
		fact, err := statAt(parent, name, ".")
		updated.Entries = []Entry{fact}
		return err
	})
	return updated, err
}

// withPinned runs work in the pinned parent of an existing approved path.
func withPinned(path string, work func(int, string) error) error {
	parent, name, err := pinParent(path)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return work(parent, name)
}

// CaptureAssets uses teardown's metadata guard and the installed manifest or
// existing public source selector. Operator overlays never enter recovery.
func CaptureAssets(root string) ([]Tree, error) {
	assets, err := captureAssets(root)
	if err != nil {
		return nil, classify("capture-assets", err)
	}
	return assets, nil
}

func captureAssets(root string) ([]Tree, error) {
	digests := map[string]string{}
	var paths []string
	body, err := readBounded(filepath.Join(root, "runtime-files.sha256"), 4<<20)
	if err == nil {
		if len(body) > 4<<20 || slices.ContainsFunc(body, func(value byte) bool { return value >= 0x80 }) {
			return nil, guardRefusal("oversized installed asset manifest")
		}
		for _, line := range splitLines(string(body)) {
			digest, relative, found := strings.Cut(line, "  ")
			if !found {
				return nil, guardRefusal("invalid installed asset line")
			}
			if !hexDigest(digest) {
				return nil, guardRefusal("invalid installed asset digest")
			}
			relative = strings.TrimPrefix(relative, "./")
			if _, duplicate := digests[relative]; duplicate {
				return nil, guardRefusal("duplicate installed asset path")
			}
			digests[relative] = digest
			paths = append(paths, relative)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		command := exec.Command("rg", "--no-config", "--files", "--hidden", "--null", "--no-require-git", "--no-ignore-dot", "--no-ignore-global", "--no-ignore-parent", "--no-ignore-exclude", "-g", "!.git")
		command.Dir = root
		body, err = command.Output()
		if err != nil {
			return nil, err
		}
		if len(body) > 4<<20 || !utf8.Valid(body) {
			return nil, guardRefusal("oversized public source selector")
		}
		for _, path := range strings.Split(string(body), "\x00") {
			if path != "" {
				paths = append(paths, path)
			}
		}
	} else {
		return nil, err
	}
	assets := []Tree{}
	seen := map[string]bool{}
	for _, relative := range paths {
		// Keep the shared Python assets shipped by package-engine.sh; retained
		// handlers use it after the original runtime has been removed.
		if !strings.HasPrefix(relative, "scripts/") && !strings.HasPrefix(relative, "config/") && !strings.HasPrefix(relative, "completions/") && !slices.Contains([]string{"bin/yard", "internal/adapters/configmaterial/tomli_w.py", "internal/adapters/configmaterial/TOMLI_W_LICENSE", "internal/safefs/safefs.py"}, relative) {
			continue
		}
		if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsAny(relative, "\r\n\t\x00") || seen[relative] || len(assets) >= 4096 {
			return nil, guardRefusal("invalid public recovery asset")
		}
		seen[relative] = true
		tree, err := Tree{Path: filepath.Join(root, relative), Entries: []Entry{}}.capture()
		if err != nil {
			return nil, err
		}
		if len(tree.Entries) != 1 || tree.Entries[0].Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, guardRefusal("public recovery asset is not one regular file")
		}
		if expected, ok := digests[relative]; ok {
			content, err := os.ReadFile(tree.Path)
			if err != nil {
				return nil, err
			}
			if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != expected {
				return nil, guardRefusal("public recovery asset differs from installed manifest")
			}
			if err := tree.check(); err != nil {
				return nil, err
			}
		}
		assets = append(assets, tree)
	}
	for _, required := range []string{"bin/yard", "config/commands.registry", "scripts/teardown-physical.sh", "scripts/lib/teardown-services.sh"} {
		if !seen[required] {
			return nil, guardRefusal("required public recovery assets are missing")
		}
	}
	return assets, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit+1))
}

// splitLines keeps the manifest's historical str.splitlines boundaries.
func splitLines(text string) []string {
	lines := []string{}
	for text != "" {
		index := strings.IndexAny(text, "\n\r\v\f\x1c\x1d\x1e")
		if index < 0 {
			return append(lines, text)
		}
		lines = append(lines, text[:index])
		if strings.HasPrefix(text[index:], "\r\n") {
			index++
		}
		text = text[index+1:]
	}
	return lines
}

// PublishRuntime copies exact public assets with teardown's pinned
// filesystem guard; the retained runtime survives all installation deletions.
func (store RecoveryStore) PublishRuntime(plan RecoveryPlan) error {
	if err := store.validateDirectory(); err != nil {
		return err
	}
	return classify("retain-runtime", retainRuntime(plan, filepath.Join(store.Directory, "runtime")))
}

func retainRuntime(plan RecoveryPlan, destination string) error {
	uid := plan.Installation.OperatorUID
	if uint32(os.Getuid()) != uid {
		return guardRefusal("retained runtime must be copied by its owner")
	}
	directory := func(path string) error {
		return withPinned(path, func(parent int, name string) error {
			if err := unix.Mkdirat(parent, name, 0700); err == nil {
				fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if err != nil {
					return err
				}
				err = unix.Fchmod(fd, 0700)
				unix.Close(fd)
				if err != nil {
					return err
				}
			} else if !errors.Is(err, unix.EEXIST) {
				return err
			}
			info, err := statAt(parent, name, ".")
			if err == nil && (info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Mode&07777 != 0700 || info.UID != uid) {
				err = guardRefusal("unsafe retained runtime directory")
			}
			return err
		})
	}
	if err := directory(destination); err != nil {
		return err
	}
	for _, tree := range plan.SourceAssets {
		if len(tree.Entries) != 1 || tree.Entries[0].Mode&unix.S_IFMT != unix.S_IFREG {
			return guardRefusal("retained public asset must be one approved file")
		}
		relative := tree.Category
		if relative == "" {
			var err error
			if relative, err = filepath.Rel(plan.SourceRoot, tree.Path); err != nil {
				return err
			}
		}
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") || filepath.Clean(relative) != relative {
			return guardRefusal("retained public asset escapes its runtime")
		}
		parents := strings.Split(relative, "/")
		for depth := 1; depth < len(parents); depth++ {
			if err := directory(filepath.Join(append([]string{destination}, parents[:depth]...)...)); err != nil {
				return err
			}
		}
		approved := tree.Entries[0]
		var body []byte
		err := withPinned(tree.Path, func(parent int, name string) error {
			fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			file := os.NewFile(uintptr(fd), name)
			defer file.Close()
			var stat unix.Stat_t
			if err := unix.Fstat(fd, &stat); err != nil {
				return err
			}
			if statFact(&stat, ".") != approved {
				return guardRefusal("retained public source changed before reading")
			}
			if body, err = io.ReadAll(io.LimitReader(file, approved.Size+1)); err != nil {
				return err
			}
			if err := unix.Fstat(fd, &stat); err != nil {
				return err
			}
			if int64(len(body)) != approved.Size || statFact(&stat, ".") != approved {
				return guardRefusal("retained public source changed while reading")
			}
			return nil
		})
		if err != nil {
			return err
		}
		mode := uint32(0600)
		if approved.Mode&0111 != 0 {
			mode = 0700
		}
		err = withPinned(filepath.Join(destination, relative), func(parent int, name string) error {
			fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err == nil {
				file := os.NewFile(uintptr(fd), name)
				defer file.Close()
				var stat unix.Stat_t
				if err := unix.Fstat(fd, &stat); err != nil {
					return err
				}
				if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07777 != mode || stat.Uid != uid {
					return guardRefusal("unsafe retained runtime asset")
				}
				existing, err := io.ReadAll(io.LimitReader(file, int64(len(body))+1))
				if err != nil {
					return err
				}
				if !bytes.Equal(existing, body) {
					return guardRefusal("retained runtime bytes differ from approved source")
				}
				return nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			pending := name + ".copy-next"
			if _, err := writePending(parent, pending, body, mode, uid); err != nil {
				return err
			}
			var stat unix.Stat_t
			if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, os.ErrNotExist) {
				return errors.Join(err, guardRefusal("retained runtime destination appeared during publication"))
			}
			return publishAt(parent, pending, name)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func privateDirectory(path string) error {
	err := os.Mkdir(path, 0700)
	if err == nil {
		return os.Chmod(path, 0700)
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
		return errors.New("unsafe recovery runtime directory")
	}
	return nil
}
