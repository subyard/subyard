package uninstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/safefs"
	"golang.org/x/sys/unix"
)

// The exact guard pins every ancestor from "/" without following symbolic
// links, records full metadata and never deletes outside a pinned directory.
const artifactLimit = 4096

// guardRefusal is a fixed guard refusal; boundaries map it to their outcome.
type guardRefusal string

func (refusal guardRefusal) Error() string { return string(refusal) }

// statAt is the only observation point; tests inject drift through it.
var statAt = factAt

// classify preserves the outcomes used by the in-process callers.
func classify(mode string, err error) error {
	if err == nil {
		return nil
	}
	switch guardExitCode(mode, err) {
	case 77:
		return os.ErrPermission
	case ports.TeardownPlanStaleExitCode:
		return fmt.Errorf("%w: %s: %s", domain.ErrPlanStale, mode, guardDiagnostic(err))
	default:
		return fmt.Errorf("%s: %s", mode, guardDiagnostic(err))
	}
}

func guardExitCode(mode string, err error) int {
	if errors.Is(err, os.ErrPermission) {
		if mode == "capture-tree" || mode == "capture-artifact" {
			return 77
		}
		return ports.TeardownPlanStaleExitCode
	}
	switch mode {
	case "guard-artifacts", "guard-artifact", "remove-artifact", "remove-artifact-as",
		"capture-tree", "check-tree", "remove-tree", "verify-tree", "publish-plan",
		"remove-files", "remove-shared", "check-profiles":
		return ports.TeardownPlanStaleExitCode
	default:
		return 1
	}
}

// guardDiagnostic renders only trusted descriptions and structural error facts.
func guardDiagnostic(err error) string {
	if group, ok := err.(*domain.Refusals); ok {
		var messages []string
		for _, cause := range group.Unwrap() {
			messages = append(messages, guardDiagnostic(cause))
		}
		return strings.Join(messages, "; ")
	}
	var refusal guardRefusal
	var field ports.TeardownMissingField
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	var exit *exec.ExitError
	var errno syscall.Errno
	switch {
	case errors.As(err, &refusal):
		return refusal.Error()
	case errors.As(err, &field):
		return field.Error()
	case errors.As(err, &syntax):
		return fmt.Sprintf("invalid JSON at byte %d", syntax.Offset)
	case errors.As(err, &shape):
		return "unexpected JSON value type"
	case errors.As(err, &exit):
		return fmt.Sprintf("subprocess exited with status %d", exit.ExitCode())
	case errors.Is(err, exec.ErrNotFound):
		return "required executable was not found"
	case errors.As(err, &errno):
		return errno.Error()
	default:
		return "could not verify teardown evidence"
	}
}

func guardContext(context string, err error) error {
	return fmt.Errorf("%w: %w", guardRefusal(context+": "+guardDiagnostic(err)), err)
}

func pinParent(path string) (int, string, error) {
	parts := strings.Split(path, "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	for index := 1; index < len(parts)-1; index++ {
		next, err := unix.Openat(fd, parts[index], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, "", err
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

// withParent runs work in the pinned parent; a missing approved path is
// absence only when lstat agrees, never a disappearance during the walk.
func withParent(path string, changed guardRefusal, work func(int, string) error) error {
	parent, name, err := pinParent(path)
	if err == nil {
		err = work(parent, name)
		unix.Close(parent)
	}
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(path); statErr == nil {
			return changed
		}
		return nil
	}
	return err
}

func factAt(fd int, name, relative string) (Entry, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Entry{}, err
	}
	entry := statFact(&stat, relative)
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		buffer := make([]byte, unix.PathMax+1)
		size, err := unix.Readlinkat(fd, name, buffer)
		if err != nil {
			return Entry{}, err
		}
		entry.Link = string(buffer[:size])
	}
	// Recorded names are JSON text; never approve a lossy name or target.
	if !utf8.ValidString(entry.Path) || !utf8.ValidString(entry.Link) {
		return Entry{}, guardRefusal("teardown artifact name is not valid UTF-8")
	}
	return entry, nil
}

func statFact(stat *unix.Stat_t, relative string) Entry {
	return Entry{Path: relative, Mode: stat.Mode, Size: stat.Size, Modified: stat.Mtim.Nano(), Device: stat.Dev, Inode: stat.Ino, UID: stat.Uid, GID: stat.Gid, Changed: stat.Ctim.Nano()}
}

func listNames(directory int) ([]string, error) {
	duplicate, err := unix.FcntlInt(uintptr(directory), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "")
	defer file.Close()
	names, err := file.Readdirnames(-1)
	sort.Strings(names)
	return names, err
}

