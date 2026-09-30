package reconcileruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type previewFileExecutor struct {
	helper string
	owner  string
}

func (fixture previewFileExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := append([]string(nil), request.Command...)
	command[3] = strings.NewReplacer("/usr/local/bin/subyard-preview", fixture.helper,
		"regular file|755|0:0", "regular file|755|"+fixture.owner).Replace(command[3])
	output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if err != nil {
		result.ExitCode = 1
	}
	return result, err
}

func TestPreviewConvergenceChecksHelperBytesPermissionsAndOwnership(t *testing.T) {
	for _, scenario := range []string{"ready", "missing", "stale", "permissions", "owner", "symlink", "directory"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			source := filepath.Join(root, "config", "preview", "subyard-preview")
			if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
				t.Fatal(err)
			}
			payload := []byte("#!/usr/bin/python3\nprint('preview')\n")
			testkit.WriteFile(t, source, payload, 0o755)
			helper := filepath.Join(root, "installed-preview")
			owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
			switch scenario {
			case "missing":
			case "directory":
				if err := os.Mkdir(helper, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(source, helper); err != nil {
					t.Fatal(err)
				}
			default:
				mode := os.FileMode(0o755)
				if scenario == "stale" {
					payload = []byte("stale")
				}
				if scenario == "permissions" {
					mode = 0o777
				}
				if scenario == "owner" {
					owner = "4294967294:4294967294"
				}
				testkit.WriteFile(t, helper, payload, mode)
			}
			runtime := Runtime{RepositoryRoot: root, Executor: previewFileExecutor{helper, owner}}
			ready, err := runtime.previewConverged(context.Background())
			if err != nil || ready != (scenario == "ready") {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestPreviewConvergenceRequiresRegularSource(t *testing.T) {
	root := testkit.TempDir(t)
	runtime := Runtime{RepositoryRoot: root, Executor: runningIncus(0)}
	if _, err := runtime.previewSourceHash(); err == nil {
		t.Fatal("missing helper source accepted")
	}
	source := filepath.Join(root, "config", "preview", "subyard-preview")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	testkit.WriteFile(t, target, []byte("fixture"), 0o755)
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.previewConverged(context.Background()); err == nil {
		t.Fatal("symlinked helper source accepted")
	}
}
