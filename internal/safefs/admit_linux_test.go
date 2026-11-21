package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAdmitDirStates(t *testing.T) {
	root := testkit.TempDir(t)
	cases := []struct {
		name    string
		prepare func(path string)
		want    fs.FileMode // Expected mode of the entry at path, or of the link target.
	}{
		{"absent", func(string) {}, 0},
		{"exact", admitFixtureDir(t, 0o700), 0o700 | fs.ModeDir},
		{"broader", admitFixtureDir(t, 0o750), 0o750 | fs.ModeDir},
		{"group only", admitFixtureDir(t, 0o050), 0o050 | fs.ModeDir},
		{"narrower", admitFixtureDir(t, 0o500), 0o700 | fs.ModeDir},
		{"no permissions", admitFixtureDir(t, 0o000), 0o700 | fs.ModeDir},
		{"narrower setgid", admitFixtureDir(t, 0o500|fs.ModeSetgid), 0o500 | fs.ModeSetgid | fs.ModeDir},
		{"narrower setuid", admitFixtureDir(t, 0o500|fs.ModeSetuid), 0o500 | fs.ModeSetuid | fs.ModeDir},
		{"narrower sticky", admitFixtureDir(t, 0o500|fs.ModeSticky), 0o700 | fs.ModeSticky | fs.ModeDir},
		{"exact setgid", admitFixtureDir(t, 0o700|fs.ModeSetgid), 0o700 | fs.ModeSetgid | fs.ModeDir},
		{"regular file", func(path string) { testkit.WriteFile(t, path, nil, 0o600) }, 0o600},
		{"dangling symlink", func(path string) { admitFixtureLink(t, filepath.Join(root, "absent-target"), path) }, 0},
		{"symlink to file", func(path string) {
			target := path + ".target"
			testkit.WriteFile(t, target, nil, 0o600)
			admitFixtureLink(t, target, path)
		}, 0o600},
		{"symlink to directory", func(path string) {
			target := path + ".target"
			admitFixtureDir(t, 0o500)(target)
			admitFixtureLink(t, target, path)
		}, 0o500 | fs.ModeDir},
	}
	for _, test := range cases {
		path := filepath.Join(root, test.name)
		test.prepare(path)
		err := AdmitDir(path, 0o700)
		if err != nil {
			t.Errorf("%s: AdmitDir = %v", test.name, err)
		}
		info, statErr := os.Stat(path)
		if test.want == 0 {
			if !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("%s: entry created: %v", test.name, statErr)
			}
			continue
		}
		if statErr != nil || info.Mode()&(fs.ModeType|fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != test.want {
			t.Errorf("%s: mode = %v, want %v (%v)", test.name, info.Mode(), test.want, statErr)
		}
	}
	file := filepath.Join(root, "regular file")
	if err := AdmitDir(filepath.Join(file, "child"), 0o700); err != nil {
		t.Fatalf("lookup error: %v", err)
	}
}

func admitFixtureDir(t *testing.T, mode fs.FileMode) func(string) {
	return func(path string) {
		t.Helper()
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
	}
}

func admitFixtureLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
