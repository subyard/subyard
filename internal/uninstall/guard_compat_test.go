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

// An unreadable plan refuses before repair, since narrowing never makes it
// readable; a readable current plan is still repaired and resumed.
func TestRecoveryStoreReadsPlanBeforeRepair(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads owner-unreadable files")
	}
	store, plan := recoveryFixture(t)
	plan.Schema = 1
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(store.Directory, "plan.json")
	testkit.WriteFile(t, planPath, body, 0240)
	if err := os.Chmod(store.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	before := statTimes(t, store.Directory, planPath)
	if _, _, err := store.Read(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable plan: %v", err)
	}
	if _, err := store.ReadForResume(t.Context()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable plan on resume: %v", err)
	}
	if after := statTimes(t, store.Directory, planPath); !reflect.DeepEqual(after, before) {
		t.Fatalf("unreadable plan changed the store: %v, was %v", after, before)
	}

	store, plan = recoveryFixture(t)
	if _, err := store.Save(plan, nil); err != nil {
		t.Fatal(err)
	}
	planPath = filepath.Join(store.Directory, "plan.json")
	if err := os.Chmod(planPath, 0660); err != nil {
		t.Fatal(err)
	}
	saved, err := store.ReadForResume(t.Context())
	if info, statErr := os.Lstat(planPath); err != nil || statErr != nil || info.Mode().Perm() != 0600 || saved.Schema != RecoverySchema {
		t.Fatalf("readable current plan was not repaired and resumed: %v %v %v", err, statErr, info)
	}
}

// statTimes records the mode and change time that a protected repair would alter.
func statTimes(t *testing.T, paths ...string) []string {
	t.Helper()
	var facts []string
	for _, path := range paths {
		var stat syscall.Stat_t
		if err := syscall.Lstat(path, &stat); err != nil {
			t.Fatal(err)
		}
		facts = append(facts, fmt.Sprintf("%s mode=%o ctime=%d.%09d", path, stat.Mode, stat.Ctim.Sec, stat.Ctim.Nsec))
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
