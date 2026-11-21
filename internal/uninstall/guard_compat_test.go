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

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The fixture holds digests produced by `git show v0.30.0:scripts/lib/teardown-plan.py`
// (artifact_digest) for escaped names, links and extreme metadata.
func TestGuardDigestsMatchFrozenReleasedHelper(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "v0.30.0-artifact-digests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Entries []Entry
		Binding string
	}
	if err := json.Unmarshal(body, &cases); err != nil || len(cases) != 3 {
		t.Fatal("invalid frozen digest fixture", err)
	}
	for index, fixture := range cases {
		if got, err := digest(fixture.Entries); err != nil || got != fixture.Binding {
			t.Fatalf("case %d: digest %q, released %q (%v)", index, got, fixture.Binding, err)
		}
	}
}

func TestRecoverySchemaAcceptsReleasedAndCurrentOnly(t *testing.T) {
	_, plan := recoveryFixture(t)
	for schema, accepted := range map[int]bool{0: false, 1: true, RecoverySchema: true, RecoverySchema + 1: false} {
		plan.Schema = schema
		if err := plan.Validate(plan.Installation.OperatorUID); (err == nil) != accepted {
			t.Fatalf("schema %d: %v", schema, err)
		}
	}
}

// Schema 1 facts keep their drift guards under the Go guard: unchanged approved
// facts are accepted, replacement, symbolic links and additions refuse. The
// full-uninstall lane resumes an actual v0.30.0-written checkpoint end to end.
func TestSchemaOneFactsKeepDriftGuards(t *testing.T) {
	for _, drift := range []string{"unchanged", "replaced", "symlinked", "foreign"} {
		t.Run(drift, func(t *testing.T) {
			store, plan := recoveryFixture(t)
			owned := filepath.Join(store.Directory, "owned")
			if err := os.MkdirAll(filepath.Join(owned, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(owned, "nested", "data"), []byte("approved"), 0600)
			tree, err := Capture(owned)
			if err != nil {
				t.Fatal(err)
			}
			artifact, err := CaptureArtifact(filepath.Join(owned, "nested", "data"))
			if err != nil {
				t.Fatal(err)
			}
			plan.Schema, plan.Trees = 1, []Tree{tree}
			plan.Services = []RecoveryService{{Scope: "user", Units: []string{"subyard-fixture.service"}, Artifacts: []ports.TeardownArtifact{artifact}}}
			body, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(store.Directory, "plan.json"), body, 0600)
			saved, _, err := store.Read()
			if err != nil || saved.Schema != 1 {
				t.Fatalf("released store was not resumable: %v", err)
			}
			switch drift {
			case "replaced":
				testkit.WriteFile(t, filepath.Join(owned, "nested", "data.next"), []byte("approved"), 0600)
				if err := os.Rename(filepath.Join(owned, "nested", "data.next"), filepath.Join(owned, "nested", "data")); err != nil {
					t.Fatal(err)
				}
			case "symlinked":
				if err := os.Rename(filepath.Join(owned, "nested"), filepath.Join(store.Directory, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(store.Directory, "moved"), filepath.Join(owned, "nested")); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				testkit.WriteFile(t, filepath.Join(owned, "nested", "foreign"), []byte("foreign"), 0600)
			}
			approved := saved.Services[0].Artifacts[0]
			guardErr := classify("guard-artifact", guardArtifact(approved.Path, approved.Binding, approved.Entries, nil))
			treeErr := saved.Trees[0].Remove()
			if drift == "unchanged" {
				if guardErr != nil || treeErr != nil {
					t.Fatalf("unchanged released facts refused: %v %v", guardErr, treeErr)
				}
				if _, err := os.Lstat(owned); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("approved released tree remains", err)
				}
				return
			}
			if drift != "foreign" && !errors.Is(guardErr, domain.ErrPlanStale) || !errors.Is(treeErr, domain.ErrPlanStale) {
				t.Fatalf("%s accepted: artifact=%v tree=%v", drift, guardErr, treeErr)
			}
			if _, err := os.Lstat(filepath.Join(owned, "nested")); err != nil {
				t.Fatal("drifted released tree was deleted", err)
			}
		})
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
