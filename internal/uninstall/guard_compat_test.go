package uninstall

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

// A schema-1 plan saved by v0.30.0 or older refuses before any resume step and
// names its retained release, or v0.32.0 once that launcher is gone.
func TestRecoveryStoreRefusesSchemaOneWithNextStep(t *testing.T) {
	store, plan := recoveryFixture(t)
	plan.Schema = 1
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(store.Directory, "plan.json"), body, 0600)
	launcher := filepath.Join(store.Directory, "runtime", "bin", "yard")
	for _, retained := range []bool{false, true} {
		if retained {
			if err := os.MkdirAll(filepath.Dir(launcher), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, launcher, []byte("retained release"), 0700)
		}
		next := "install Subyard 0.32.0 (https://github.com/Subyard/Subyard/releases/tag/v0.32.0), the last release that resumes schema 1 plans, then run:\n  yard _uninstall-resume " + store.Directory
		if retained {
			next = "resume it with its retained release:\n  " + launcher + " _uninstall-resume " + store.Directory
		}
		for name, read := range map[string]func() error{
			"read":   func() error { _, _, err := store.Read(); return err },
			"resume": func() error { _, err := store.ReadForResume(t.Context()); return err },
		} {
			err := read()
			var refusal LegacyRecoverySchemaError
			if !errors.As(err, &refusal) || !strings.HasSuffix(err.Error(), next) || !strings.Contains(err.Error(), "Retain "+store.Directory) {
				t.Fatalf("retained=%v %s: %v", retained, name, err)
			}
		}
	}
	if saved, err := os.ReadFile(filepath.Join(store.Directory, "plan.json")); err != nil || !bytes.Equal(saved, body) {
		t.Fatal("refused schema-1 plan changed", err)
	}
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
