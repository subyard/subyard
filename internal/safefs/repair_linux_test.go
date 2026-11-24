package safefs

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestRepairPathStates(t *testing.T) {
	root := testkit.TempDir(t)
	cases := []struct {
		name    string
		prepare func(path string)
		deny    fs.FileMode
		want    fs.FileMode
		err     error
	}{
		{"broader file", func(path string) { testkit.WriteFile(t, path, nil, 0o644) }, 0o077, 0o600, nil},
		{"group write", func(path string) { testkit.WriteFile(t, path, nil, 0o664) }, 0o022, 0o644, nil},
		{"executable", func(path string) { testkit.WriteFile(t, path, nil, 0o755) }, 0o077, 0o700, nil},
		{"read only private", func(path string) { testkit.WriteFile(t, path, nil, 0o400) }, 0o077, 0o400, nil},
		{"read other allowed", func(path string) { testkit.WriteFile(t, path, nil, 0o644) }, 0o022, 0o644, nil},
		{"setuid file", func(path string) { testkit.WriteFile(t, path, nil, 0o600|fs.ModeSetuid) }, 0o077, 0o600, nil},
		{"broader directory", admitFixtureDir(t, 0o755), 0o077, 0o700 | fs.ModeDir, nil},
		{"sticky directory", admitFixtureDir(t, 0o777|fs.ModeSticky), 0o077, 0o700 | fs.ModeSticky | fs.ModeDir, nil},
		{"setgid directory", admitFixtureDir(t, 0o755|fs.ModeSetgid), 0o077, 0o700 | fs.ModeSetgid | fs.ModeDir, nil},
		{"setgid private directory", admitFixtureDir(t, 0o700|fs.ModeSetgid), 0o077, 0o700 | fs.ModeSetgid | fs.ModeDir, nil},
		{"symlink", func(path string) {
			testkit.WriteFile(t, path+".target", nil, 0o644)
			admitFixtureLink(t, path+".target", path)
		}, 0o077, 0, ErrSymlink},
		{"hard link", func(path string) {
			testkit.WriteFile(t, path+".other", nil, 0o644)
			if err := os.Link(path+".other", path); err != nil {
				t.Fatal(err)
			}
		}, 0o077, 0o644, ErrMultipleLinks},
		{"fifo", func(path string) {
			if err := unix.Mkfifo(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, 0o077, 0, ErrNotRegular},
		{"absent", func(string) {}, 0o077, 0, fs.ErrNotExist},
	}
	for _, test := range cases {
		path := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-"))
		test.prepare(path)
		err := RepairPath(path, test.deny)
		if test.err != nil {
			if !errors.Is(err, test.err) || !strings.Contains(err.Error(), path) {
				t.Errorf("%s: RepairPath = %v, want %v naming the path", test.name, err, test.err)
			}
		} else if err != nil {
			t.Errorf("%s: RepairPath = %v", test.name, err)
		}
		if test.want == 0 {
			continue
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&(fs.ModeType|fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != test.want {
			t.Errorf("%s: mode = %v, want %v (%v)", test.name, info.Mode(), test.want, statErr)
		}
	}
}

func TestRepairPathRefusesForeignOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns the system fixture")
	}
	if err := RepairPath("/etc/passwd", 0o077); !errors.Is(err, ErrUnsafeOwner) {
		t.Fatalf("RepairPath = %v, want %v", err, ErrUnsafeOwner)
	}
}

func TestRepairModeUsesOpenDescriptor(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "file")
	testkit.WriteFile(t, path, []byte("x"), 0o640)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, nil, 0o644)
	if err := RepairMode(file, 0o077); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fs.FileMode{path + ".moved": 0o600, path: 0o644} {
		if info, err := os.Stat(name); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v (%v)", name, info.Mode(), want, err)
		}
	}
}

func TestRepairPathClearsAccessACL(t *testing.T) {
	setfacl, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("setfacl is not installed")
	}
	root := testkit.TempDir(t)
	if output, err := exec.Command(setfacl, "-m", "d:u:nobody:rwx", root).CombinedOutput(); err != nil {
		t.Skipf("filesystem without ACL support: %v: %s", err, output)
	}
	path := filepath.Join(root, "file")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := RepairPath(path, 0o077); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v (%v)", info.Mode(), err)
	}
	if getfacl, err := exec.LookPath("getfacl"); err == nil {
		output, err := exec.Command(getfacl, "-c", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(output), "mask::---") {
			t.Fatalf("ACL mask still grants access:\n%s", output)
		}
	}
}
