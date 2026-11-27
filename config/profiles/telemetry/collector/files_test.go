package collector

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testkit/interrupt"
)

func setFileFault(t *testing.T, fault func(safefs.Point) error) {
	t.Helper()
	fileFault = fault
	t.Cleanup(func() { fileFault = nil })
}

func stageLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".telemetry-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestPrivateDirPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, path string)
		refusal string // Empty: the leaf is a 0700 directory afterwards.
	}{
		{name: "missing leaf and parent", prepare: func(*testing.T, string) {}},
		{name: "own broader", prepare: func(t *testing.T, path string) { mkdirMode(t, path, 0o750) }},
		{name: "own narrower", prepare: func(t *testing.T, path string) { mkdirMode(t, path, 0o500) }},
		{name: "own set-group-ID", prepare: func(t *testing.T, path string) { mkdirMode(t, path, os.ModeSetgid|0o770) }},
		{name: "symlink to own directory", refusal: "data directory must be a real directory",
			prepare: func(t *testing.T, path string) {
				target := path + "-target"
				mkdirMode(t, target, 0o700)
				symlink(t, target, path)
			}},
		{name: "dangling symlink", refusal: "mkdir {path}: file exists",
			prepare: func(t *testing.T, path string) { symlink(t, path+"-missing", path) }},
		{name: "symlink to own file", refusal: "mkdir {path}: not a directory",
			prepare: func(t *testing.T, path string) {
				mkdirMode(t, filepath.Dir(path), 0o700)
				testkit.WriteFile(t, path+"-target", nil, 0o600)
				symlink(t, path+"-target", path)
			}},
		{name: "regular file", refusal: "mkdir {path}: not a directory",
			prepare: func(t *testing.T, path string) {
				mkdirMode(t, filepath.Dir(path), 0o700)
				testkit.WriteFile(t, path, nil, 0o600)
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(testkit.TempDir(t), "parent", "data")
			test.prepare(t, path)
			before, _ := os.Lstat(path)
			err := privateDir(path)
			if test.refusal == "" {
				if err != nil {
					t.Fatal(err)
				}
				for _, dir := range []string{filepath.Dir(path), path} {
					if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&(fs.ModePerm|fs.ModeSetgid|fs.ModeSticky) != 0o700 {
						t.Fatalf("%s is not a 0700 directory: %v (%v)", dir, info, err)
					}
				}
				return
			}
			if want := strings.ReplaceAll(test.refusal, "{path}", path); err == nil || err.Error() != want {
				t.Fatalf("privateDir error = %v, want %q", err, want)
			}
			if after, err := os.Lstat(path); err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
				t.Fatalf("refused entry changed: %v -> %v (%v)", before, after, err)
			}
		})
	}
	t.Run("another owner", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root owns the system directory")
		}
		info, err := os.Stat("/usr")
		if err != nil || info.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid()) || info.Mode().Perm()&0o077 == 0 {
			t.Skip("no foreign shared directory at /usr")
		}
		const want = "telemetry directory /usr must not be accessible by group or others"
		if err := privateDir("/usr"); err == nil || err.Error() != want {
			t.Fatalf("privateDir error = %v, want %q", err, want)
		}
	})
}

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func TestPublishPrivateReplacesOnlyOwnRegularFiles(t *testing.T) {
	data := []byte(`{"state":"new"}`)
	t.Run("broader own file", func(t *testing.T) {
		path := filepath.Join(testkit.TempDir(t), "producer.json")
		testkit.WriteFile(t, path, []byte("old"), 0o644)
		before, _ := os.Lstat(path)
		if err := publishPrivate(path, data); err != nil {
			t.Fatal(err)
		}
		after, err := os.Lstat(path)
		if err != nil || after.Mode() != 0o600 || os.SameFile(before, after) {
			t.Fatalf("published file: %v (%v), replaced inode %v", after, err, !os.SameFile(before, after))
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
			t.Fatalf("published content = %q", got)
		}
	})
	for _, test := range []struct {
		name, refusal string
		prepare       func(t *testing.T, path string)
	}{
		{"symlink", "telemetry file must be regular with mode 0000", func(t *testing.T, path string) {
			testkit.WriteFile(t, path+"-target", []byte("old"), 0o600)
			symlink(t, path+"-target", path)
		}},
		{"directory", "telemetry file must be regular with mode 0000", func(t *testing.T, path string) {
			mkdirMode(t, path, 0o700)
		}},
		{"hard link", "telemetry file has unsafe ownership or links", func(t *testing.T, path string) {
			testkit.WriteFile(t, path, []byte("old"), 0o600)
			if err := os.Link(path, path+"-link"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := testkit.TempDir(t)
			path := filepath.Join(dir, "producer.json")
			test.prepare(t, path)
			before := interrupt.Tree(t, dir)
			if err := publishPrivate(path, data); err == nil || err.Error() != test.refusal {
				t.Fatalf("publishPrivate error = %v, want %q", err, test.refusal)
			}
			if after := interrupt.Tree(t, dir); !maps.Equal(before, after) {
				t.Fatalf("refusal changed the directory: %v -> %v", before, after)
			}
		})
	}
	t.Run("symlink placed after staging", func(t *testing.T) {
		dir := testkit.TempDir(t)
		path := filepath.Join(dir, "producer.json")
		setFileFault(t, func(point safefs.Point) error {
			if point == safefs.AfterWrite {
				symlink(t, filepath.Join(dir, "elsewhere"), path)
			}
			return nil
		})
		if err := publishPrivate(path, data); !errors.Is(err, safefs.ErrSymlink) {
			t.Fatalf("publishPrivate error = %v, want a symlink refusal", err)
		}
		if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink was replaced: %v (%v)", info, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "elsewhere")); !os.IsNotExist(err) {
			t.Fatalf("publication followed the symlink: %v", err)
		}
		if leftovers := stageLeftovers(t, dir); len(leftovers) != 0 {
			t.Fatalf("refusal left staging files: %v", leftovers)
		}
	})
}

