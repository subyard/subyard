package uninstall

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTreeRemovalPreservesOutsideSymlinkTargets(t *testing.T) {
	base := testkit.TempDir(t)
	root := filepath.Join(base, "owned")
	outside := filepath.Join(base, "foreign")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, outside, []byte("foreign"), 0600)
	testkit.WriteFile(t, filepath.Join(root, "nested", "data"), []byte("owned"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	tree, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root remains: %v", err)
	}
	value, err := os.ReadFile(outside)
	if err != nil || string(value) != "foreign" {
		t.Fatalf("foreign changed: %q %v", value, err)
	}
	if err := tree.Remove(); err != nil {
		t.Fatalf("retry absent: %v", err)
	}
}

func TestTreeRetryRetainsExactApprovedIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
		stale  bool
	}{
		{name: "interrupted", mutate: func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "first")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "added", mutate: func(t *testing.T, root string) { testkit.WriteFile(t, filepath.Join(root, "new"), []byte("new"), 0600) }, stale: true},
		{name: "replacement", mutate: func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, "second"), filepath.Join(root, "old")); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "second"), []byte("second"), 0600)
		}, stale: true},
		{name: "permissions", mutate: func(t *testing.T, root string) {
			if err := os.Chmod(root, 0777); err != nil {
				t.Fatal(err)
			}
		}, stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(testkit.TempDir(t), "owned")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "first"), []byte("first"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "second"), []byte("second"), 0600)
			tree, err := Capture(root)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, root)
			err = tree.Remove()
			if test.stale {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("stale=%v", err)
				}
				if _, err := os.Lstat(filepath.Join(root, "second")); err != nil {
					t.Fatalf("deleted before refusal: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTreeRefusesSymbolicLinkAncestors(t *testing.T) {
	base := testkit.TempDir(t)
	root := filepath.Join(base, "owned")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "data"), []byte("data"), 0600)
	if err := os.Symlink(root, filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(filepath.Join(base, "alias", "data")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if _, err := Capture("/"); err == nil {
		t.Fatal("filesystem root accepted")
	}
}

func TestInstallationAbsentRetainsChangedReadRefusal(t *testing.T) {
	missing := &os.PathError{Op: "lstat", Path: "installation.json", Err: os.ErrNotExist}
	if !InstallationAbsent(missing) {
		t.Fatal("initial absence no longer selects optional inventory handling")
	}
	changed := errors.Join(ErrInstallationChanged, missing)
	var pathError *os.PathError
	if !errors.Is(changed, os.ErrNotExist) || !errors.As(changed, &pathError) || InstallationAbsent(changed) {
		t.Fatal("changed inventory selected absence fallback or lost read cause")
	}
}
