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
	empty, sawEmpty := false, false
	interrupt.Sweep(t, interrupt.Case[state]{
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
		Signature: func(t *testing.T, s state) any { return interrupt.Tree(t, filepath.Dir(s.plan.Installation.DataHome)) },
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
			if o.Code != 1 || !strings.Contains(o.Output, "_uninstall-resume "+s.store.Directory) {
				t.Fatalf("%s: interrupted exit=%d output=%s", p, o.Code, o.Output)
			}
			launcher := filepath.Join(s.store.Directory, "runtime", "bin", "yard")
			if _, err := os.Lstat(launcher); errors.Is(err, os.ErrNotExist) {
				want := fmt.Sprintf("Full removal is incomplete. Retain %s, install a compatible Subyard version, then resume with:\n  yard _uninstall-resume %s\n", s.store.Directory, s.store.Directory)
				if !strings.Contains(o.Output, want) {
					t.Fatalf("%s: missing retry guidance %q in output %s", p, want, o.Output)
				}
			} else if err != nil {
				t.Fatalf("%s: inspect retained launcher: %v", p, err)
			}
			entries, err := os.ReadDir(s.store.Directory)
			if err != nil {
				t.Fatalf("%s: inspect interrupted store: %v", p, err)
			}
			empty = len(entries) == 0
		},
		Resumed: func(t *testing.T, s state, p interrupt.Point, o interrupt.Outcome) {
			if empty {
				sawEmpty = true
				if o.Code != 1 || !strings.Contains(o.Output, "Run 'yard teardown --full' again.") {
					t.Fatalf("%s: empty-store retry exit=%d output=%s", p, o.Code, o.Output)
				}
			} else if o.Code != 0 {
				t.Fatalf("%s: saved-plan retry exit=%d output=%s", p, o.Code, o.Output)
			}
		},
		Repeated: func(t *testing.T, s state, p interrupt.Point, o interrupt.Outcome) {
			if want := "yard: No saved full uninstall plan. Run 'yard teardown --full' again.\n"; o.Code != 1 || o.Output != want {
				t.Fatalf("%s: repeated retry exit=%d output=%q, want %q", p, o.Code, o.Output, want)
			}
		},
		MinPoints: 49,
	})
	if !sawEmpty {
		t.Fatal("interruption never left an empty recovery directory")
	}
}