func openPinned(parent int, name string, fact Entry, replaced guardRefusal) (int, error) {
	directory, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var pinned unix.Stat_t
	if err = unix.Fstat(directory, &pinned); err == nil && (pinned.Dev != fact.Device || pinned.Ino != fact.Inode) {
		err = replaced
	}
	if err != nil {
		unix.Close(directory)
		return -1, err
	}
	return directory, nil
}

func childPath(relative, name string) string {
	if relative == "." {
		return name
	}
	return relative + "/" + name
}

func excluded(relative string, exclusions []string) bool {
	return slices.ContainsFunc(exclusions, func(value string) bool { return relative == value || strings.HasPrefix(relative, value+"/") })
}

func withinOwned(path string, owned []string) bool {
	return path == "." || slices.ContainsFunc(owned, func(item string) bool {
		return path == item || strings.HasPrefix(path, item+"/") || strings.HasPrefix(item, path+"/")
	})
}

// repairAt clears deny on the operator's own entry name under parent. Every
// state it leaves is reported by the caller's own checks.
func repairAt(parent int, name string, deny fs.FileMode) {
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		_ = safefs.RepairFD(fd, name, deny)
		_ = unix.Close(fd)
	}
}

// grantOwnerAccess gives the operator read, write and search access to each
// directory it owns in a private tree, outside exclusions, so the approved
// tree can be captured and removed. It is best effort: capture reports every
// entry it still cannot observe.
// A scoped tree is entered only along its owned paths, as capture does.
func grantOwnerAccess(fd int, name string, limit int, exclusions, owned []string, scoped bool) {
	visited := 0
	var walk func(int, string, string)
	walk = func(parent int, child, relative string) {
		if excluded(relative, exclusions) || scoped && !withinOwned(relative, owned) || visited >= limit {
			return
		}
		visited++
		pinned, err := unix.Openat(parent, child, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return
		}
		defer unix.Close(pinned)
		var stat unix.Stat_t
		if unix.Fstat(pinned, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) {
			return
		}
		if stat.Mode&0o700 != 0o700 && unix.Chmod("/proc/self/fd/"+strconv.Itoa(pinned), stat.Mode&0o7777|0o700) != nil {
			return
		}
		directory, err := openPinned(parent, child, Entry{Device: uint64(stat.Dev), Inode: stat.Ino}, "")
		if err != nil {
			return
		}
		defer unix.Close(directory)
		names, err := listNames(directory)
		if err != nil {
			return
		}
		for _, name := range names {
			var entry unix.Stat_t
			if unix.Fstatat(directory, name, &entry, unix.AT_SYMLINK_NOFOLLOW) == nil && entry.Mode&unix.S_IFMT == unix.S_IFDIR {
				walk(directory, name, childPath(relative, name))
			}
		}
	}
	walk(fd, name, ".")
}

// walkEntries records a sorted metadata inventory and rechecks each directory
// after listing it. Scoped walks preserve paths outside the owned set.
func walkEntries(fd int, name string, limit int, exclusions, owned []string, scoped bool, preserved *[]string) ([]Entry, error) {
	entries := []Entry{}
	var walk func(int, string, string) error
	walk = func(parent int, child, relative string) error {
		if excluded(relative, exclusions) {
			return nil
		}
		if scoped && !withinOwned(relative, owned) {
			if preserved != nil {
				*preserved = append(*preserved, relative)
			}
			return nil
		}
		if len(entries) >= limit {
			return guardRefusal("teardown artifact exceeds bounded metadata inventory")
		}
		fact, err := statAt(parent, child, relative)
		if err != nil {
			return err
		}
		entries = append(entries, fact)
		if fact.Mode&unix.S_IFMT != unix.S_IFDIR {
			return nil
		}
		directory, err := openPinned(parent, child, fact, "teardown directory replaced during observation")
		if err != nil {
			return err
		}
		defer unix.Close(directory)
		names, err := listNames(directory)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := walk(directory, name, childPath(relative, name)); err != nil {
				return err
			}
		}
		again, err := statAt(parent, child, relative)
		if err == nil && again != fact {
			err = guardRefusal("teardown directory changed during observation")
		}
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	return entries, walk(fd, name, ".")
}

func digest(entries []Entry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	// Schema-1 recovery rehashes these exact compact JSON bytes.
	payload, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func artifactFromEntries(path string, entries []Entry) (ports.TeardownArtifact, error) {
	binding, err := digest(entries)
	return ports.TeardownArtifact{Path: path, Entries: entries, Binding: binding}, err
}

func sameFact(approved *Entry, current Entry, resumable bool) bool {
	return approved != nil && (*approved == current || resumable && sameEntry(*approved, current))
}

func checkEntries(approved, current []Entry) error {
	originals := make(map[string]*Entry, len(approved))
	for index := range approved {
		relative := approved[index].Path
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") || filepath.Clean(relative) != relative || originals[relative] != nil {
			return guardRefusal("invalid installation artifact descendant")
		}
		originals[relative] = &approved[index]
	}
	for _, entry := range current {
		if !sameFact(originals[entry.Path], entry, true) {
			return guardRefusal("installation artifact changed after confirmation")
		}
	}
	return nil
}

