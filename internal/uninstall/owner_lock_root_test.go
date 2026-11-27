package uninstall

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.RunWithOwnerLockRoot(m)) }

// Test data homes coordinate under the run's private root, never in /tmp.
func TestOwnerLockFollowsTestRoot(t *testing.T) {
	root := testkit.TempDir(t)
	t.Setenv(OwnerLockRootEnvironment, root)
	home := filepath.Join(testkit.TempDir(t), "data")
	shared := "/tmp/subyard-owner-locks-" + strconv.Itoa(os.Getuid())
	_, release, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	path := OwnerLockPath(home)
	if !strings.HasPrefix(path, root+"/subyard-owner-locks-") {
		t.Fatalf("lock path %s is outside the test root %s", path, root)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, filepath.Base(path))); !os.IsNotExist(err) {
		t.Fatalf("test lock reached the shared namespace: %v", err)
	}

	for _, invalid := range []string{"relative/root", root + "/../" + filepath.Base(root)} {
		t.Setenv(OwnerLockRootEnvironment, invalid)
		if _, _, err := OwnerLock(context.Background(), home, nil); err == nil || !strings.Contains(err.Error(), "clean absolute path") {
			t.Fatalf("invalid root %q: %v", invalid, err)
		}
	}
	// A test binary without a private root is refused before anything is created.
	t.Setenv(OwnerLockRootEnvironment, "")
	if _, _, err := TryOwnerLock(context.Background(), home); err == nil || !strings.Contains(err.Error(), "testkit.RunWithOwnerLockRoot") {
		t.Fatalf("shared namespace accepted in a test binary: %v", err)
	}
	if path := OwnerLockPath(home); !strings.HasPrefix(path, shared+"/") {
		t.Fatalf("production lock path changed: %s", path)
	}
}
