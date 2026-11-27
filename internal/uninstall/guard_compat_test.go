package uninstall

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRecoverySchemaAcceptsCurrentOnly(t *testing.T) {
	_, plan := recoveryFixture(t)
	for schema, accepted := range map[int]bool{0: false, 1: false, RecoverySchema: true, RecoverySchema + 1: false} {
		plan.Schema = schema
		if err := plan.Validate(plan.Installation.OperatorUID); (err == nil) != accepted {
			t.Fatalf("schema %d: %v", schema, err)
		}
	}
}

// Schema-2 plans saved by v0.31.0 and v0.32.0 rehash these exact bytes. The
// digest is frozen from an independent Python encoding of Go's HTML-safe JSON.
func TestGuardDigestMatchesFrozenSchemaTwoBytes(t *testing.T) {
	entries := []Entry{
		{Path: ".", Mode: 16832, Size: 4096, Modified: -1, Device: ^uint64(0), Inode: 2, UID: ^uint32(0), Changed: 1763500000987654321},
		{Path: "link <&>\t\"é😀\u2028\\", Mode: 41471, Size: 24, Link: "../t\b\f\x01\u2029"},
	}
	if got, err := digest(entries); err != nil || got != "578a8b3357da6b886d85e71dea4d81758cc5bd8130224f635a9123c8694bdefe" {
		t.Fatalf("digest %q (%v) differs from the frozen schema-2 bytes", got, err)
	}
}

