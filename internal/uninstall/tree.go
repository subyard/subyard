// Package uninstall owns metadata-only inventory and exact, resumable file
// removal for an approved local installation. It never discovers new targets
// while applying a plan.
package uninstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const maxEntries = 131072

type Entry = ports.ArtifactEntry

// Tree adds native owner operations to the single persisted artifact record.
type Tree ports.TeardownArtifact

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
	if private {
		exclusions := []string{}
		for _, relative := range tree.Exclusions {
			if filepath.IsAbs(relative) {
				relative, _ = filepath.Rel(tree.Path, relative)
			}
			exclusions = append(exclusions, relative)
		}
		_ = withParent(tree.Path, "", func(parent int, name string) error {
			grantOwnerAccess(parent, name, maxEntries, exclusions)
			return nil
		})
	}
	captured, err := tree.capture()
	err = classify("capture-tree", err)
	if private && errors.Is(err, os.ErrPermission) {
		tree.Entries = root.Entries
		tree.Opaque = true
		return tree, nil
	}
	if err != nil {
		return tree, err
	}
	return captured, nil
}
func (tree Tree) VerifyRemoved() error {
	return classify("verify-tree", tree.verifyRemoved("approved installation artifact remains"))
}
func (tree Tree) Remove() error {
	if tree.Opaque {
		return errors.New("private uninstall root requires the approved privileged boundary")
	}
	return classify("remove-tree", tree.remove())
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
	return classify("check-tree", tree.check())
}

func safePath(path string) error {
	if slices.Contains([]string{"/usr/local", "/usr/local/bin", "/usr/bin", "/usr/sbin", "/var/lib", "/var/log", "/etc/systemd", "/etc/systemd/system"}, path) || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") || filepath.Dir(path) == "/" {
		return config.OwnerMetadataError(path, "uninstall artifact path", "uninstall requires a clean, non-root absolute artifact path", "", nil)
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return config.OwnerMetadataError(current, "uninstall artifact ancestor", config.MetadataAccessReason(err), "", err)
		}
		if !info.IsDir() {
			return config.OwnerMetadataError(current, "uninstall artifact ancestor", "has a non-directory or symbolic-link ancestor; "+config.FileMetadata(info)+"; required type=directory", "", nil)
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
		if err != nil {
			root.Close()
			child.Close()
			return nil, config.OwnerMetadataError(path, "uninstall artifact ancestor", config.MetadataAccessReason(err), "", errors.Join(domain.ErrPlanStale, err))
		}
		if !os.SameFile(before, pinned) {
			root.Close()
			child.Close()
			return nil, config.OwnerMetadataError(path, "uninstall artifact ancestor", "inode identity changed while opening", "", domain.ErrPlanStale)
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
			return config.OwnerMetadataError(tree.Path, "installation generation", "changed while waiting for owner mutation lock", fmt.Sprintf("stored entries=%d, current entries=%d", len(tree.Entries), len(current.Entries)), domain.ErrPlanStale)
		}
		if len(tree.Entries) > 0 && !sameEntry(tree.Entries[0], current.Entries[0]) {
			return config.OwnerMetadataError(tree.Path, "installation root identity", "changed while waiting for owner mutation lock", entryIdentityFacts(tree.Entries[0], current.Entries[0]), domain.ErrPlanStale)
		}
	}
	return nil
}
