// Package uninstall owns metadata-only inventory and exact, resumable file
// removal for an approved local installation. It never discovers new targets
// while applying a plan.
package uninstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const maxEntries = 131072

type Entry = ports.ArtifactEntry

// Tree adds native owner operations to the single persisted artifact record.
type Tree ports.TeardownArtifact

// CaptureArtifact preserves original facts and their canonical native digest.
func CaptureArtifact(path string) (ports.TeardownArtifact, error) {
	artifact := ports.TeardownArtifact{Path: path}
	err := nativeArtifact("capture-artifact", artifact, &artifact)
	return artifact, err
}

// Full and selected teardown use the same ancestor-pinned native inventory
// and remover. Go retains only metadata records for typed assessment/recovery.
func Capture(path string) (Tree, error) {
	return captureTree(Tree{Path: path, Entries: []Entry{}}, false)
}
func CaptureScoped(path string, owned, exclusions []string) (Tree, error) {
	return captureTree(Tree{Path: path, Entries: []Entry{}, Scoped: true, OwnedPaths: owned, Exclusions: exclusions}, true)
}
func CapturePrivate(path string) (Tree, error) { return CapturePrivateExcluded(path, nil) }
func CapturePrivateExcluded(path string, exclusions []string) (Tree, error) {
	return captureTree(Tree{Path: path, Entries: []Entry{}, Exclusions: exclusions}, true)
}
func captureTree(tree Tree, private bool) (Tree, error) {
	if err := safePath(tree.Path); err != nil {
		return tree, err
	}
	root, err := CaptureRoot(tree.Path)
	if err != nil || len(root.Entries) == 0 {
		return tree, err
	}
	if !tree.Scoped && root.Entries[0].Mode&syscall.S_IFMT != syscall.S_IFDIR {
		tree.Entries = root.Entries
		return tree, nil
	}
	err = tree.native("capture-tree", &tree)
	if private && errors.Is(err, os.ErrPermission) {
		tree.Entries = root.Entries
		tree.Opaque = true
		return tree, nil
	}
	return tree, err
}
func (tree Tree) VerifyRemoved() error { return tree.native("verify-tree", nil) }
func (tree Tree) Remove() error {
	if tree.Opaque {
		return errors.New("private uninstall root requires the approved privileged boundary")
	}
	return tree.native("remove-tree", nil)
}
func (tree Tree) Check() error {
	if len(tree.Entries) == 0 || tree.Opaque || tree.Entries[0].Mode&syscall.S_IFMT != syscall.S_IFDIR {
		current, err := CaptureRoot(tree.Path)
		if err != nil {
			return err
		}
		if len(current.Entries) == 0 {
			return nil
		}
		if len(tree.Entries) == 0 || !sameEntry(tree.Entries[0], current.Entries[0]) {
			return domain.ErrPlanStale
		}
		return nil
	}
	return tree.native("check-tree", nil)
}

var artifactGuard atomic.Value

// UseArtifactGuard selects the retained, shipped guard before removing runtime.
func UseArtifactGuard(root string) {
	artifactGuard.Store(filepath.Join(root, "scripts/lib/teardown-plan.py"))
}
func (tree Tree) native(mode string, result *Tree) error {
	if result == nil {
		return nativeArtifact(mode, tree, nil)
	}
	return nativeArtifact(mode, tree, result)
}
func nativeArtifact(mode string, input, result any) error {
	script, _ := artifactGuard.Load().(string)
	if script == "" {
		executable, _ := os.Executable()
		script = filepath.Join(filepath.Dir(executable), "../scripts/lib/teardown-plan.py")
		if strings.HasSuffix(executable, ".test") {
			working, _ := os.Getwd()
			for working != "/" {
				candidate := filepath.Join(working, "scripts/lib/teardown-plan.py")
				if _, err := os.Stat(candidate); err == nil {
					script = candidate
					break
				}
				working = filepath.Dir(working)
			}
		}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	command := exec.Command("python3", "-B", script, mode)
	barriers, err := borrowOwnerBarriers()
	if err != nil {
		return err
	}
	defer func() {
		for _, file := range barriers {
			file.Close()
		}
	}()
	command.ExtraFiles = barriers
	command.Stdin = bytes.NewReader(body)
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if exit.ExitCode() == 77 {
				return os.ErrPermission
			}
			if exit.ExitCode() == 75 {
				return fmt.Errorf("%w: native installation artifact guard", domain.ErrPlanStale)
			}
		}
		return fmt.Errorf("native teardown artifact guard failed (%s): %w", mode, err)
	}
	if result != nil {
		return json.Unmarshal(output, result)
	}
	return nil
}

func safePath(path string) error {
	if slices.Contains([]string{"/usr/local", "/usr/local/bin", "/usr/bin", "/usr/sbin", "/var/lib", "/var/log", "/etc/systemd", "/etc/systemd/system"}, path) || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") || filepath.Dir(path) == "/" {
		return errors.New("uninstall requires a clean, non-root absolute artifact path")
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("uninstall artifact has a non-directory or symbolic-link ancestor: %s", current)
		}
	}
	return nil
}

func fact(path, relative string) (Entry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Entry{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Entry{}, errors.New("native uninstall artifact identity is unavailable")
	}
	entry := Entry{Path: relative, Device: uint64(stat.Dev), Inode: stat.Ino, Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid, Size: stat.Size, Modified: stat.Mtim.Nano(), Changed: stat.Ctim.Nano()}
	if info.Mode()&fs.ModeSymlink != 0 {
		entry.Link, err = os.Readlink(path)
	}
	return entry, err
}

func sameEntry(approved, current Entry) bool {
	if approved.Mode&syscall.S_IFMT == syscall.S_IFDIR {
		approved.Size, current.Size = 0, 0
		approved.Modified, current.Modified = 0, 0
		approved.Changed, current.Changed = 0, 0
	}
	return approved == current
}

func openParent(path string) (*os.Root, error) {
	root, err := os.OpenRoot("/")
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		before, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !before.IsDir() {
			root.Close()
			return nil, errors.New("uninstall parent became a symbolic link or non-directory")
		}
		child, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		pinned, err := child.Stat(".")
		if err != nil || !os.SameFile(before, pinned) {
			root.Close()
			child.Close()
			return nil, domain.ErrPlanStale
		}
		root.Close()
		root = child
	}
	return root, nil
}

// CaptureRoot records an installation generation without reading descendants.
func CaptureRoot(path string) (Tree, error) {
	tree := Tree{Path: path, Entries: []Entry{}}
	if err := safePath(path); err != nil {
		return tree, err
	}
	entry, err := fact(path, ".")
	if errors.Is(err, os.ErrNotExist) {
		return tree, nil
	}
	if err != nil {
		return tree, err
	}
	tree.Entries = append(tree.Entries, entry)
	return tree, nil
}

func CheckGeneration(approved []Tree) error {
	for _, tree := range approved {
		current, err := CaptureRoot(tree.Path)
		if err != nil {
			return err
		}
		if len(current.Entries) != len(tree.Entries) {
			return fmt.Errorf("%w: installation generation changed while waiting for owner mutation lock", domain.ErrPlanStale)
		}
		if len(tree.Entries) > 0 && !sameEntry(tree.Entries[0], current.Entries[0]) {
			return fmt.Errorf("%w: installation root identity changed while waiting for owner mutation lock", domain.ErrPlanStale)
		}
	}
	return nil
}
