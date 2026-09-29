//go:build linux

package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestExecutablePathKeepsPinnedRootAfterPathReplacement(t *testing.T) {
	for _, process := range []string{"self", fmt.Sprint(os.Getpid())} {
		t.Run(process, func(t *testing.T) {
			parent := testkit.TempDir(t)
			root := filepath.Join(parent, "release")
			hook := filepath.Join("config", "profiles", "sample", "guest.sh")
			writePinnedHook(t, filepath.Join(root, hook), "original\n")
			pin, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			anchor := fmt.Sprintf("/proc/%s/fd/%d", process, pin.Fd())
			definition := Definition{Root: filepath.Join(anchor, "config", "profiles", "sample")}
			if err := os.Rename(root, root+"-retained"); err != nil {
				t.Fatal(err)
			}
			writePinnedHook(t, filepath.Join(root, hook), "replacement\n")
			path, err := definition.ExecutablePath("guest.sh")
			if err != nil || !strings.HasPrefix(path, fmt.Sprintf("/proc/%d/fd/%d/", os.Getpid(), pin.Fd())) {
				t.Fatalf("pinned executable path = %q, %v", path, err)
			}
			if data, err := definition.ReadExecutable("guest.sh"); err != nil || string(data) != "original\n" {
				t.Fatalf("pinned hook = %q, %v", data, err)
			}
			if err := pin.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := definition.ReadExecutable("guest.sh"); err == nil {
				t.Fatal("accepted a closed directory descriptor")
			}
		})
	}
}

func TestExecutablePathRejectsSymlinksBelowPinnedRoot(t *testing.T) {
	for _, relative := range []string{"config", "config/profiles", "config/profiles/sample", "config/profiles/sample/nested", "config/profiles/sample/nested/guest.sh"} {
		t.Run(relative, func(t *testing.T) {
			root := testkit.TempDir(t)
			writePinnedHook(t, filepath.Join(root, "config/profiles/sample/nested/guest.sh"), "safe\n")
			pin, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			definition := Definition{Root: fmt.Sprintf("/proc/%d/fd/%d/config/profiles/sample", os.Getpid(), pin.Fd())}
			path := filepath.Join(root, relative)
			if err := os.Rename(path, path+"-real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+"-real", path); err != nil {
				t.Fatal(err)
			}
			if _, err := definition.ReadExecutable("nested/guest.sh"); err == nil {
				t.Fatal("accepted a substituted path below the directory pin")
			}
		})
	}
}

func writePinnedHook(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content), 0o700)
}
