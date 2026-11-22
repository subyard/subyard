package safefs_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestPythonParityVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/python-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Operation   string
		Target      string
		InitialMode uint32 `json:"initial_mode"`
		Mode        fs.FileMode
		Umask       int
		NoReplace   bool `json:"no_replace"`
		Outcome     string
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for i, vector := range vectors {
		t.Run(fmt.Sprintf("%d-%s", i, vector.Target), func(t *testing.T) {
			dir := testkit.TempDir(t)
			path, other := filepath.Join(dir, "target"), filepath.Join(dir, "untouched")
			switch vector.Operation {
			case "finish-directory":
				checkFinishParity(t, path, vector.InitialMode, vector.Mode, vector.Outcome)
				return
			case "resolve-file":
				checkDanglingParity(t, path, filepath.Join(dir, "missing"), vector.Outcome)
				return
			case "", "write-file":
			default:
				t.Fatalf("unknown vector operation %q", vector.Operation)
			}
			testkit.WriteFile(t, other, []byte("untouched"), 0o600)
			var err error
			switch vector.Target {
			case "absent":
			case "regular":
				testkit.WriteFile(t, path, []byte("old"), 0o600)
			case "symlink":
				err = os.Symlink(other, path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			default:
				t.Fatalf("unknown vector target %q", vector.Target)
			}
			if err != nil {
				t.Fatal(err)
			}
			mask := unix.Umask(vector.Umask)
			t.Cleanup(func() { unix.Umask(mask) })
			err = safefs.WriteFile(path, []byte("new"), safefs.Options{Mode: vector.Mode, NoReplace: vector.NoReplace})
			var actual string
			switch {
			case err == nil:
				actual = "published"
				checkMode(t, path, vector.Mode)
			case errors.Is(err, safefs.ErrSymlink):
				actual = "symlink"
			case errors.Is(err, safefs.ErrNotRegular):
				actual = "not regular"
			case errors.Is(err, fs.ErrExist):
				actual = "exists"
			default:
				t.Fatalf("unexpected write error: %v", err)
			}
			if actual != vector.Outcome {
				t.Fatalf("outcome = %q, want %q", actual, vector.Outcome)
			}
			want := "old"
			if actual == "published" {
				want = "new"
			} else if vector.Target == "symlink" {
				want = "untouched"
				if target, err := os.Readlink(path); err != nil || target != other {
					t.Fatalf("symlink target = %q, %v", target, err)
				}
			} else if vector.Target != "regular" {
				return
			}
			if body, err := os.ReadFile(path); err != nil || string(body) != want {
				t.Fatalf("read = %q, %v, want %q", body, err, want)
			}
		})
	}
}

func checkFinishParity(t *testing.T, path string, initial uint32, mode fs.FileMode, want string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chmod(path, initial); err != nil {
		t.Fatal(err)
	}
	var before, after unix.Stat_t
	if err := unix.Stat(path, &before); err != nil {
		t.Fatal(err)
	}
	if err := safefs.EnsureDir(path, safefs.DirOptions{Mode: mode}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(path, &after); err != nil {
		t.Fatal(err)
	}
	if actual := fmt.Sprintf("%04o", after.Mode&0o7777); actual != want {
		t.Fatalf("mode = %s, want %s", actual, want)
	}
	if after.Uid != before.Uid || after.Gid != before.Gid {
		t.Fatal("finishing the directory changed its owner or group")
	}
}

func checkDanglingParity(t *testing.T, path, missing, want string) {
	t.Helper()
	if err := os.Symlink(filepath.Base(missing), path); err != nil {
		t.Fatal(err)
	}
	resolved, err := safefs.ResolveFile(path)
	if want != "resolved missing target" || err != nil || resolved != missing {
		t.Fatalf("ResolveFile = %q, %v, want %s", resolved, err, want)
	}
	if link, err := os.Readlink(path); err != nil || link != filepath.Base(missing) {
		t.Fatalf("link = %q, %v", link, err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing target lookup = %v", err)
	}
}

func TestInlinePython(t *testing.T) {
	const marker = "import safefs  # safefs: inline"
	for _, program := range []string{"import safefs", marker + "\n" + marker} {
		if _, err := safefs.InlinePython(program); err == nil {
			t.Fatalf("accepted invalid program %q", program)
		}
	}
	program, err := safefs.InlinePython("# first line\n" + marker + "\nimport safefs\nassert safefs.DURABLE == 'durable'\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(program, "\n") != 4 || len(program) >= 120*1024 {
		t.Fatal("unexpected embedded program line count or size")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	checkout := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(checkout, "safefs.py"), []byte("raise RuntimeError('unexpected import')\n"), 0o600)
	cmd := exec.Command(python, "-B", "-c", program)
	cmd.Dir = checkout
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedded program failed: %v\n%s", err, output)
	}
}