// removeEntries deletes only approved facts below a pinned parent. Resumable
// directories may lose children; additions, replacement or ownership refuse.
func removeEntries(fd int, name string, entries []Entry, resumable bool, exclusions []string, retainRoot bool, before func() error) error {
	if before == nil {
		before = func() error { return nil }
	}
	approved := make(map[string]*Entry, len(entries))
	for index := range entries {
		approved[entries[index].Path] = &entries[index]
	}
	var remove func(int, string, string) error
	remove = func(parent int, child, relative string) error {
		if excluded(relative, exclusions) {
			return nil
		}
		fact, err := statAt(parent, child, relative)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !sameFact(approved[relative], fact, resumable) {
			return guardRefusal("teardown artifact changed before deletion")
		}
		if fact.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := before(); err != nil {
				return err
			}
			return unix.Unlinkat(parent, child, 0)
		}
		directory, err := openPinned(parent, child, fact, "teardown directory replaced before deletion")
		if err != nil {
			return err
		}
		defer unix.Close(directory)
		names, err := listNames(directory)
		if err != nil {
			return err
		}
		for _, name := range names {
			if key := childPath(relative, name); approved[key] == nil && !excluded(key, exclusions) {
				return guardRefusal("unapproved teardown directory entry")
			}
		}
		for _, name := range names {
			if err := remove(directory, name, childPath(relative, name)); err != nil {
				return err
			}
		}
		// Our unlinks change directory size/times. Ownership and identity must
		// still match; rmdir refuses any entries added during removal.
		current, err := statAt(parent, child, relative)
		if err != nil {
			return err
		}
		if current.Mode != fact.Mode || current.Device != fact.Device || current.Inode != fact.Inode || current.UID != fact.UID || current.GID != fact.GID {
			return guardRefusal("teardown directory ownership changed before deletion")
		}
		if relative == "." && (retainRoot || len(exclusions) > 0) || slices.ContainsFunc(exclusions, func(value string) bool { return strings.HasPrefix(value, relative+"/") }) {
			return nil
		}
		if err := before(); err != nil {
			return err
		}
		return unix.Unlinkat(parent, child, unix.AT_REMOVEDIR)
	}
	if len(entries) == 0 {
		return nil
	}
	return remove(fd, name, ".")
}

// guardArtifact checks an approved artifact against its original facts, or
// its digest when only a binding was approved. A non-nil before removes it and
// runs immediately before each unlink.
// beforeArtifactRemoval is a fixture seam between the artifact check and its removal.
var beforeArtifactRemoval func()

func guardArtifact(path, binding string, originals []Entry, before func() error) error {
	return withParent(path, "teardown artifact changed during observation", func(parent int, name string) error {
		entries, err := walkEntries(parent, name, artifactLimit, nil, nil, false, nil)
		if err != nil {
			return err
		}
		if originals != nil {
			sum, err := digest(originals)
			if err != nil {
				return err
			}
			if sum != binding {
				return guardRefusal("original teardown artifact binding changed")
			}
			if err := checkEntries(originals, entries); err != nil {
				return err
			}
		} else if sum, err := digest(entries); err != nil || sum != binding && len(entries) > 0 {
			return errors.Join(err, guardRefusal("teardown artifact changed after confirmation"))
		}
		if before == nil {
			return nil
		}
		if beforeArtifactRemoval != nil {
			beforeArtifactRemoval()
		}
		if originals == nil {
			return removeEntries(parent, name, entries, false, nil, false, before)
		}
		return removeEntries(parent, name, originals, true, nil, false, before)
	})
}

// CaptureArtifact preserves original facts and their canonical native digest.
func CaptureArtifact(path string) (ports.TeardownArtifact, error) {
	artifact, err := captureArtifact(path)
	if err != nil {
		return ports.TeardownArtifact{Path: path}, classify("capture-artifact", err)
	}
	return artifact, nil
}

func captureArtifact(path string) (ports.TeardownArtifact, error) {
	artifact := ports.TeardownArtifact{Path: path, Entries: []Entry{}}
	err := withParent(path, "teardown artifact changed during observation", func(parent int, name string) error {
		entries, err := walkEntries(parent, name, artifactLimit, nil, nil, false, nil)
		if err == nil {
			artifact.Entries = entries
		}
		return err
	})
	if err == nil {
		artifact.Binding, err = digest(artifact.Entries)
	}
	return artifact, err
}