// TestPrepareConvergesAfterEveryFileCut crashes `prepare` at each safefs point of
// the data directory, spool directory and both state publications. A crash keeps
// .telemetry-* staging files, which the signature ignores: they are never read
// and a retry publishes through fresh ones.
func TestPrepareConvergesAfterEveryFileCut(t *testing.T) {
	interrupt.Sweep(t, interrupt.Case[string]{
		Setup: func(t *testing.T) string {
			dir := filepath.Join(testkit.TempDir(t), "data")
			s, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			// Fixed identities make the published state comparable across fixtures.
			_, err = s.db.Exec(`UPDATE metadata SET value=CASE key
				WHEN 'producer_id' THEN '00112233445566778899aabbccddeeff'
				ELSE '0123456789abcdef0123456789abcdef' END
				WHERE key IN ('producer_id','store_generation_id')`)
			if err = errors.Join(err, s.Close()); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			return dir
		},
		Run: func(t *testing.T, dir string, in *interrupt.Injector) interrupt.Outcome {
			setFileFault(t, in.Safefs())
			defer func() { fileFault = nil }()
			return interrupt.Outcome{Err: RunCLI([]string{"prepare", "--data-dir", dir}, nil)}
		},
		Signature: func(t *testing.T, dir string) any {
			return interrupt.Tree(t, dir, "telemetry.sqlite*", ".telemetry-*")
		},
		Completed: func(t *testing.T, dir string, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatal(o.Err)
			}
			state, err := loadProducerFiles(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(state, `"origin_id":"0123456789abcdef0123456789abcdef"`) {
				t.Fatalf("published state: %s", state)
			}
		},
		Interrupted: func(t *testing.T, _ string, p interrupt.Point, o interrupt.Outcome) {
			if !errors.Is(o.Err, interrupt.ErrInjected) {
				t.Fatalf("error = %v, want the injected crash at %s", o.Err, p)
			}
		},
		Resumed: func(t *testing.T, _ string, p interrupt.Point, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatalf("prepare after %s: %v", p, o.Err)
			}
		},
		Mode:      interrupt.Crash,
		MinPoints: 10,
		Require: []string{string(safefs.AfterDirChmod), string(safefs.AfterDirCreate), string(safefs.AfterStage),
			string(safefs.BeforePublish), string(safefs.AfterPublish)},
	})
}

func loadProducerFiles(dir string) (string, error) {
	var out strings.Builder
	for _, name := range []string{"producer.json", "spool-policy.json"} {
		path := filepath.Join(dir, name)
		if err := checkPrivateFile(path, 0o600); err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		out.Write(data)
	}
	return out.String(), nil
}
