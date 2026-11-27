package uninstall_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/cli"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testkit/interrupt"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// A failed observation stops store removal without compensation, and the sweep rejects any
// observation after the cut, so Fail mode leaves what a crash at that point would.
func TestStoreRemovalResumesAtEveryInterruption(t *testing.T) {
	t.Setenv("PATH", testkit.TempDir(t))
	type state struct {
		store uninstall.RecoveryStore
		plan  uninstall.RecoveryPlan
	}
	type signature struct {
		Tree  map[string]string
		Epoch string // lives outside the fixture root, in the shared owner-lock directory
	}
	const noPlan = "yard: No saved full uninstall plan. Run 'yard teardown --full' again.\n"
	empty, sawEmpty, resumed := false, false, 0
	points := interrupt.Sweep(t, interrupt.Case[state]{
		Setup: func(t *testing.T) state {
			store, plan := uninstall.CompletedRecoveryStoreFixture(t)
			testkit.WriteFile(t, filepath.Join(store.Directory, ".plan-next"), []byte("stale publication"), 0600)
			return state{store, plan}
		},
		Run: func(t *testing.T, s state, in *interrupt.Injector) interrupt.Outcome {
			var diagnostics bytes.Buffer
			root := filepath.Dir(s.plan.Installation.DataHome)
			program, err := cli.New(cli.Options{
				RepositoryRoot: "../..", WorkingDir: root, Stdout: &diagnostics, Stderr: &diagnostics,
				Environment: []string{"HOME=" + root},
				Arguments:   []string{"_uninstall-resume", s.store.Directory, "--yes"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer uninstall.InterruptRecoveryStoreRemoval(s.store.Directory, in)()
			code := program.Run(t.Context())
			return interrupt.Outcome{Code: code, Output: diagnostics.String()}
		},
		Signature: func(t *testing.T, s state) any {
			epoch, err := uninstall.ReadOwnerEpoch(s.plan.Installation.DataHome)
			return signature{interrupt.Tree(t, filepath.Dir(s.plan.Installation.DataHome)), fmt.Sprint(epoch, err)}
		},
		Completed: func(t *testing.T, s state, o interrupt.Outcome) {
			if o.Code != 0 {
				t.Fatalf("uninterrupted exit=%d output=%s", o.Code, o.Output)
			}
			for _, path := range []string{s.store.Directory, s.plan.Trees[0].Path} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cleanup left %s: %v", path, err)
				}
			}
			if epoch, err := uninstall.ReadOwnerEpoch(s.plan.Installation.DataHome); epoch != 1 || err != nil {
				t.Fatalf("owner epoch=%d, want 1: %v", epoch, err)
			}
		},
		Interrupted: func(t *testing.T, s state, p interrupt.Point, o interrupt.Outcome) {
			directory := s.store.Directory
			launcher := filepath.Join(directory, "runtime", "bin", "yard")
			want := fmt.Sprintf("Full removal is incomplete. Retain %s and resume with:\n  %s _uninstall-resume %s\n", directory, launcher, directory)
			if _, err := os.Lstat(launcher); errors.Is(err, os.ErrNotExist) {
				want = fmt.Sprintf("Full removal is incomplete. Retain %s, install a compatible Subyard version, then resume with:\n  yard _uninstall-resume %s\n", directory, directory)
			} else if err != nil {
				t.Fatalf("%s: inspect retained launcher: %v", p, err)
			}
			if o.Code != 1 || !strings.Contains(o.Output, want) {
				t.Fatalf("%s: interrupted exit=%d, want 1 and retry guidance %q in output %s", p, o.Code, want, o.Output)
			}
			entries, err := os.ReadDir(s.store.Directory)
			if err != nil {
				t.Fatalf("%s: inspect interrupted store: %v", p, err)
			}
			empty = len(entries) == 0
		},
		Resumed: func(t *testing.T, s state, p interrupt.Point, o interrupt.Outcome) {
			resumed++
			if empty {
				sawEmpty = true
				if o.Code != 1 || o.Output != noPlan {
					t.Fatalf("%s: empty-store retry exit=%d output=%q, want %q", p, o.Code, o.Output, noPlan)
				}
			} else if o.Code != 0 {
				t.Fatalf("%s: saved-plan retry exit=%d output=%s", p, o.Code, o.Output)
			}
		},
		Repeated: func(t *testing.T, s state, p interrupt.Point, o interrupt.Outcome) {
			if o.Code != 1 || o.Output != noPlan {
				t.Fatalf("%s: repeated retry exit=%d output=%q, want %q", p, o.Code, o.Output, noPlan)
			}
		},
		MinPoints: 49,
	})
	// Replaying one cut with -run skips the cut that empties the store.
	if resumed == len(points) && !sawEmpty {
		t.Fatal("interruption never left an empty recovery directory")
	}
}

// The CLI renders the schema-1 refusal's own next step and leaves the store as found.
func TestResumeRefusesSchemaOnePlanWithItsNextStep(t *testing.T) {
	t.Setenv("PATH", testkit.TempDir(t))
	store, plan := uninstall.CompletedRecoveryStoreFixture(t)
	path := filepath.Join(store.Directory, "plan.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	released := bytes.Replace(body, []byte(fmt.Sprintf(`"Schema":%d,`, uninstall.RecoverySchema)), []byte(`"Schema":1,`), 1)
	if bytes.Equal(released, body) {
		t.Fatal("fixture plan has no schema field")
	}
	testkit.WriteFile(t, path, released, 0600)
	var diagnostics bytes.Buffer
	root := filepath.Dir(plan.Installation.DataHome)
	program, err := cli.New(cli.Options{
		RepositoryRoot: "../..", WorkingDir: root, Stdout: &diagnostics, Stderr: &diagnostics,
		Environment: []string{"HOME=" + root},
		Arguments:   []string{"_uninstall-resume", store.Directory, "--yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(store.Directory, "runtime", "bin", "yard")
	want := fmt.Sprintf("resume it with its retained release:\n  %s _uninstall-resume %s\n", launcher, store.Directory)
	if code := program.Run(t.Context()); code != 1 || !strings.HasSuffix(diagnostics.String(), want) || strings.Contains(diagnostics.String(), "ORIGINAL") {
		t.Fatalf("exit=%d output=%q, want suffix %q", code, diagnostics.String(), want)
	}
	if saved, err := os.ReadFile(path); err != nil || !bytes.Equal(saved, released) {
		t.Fatal("refused schema-1 plan changed", err)
	}
	if _, err := os.Lstat(launcher); err != nil {
		t.Fatal("retained launcher was removed", err)
	}
}