// A schema-1 plan saved by v0.30.0 or older refuses before the protected read
// repairs the store. Only an executable regular retained launcher is offered,
// always with the v0.32.0 fallback.
func TestRecoveryStoreRefusesSchemaOneWithNextStep(t *testing.T) {
	for _, shape := range []string{"absent", "dangling", "directory", "not-executable", "executable"} {
		t.Run(shape, func(t *testing.T) {
			store, plan := recoveryFixture(t)
			plan.Schema = 1
			body, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(store.Directory, "plan.json")
			testkit.WriteFile(t, planPath, body, 0644)
			launcher := filepath.Join(store.Directory, "runtime", "bin", "yard")
			if shape != "absent" {
				if err := os.MkdirAll(filepath.Dir(launcher), 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch shape {
			case "dangling":
				err = os.Symlink(filepath.Join(store.Directory, "missing"), launcher)
			case "directory":
				err = os.Mkdir(launcher, 0700)
			case "not-executable", "executable":
				mode := map[string]os.FileMode{"not-executable": 0600, "executable": 0700}[shape]
				testkit.WriteFile(t, launcher, []byte("retained release"), mode)
			}
			if err != nil || os.Chmod(store.Directory, 0755) != nil {
				t.Fatal("prepare store", err)
			}
			before := statTimes(t, store.Directory, planPath)
			fallback := "install Subyard 0.32.0 (https://github.com/Subyard/Subyard/releases/tag/v0.32.0), the last release that resumes schema 1 plans, then run:\n  yard _uninstall-resume " + store.Directory
			want := ", " + fallback
			if shape == "executable" {
				want = " and resume it with its retained release:\n  " + launcher + " _uninstall-resume " + store.Directory + "\nIf that fails, " + fallback
			}
			for name, read := range map[string]func() error{
				"read":   func() error { _, _, err := store.Read(); return err },
				"resume": func() error { _, err := store.ReadForResume(t.Context()); return err },
			} {
				err := read()
				var refusal LegacyRecoverySchemaError
				if !errors.As(err, &refusal) || !strings.HasSuffix(err.Error(), "Retain "+store.Directory+want) {
					t.Fatalf("%s: %v", name, err)
				}
			}
			if after := statTimes(t, store.Directory, planPath); !reflect.DeepEqual(after, before) {
				t.Fatalf("refusal changed the store: %v, was %v", after, before)
			}
			if saved, err := os.ReadFile(planPath); err != nil || !bytes.Equal(saved, body) {
				t.Fatal("refused schema-1 plan changed", err)
			}
		})
	}
}

// Every state a read cannot prove current or empty stays as found and refuses;
// a proven current store is narrowed and read, an empty one is removed.
func TestRecoveryStoreRepairsOnlyProvenStores(t *testing.T) {
	_, fixture := recoveryFixture(t)
	encode := func(schema int) []byte {
		plan := fixture
		plan.Schema = schema
		body, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	write := func(name string, body []byte, mode os.FileMode) func(*testing.T, string) {
		return func(t *testing.T, dir string) { testkit.WriteFile(t, filepath.Join(dir, name), body, mode) }
	}
	legacy := encode(1)
	refused := []struct {
		name    string
		root    bool // the row relies on permission checks that root bypasses
		prepare func(*testing.T, string)
	}{
		{"directory-0311", true, write("plan.json", legacy, 0644)},
		{"plan-0240", true, write("plan.json", legacy, 0240)},
		{"symlink", false, func(t *testing.T, dir string) {
			write("elsewhere.json", legacy, 0644)(t, filepath.Dir(dir))
			if err := os.Symlink(filepath.Join(filepath.Dir(dir), "elsewhere.json"), filepath.Join(dir, "plan.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory-plan", false, func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "plan.json"), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", false, func(t *testing.T, dir string) {
			if err := syscall.Mkfifo(filepath.Join(dir, "plan.json"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated", false, write("plan.json", legacy[:len(legacy)/2], 0644)},
		{"garbage", false, write("plan.json", []byte("not a plan"), 0644)},
		{"schema-0", false, write("plan.json", encode(0), 0644)},
		{"schema-3", false, write("plan.json", encode(RecoverySchema+1), 0644)},
		{"planless", false, func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "runtime"), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"schema-1", false, write("plan.json", legacy, 0644)},
		{"schema-1-next-only", false, write(".plan-next", legacy, 0644)},
		{"schema-1-trailing", false, write("plan.json", append(slices.Clone(legacy), " {}"...), 0644)},
		{"schema-1-hardlinked", false, func(t *testing.T, dir string) {
			write("plan.json", legacy, 0644)(t, dir)
			if err := os.Link(filepath.Join(dir, "plan.json"), filepath.Join(filepath.Dir(dir), "alias.json")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, row := range refused {
		for _, entry := range []string{"read", "resume"} {
			t.Run(row.name+"/"+entry, func(t *testing.T) {
				if row.root && os.Geteuid() == 0 {
					t.Skip("root bypasses the permission this row relies on")
				}
				store := RecoveryStore{Directory: filepath.Join(testkit.TempDir(t), "data.uninstall"), UID: uint32(os.Getuid())}
				if err := os.Mkdir(store.Directory, 0700); err != nil {
					t.Fatal(err)
				}
				row.prepare(t, store.Directory)
				mode := os.FileMode(0755)
				if row.name == "directory-0311" {
					mode = 0311
				}
				if err := os.Chmod(store.Directory, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(store.Directory, 0700) })
				guarded := []string{store.Directory}
				for _, name := range []string{"plan.json", ".plan-next", "runtime"} {
					if _, err := os.Lstat(filepath.Join(store.Directory, name)); err == nil {
						guarded = append(guarded, filepath.Join(store.Directory, name))
					}
				}
				before := statTimes(t, guarded...)
				var err error
				if entry == "read" {
					_, _, err = store.Read()
				} else {
					_, err = store.ReadForResume(t.Context())
				}
				if err == nil {
					t.Fatal("unproven store was read")
				}
				if after := statTimes(t, guarded...); !reflect.DeepEqual(after, before) {
					t.Fatalf("refusal changed the store (%v): %v, was %v", err, after, before)
				}
			})
		}
	}
	for _, name := range []string{"plan.json", ".plan-next"} {
		for _, entry := range []string{"read", "resume"} {
			t.Run("schema-2-"+name+"/"+entry, func(t *testing.T) {
				store, plan := recoveryFixture(t)
				if _, err := store.Save(plan, nil); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(store.Directory, name)
				if err := os.Rename(filepath.Join(store.Directory, "plan.json"), path); err != nil && name != "plan.json" {
					t.Fatal(err)
				}
				if os.Chmod(path, 0664|os.ModeSetgid) != nil || os.Chmod(store.Directory, 0755) != nil {
					t.Fatal("loosen current store")
				}
				var saved RecoveryPlan
				var err error
				if entry == "read" {
					saved, _, err = store.Read()
				} else {
					saved, err = store.ReadForResume(t.Context())
				}
				directory, _ := os.Lstat(store.Directory)
				file, _ := os.Lstat(path)
				if err != nil || saved.Schema != RecoverySchema || directory.Mode().Perm() != 0700 || file.Mode() != 0600 {
					t.Fatalf("current store was not narrowed and read: %v %v %v", err, directory.Mode(), file.Mode())
				}
			})
		}
	}
	empty := RecoveryStore{Directory: filepath.Join(testkit.TempDir(t), "data.uninstall"), UID: uint32(os.Getuid())}
	if err := os.Mkdir(empty.Directory, 0755); err != nil || os.Chmod(empty.Directory, 0755) != nil {
		t.Fatal("prepare empty store", err)
	}
	if _, err := empty.ReadForResume(t.Context()); !errors.Is(err, ErrNoRecoveryStore) {
		t.Fatalf("empty store: %v", err)
	}
	if _, err := os.Lstat(empty.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty store remains", err)
	}
}

// statTimes records the mode, content and change time that a repair or write would alter.
func statTimes(t *testing.T, paths ...string) []string {
	t.Helper()
	var facts []string
	for _, path := range paths {
		var stat syscall.Stat_t
		if err := syscall.Lstat(path, &stat); err != nil {
			t.Fatal(err)
		}
		facts = append(facts, fmt.Sprintf("%s mode=%o size=%d mtime=%d.%09d ctime=%d.%09d", path, stat.Mode, stat.Size, stat.Mtim.Sec, stat.Mtim.Nsec, stat.Ctim.Sec, stat.Ctim.Nsec))
	}
	return facts
}

// The corpus holds outcomes of `git show v0.30.0:scripts/lib/teardown-plan.py`
// on wire inputs (generator beside it). Engine inputs keep the exit code and
// meaning of successful output. Every released refusal must still refuse.
func TestGuardKeepsReleasedHelperCorpus(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "v0.30.0-guard-corpus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	work := testkit.TempDir(t)
	names := []string{"SUBYARD_TEARDOWN_INVENTORY", "SUBYARD_TEARDOWN_ARTIFACTS", "SUBYARD_TEARDOWN_SERVICES"}
	for _, name := range append(names, "SUBYARD_TEARDOWN_PLAN", "SUBYARD_TEARDOWN_PLAN_SHA256") {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("PATH", testkit.TempDir(t))
	t.Setenv("HOME", work)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	rows := 0
	for ; scanner.Scan(); rows++ {
		var row struct {
			Env     map[string]string
			Engine  bool
			Results []struct {
				Mode           []string
				Exit           int
				Stdout, Stderr string
			}
		}
		if err := json.Unmarshal(bytes.ReplaceAll(scanner.Bytes(), []byte("$W"), []byte(work)), &row); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if value, present := row.Env[name]; present {
				os.Setenv(name, value)
			} else {
				os.Unsetenv(name)
			}
		}
		for _, released := range row.Results {
			var stdout, stderr strings.Builder
			code := RunGuard(released.Mode, nil, &stdout, &stderr)
			if row.Engine && code != released.Exit || released.Exit != 0 && code == 0 {
				t.Errorf("%v with %v (engine %v): released %d %q %q, got %d %q %q", released.Mode, row.Env, row.Engine,
					released.Exit, released.Stdout, released.Stderr, code, stdout.String(), stderr.String())
			}
			if row.Engine && released.Exit == 0 && code == 0 {
				if released.Mode[0] == "producer-artifacts-json" {
					var got, want []ports.TeardownArtifact
					if json.Unmarshal([]byte(stdout.String()), &got) != nil || json.Unmarshal([]byte(released.Stdout), &want) != nil || !reflect.DeepEqual(got, want) {
						t.Errorf("%v: got artifact JSON %q, want equivalent to %q", released.Mode, stdout.String(), released.Stdout)
					}
				} else if !slices.Equal(strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n"), strings.Split(strings.TrimSuffix(released.Stdout, "\n"), "\n")) {
					t.Errorf("%v: got records %q, want %q", released.Mode, stdout.String(), released.Stdout)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil || rows < 150 {
		t.Fatalf("incomplete released corpus: %d rows, %v", rows, err)
	}
}
