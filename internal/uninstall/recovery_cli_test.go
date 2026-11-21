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
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestStoreRemovalResumesAtEveryInterruption(t *testing.T) {
	t.Setenv("PATH", testkit.TempDir(t))
	sawEmpty := false
	for limit := 0; limit <= 1000; limit++ {
		store, plan := uninstall.CompletedRecoveryStoreFixture(t)
		testkit.WriteFile(t, filepath.Join(store.Directory, ".plan-next"), []byte("stale publication"), 0600)
		var diagnostics bytes.Buffer
		resume := func() *cli.CLI {
			t.Helper()
			root := filepath.Dir(plan.Installation.DataHome)
			program, err := cli.New(cli.Options{
				RepositoryRoot: "../..", WorkingDir: root, Stdout: &diagnostics, Stderr: &diagnostics,
				Environment: []string{"HOME=" + root},
				Arguments:   []string{"_uninstall-resume", store.Directory, "--yes"},
			})
			if err != nil {
				t.Fatal(err)
			}
			return program
		}
		program := resume()
		finish := uninstall.InterruptRecoveryStoreRemoval(store.Directory, limit)
		code := program.Run(t.Context())
		interrupted := finish()
		if interrupted {
			if code != 1 || !strings.Contains(diagnostics.String(), "_uninstall-resume "+store.Directory) {
				t.Fatalf("observation %d: interrupted exit=%d output=%s", limit, code, &diagnostics)
			}
			launcher := filepath.Join(store.Directory, "runtime", "bin", "yard")
			if _, err := os.Lstat(launcher); errors.Is(err, os.ErrNotExist) {
				want := fmt.Sprintf("Full removal is incomplete. Retain %s, install a compatible Subyard version, then resume with:\n  yard _uninstall-resume %s\n", store.Directory, store.Directory)
				if !strings.Contains(diagnostics.String(), want) {
					t.Fatalf("observation %d: missing retry guidance %q in output %s", limit, want, &diagnostics)
				}
			} else if err != nil {
				t.Fatalf("observation %d: inspect retained launcher: %v", limit, err)
			}
			entries, err := os.ReadDir(store.Directory)
			if err != nil {
				t.Fatalf("observation %d: inspect interrupted store: %v", limit, err)
			}
			diagnostics.Reset()
			code = resume().Run(t.Context())
			if len(entries) == 0 {
				sawEmpty = true
				if code != 1 || !strings.Contains(diagnostics.String(), "Run 'yard teardown --full' again.") {
					t.Fatalf("observation %d: empty-store retry exit=%d output=%s", limit, code, &diagnostics)
				}
			} else if code != 0 {
				t.Fatalf("observation %d: saved-plan retry exit=%d output=%s", limit, code, &diagnostics)
			}
		} else if code != 0 {
			t.Fatalf("observation %d: uninterrupted exit=%d output=%s", limit, code, &diagnostics)
		}
		for _, path := range []string{store.Directory, plan.Trees[0].Path} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("observation %d: cleanup left %s: %v", limit, path, err)
			}
		}
		if epoch, err := uninstall.ReadOwnerEpoch(plan.Installation.DataHome); epoch != 1 || err != nil {
			t.Fatalf("observation %d: owner epoch=%d, want 1: %v", limit, epoch, err)
		}
		if !interrupted {
			if !sawEmpty {
				t.Fatal("interruption never left an empty recovery directory")
			}
			t.Logf("verified %d interruption points", limit)
			return
		}
	}
	t.Fatal("removal never completed")
}