// RemoveArtifact deletes one approved selected-teardown artifact by binding.
// Cancellation is checked before every unlink and returned unclassified.
func RemoveArtifact(ctx context.Context, path string, approved []ports.TeardownArtifactBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	items, err := approvedBindings(approved)
	if err == nil {
		err = guardSelected(items, path, ctx.Err)
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return err
	}
	return classify("remove-artifact", err)
}

func guardSelected(items []ports.TeardownArtifact, path string, before func() error) error {
	index := slices.IndexFunc(items, func(item ports.TeardownArtifact) bool { return item.Path == path })
	if index < 0 || slices.ContainsFunc(items[index+1:], func(item ports.TeardownArtifact) bool { return item.Path == path }) {
		return guardRefusal("teardown artifact was not approved")
	}
	return guardArtifact(items[index].Path, items[index].Binding, items[index].Entries, before)
}

// approvedBindings validates the bounded artifact transport.
func approvedBindings(approved []ports.TeardownArtifactBinding) ([]ports.TeardownArtifact, error) {
	payload, err := json.Marshal(approved)
	if err != nil {
		return nil, err
	}
	if len(payload) > 65536 {
		return nil, guardRefusal("missing or oversized teardown artifacts")
	}
	items := make([]ports.TeardownArtifact, len(approved))
	for index, item := range approved {
		items[index] = ports.TeardownArtifact{Path: item.Path, Binding: item.Binding}
	}
	return items, validateArtifacts(items)
}

func validateArtifacts(items []ports.TeardownArtifact) error {
	if len(items) > 256 {
		return guardRefusal("invalid teardown artifacts")
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !strings.HasPrefix(item.Path, "/") || item.Path == "/" || filepath.Clean(item.Path) != item.Path {
			return guardRefusal("invalid teardown artifact path")
		}
		if item.Binding != "" && !hexDigest(item.Binding) || seen[item.Path] {
			return guardRefusal("invalid teardown artifact binding")
		}
		seen[item.Path] = true
	}
	return nil
}

func hexDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

// capture records the tree metadata with normalized retained subtrees.
func (tree Tree) capture() (Tree, error) {
	root := tree.Path
	if !strings.HasPrefix(root, "/") || filepath.Clean(root) != root || filepath.Dir(root) == "/" {
		return tree, guardRefusal("invalid installation artifact root")
	}
	exclusions := []string{}
	for _, relative := range tree.Exclusions {
		if filepath.IsAbs(relative) {
			relative, _ = filepath.Rel(root, relative)
		}
		if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || filepath.Clean(relative) != relative {
			return tree, guardRefusal("invalid retained artifact subtree")
		}
		exclusions = append(exclusions, relative)
	}
	preserved := []string{}
	entries := []Entry{}
	err := withParent(root, "installation artifact changed during observation", func(parent int, name string) error {
		walked, err := walkEntries(parent, name, maxEntries, exclusions, tree.OwnedPaths, tree.Scoped, &preserved)
		if err == nil {
			entries = walked
		}
		return err
	})
	if err != nil {
		return tree, err
	}
	if slices.ContainsFunc(entries, func(entry Entry) bool { return entry.Device != entries[0].Device }) {
		return tree, guardRefusal("installation artifact contains mounted descendants")
	}
	tree.Entries, tree.Exclusions, tree.Preserved = entries, append(exclusions, preserved...), preserved
	return tree, nil
}

func (tree Tree) check() error {
	current, err := tree.capture()
	if err != nil {
		return err
	}
	return checkEntries(tree.Entries, current.Entries)
}

func (tree Tree) remove() error {
	if err := tree.check(); err != nil {
		return err
	}
	return withParent(tree.Path, "installation artifact root changed", func(parent int, name string) error {
		return removeEntries(parent, name, tree.Entries, true, tree.Exclusions, false, nil)
	})
}

func (tree Tree) verifyRemoved(remains guardRefusal) error {
	preserved := []string{}
	for _, path := range append(append([]string{}, tree.Exclusions...), tree.Preserved...) {
		if !slices.Contains(preserved, path) {
			preserved = append(preserved, path)
		}
	}
	tree.Scoped, tree.Exclusions = false, preserved
	current, err := tree.capture()
	if err != nil {
		return err
	}
	for _, entry := range current.Entries {
		retained := slices.ContainsFunc(preserved, func(path string) bool { return entry.Path == "." || strings.HasPrefix(path, entry.Path+"/") })
		if entry.Mode&unix.S_IFMT != unix.S_IFDIR || !retained {
			return remains
		}
	}
	return nil
}
