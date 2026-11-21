package interrupt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Tree is a "no leftovers" signature of root: relative path → type, permission and
// setuid, setgid and sticky bits, and content digest or link target. A missing root yields an empty map. Ignore takes path.Match
// patterns on slash-separated relative paths; a matching directory is skipped whole.
// Link targets under root are recorded relative to it, so fresh fixture roots compare equal.
func Tree(t *testing.T, root string, ignore ...string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && name == root {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		for _, pattern := range ignore {
			if matched, _ := path.Match(pattern, relative); matched {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		bits := mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		switch {
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if strings.HasPrefix(target, root+string(filepath.Separator)) {
				target = "$ROOT" + strings.TrimPrefix(target, root)
			}
			tree[relative] = fmt.Sprintf("link %v -> %s", bits, target)
		case mode.IsDir():
			tree[relative] = fmt.Sprintf("dir %v", bits)
		case mode.IsRegular():
			body, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(body)
			tree[relative] = fmt.Sprintf("file %v %s", bits, hex.EncodeToString(digest[:]))
		default:
			tree[relative] = fmt.Sprintf("special %v", mode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("interrupt: read tree %s: %v", root, err)
	}
	return tree
}
