package reconcileruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/testkit"
)

type previewFileExecutor struct {
	helper, endpoint           string
	helperOwner, endpointOwner string
}

func (fixture previewFileExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := append([]string(nil), request.Command...)
	command[3] = strings.NewReplacer("/usr/local/bin/subyard-preview", fixture.helper,
		"/etc/subyard/preview.json", fixture.endpoint,
		"regular file|755|0:0", "regular file|755|"+fixture.helperOwner,
		"regular file|644|0:0", "regular file|644|"+fixture.endpointOwner).Replace(command[3])
	output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if err != nil {
		result.ExitCode = 1
	}
	return result, err
}

func TestPreviewConvergenceChecksHelperAndEndpointBytesPermissionsAndOwnership(t *testing.T) {
	for _, scenario := range []string{
		"ready", "helper missing", "helper stale", "helper permissions", "helper owner", "helper symlink", "helper directory",
		"endpoint missing", "endpoint stale", "endpoint permissions", "endpoint owner", "endpoint symlink", "endpoint directory",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			source := filepath.Join(root, "config", "preview", "subyard-preview")
			if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
				t.Fatal(err)
			}
			payload := []byte("#!/usr/bin/python3\nprint('preview')\n")
			testkit.WriteFile(t, source, payload, 0o755)
			helper := filepath.Join(root, "installed-preview")
			endpoint := filepath.Join(root, "preview.json")
			owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
			fixture := previewFileExecutor{helper: helper, endpoint: endpoint, helperOwner: owner, endpointOwner: owner}
			runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{YardKind: domain.YardVM}, Executor: fixture}
			target, mode := helper, os.FileMode(0o755)
			component, fault, _ := strings.Cut(scenario, " ")
			if component == "endpoint" {
				testkit.WriteFile(t, helper, payload, 0o755)
				target, mode, payload = endpoint, 0o644, runtime.previewEndpoint(context.Background())
			} else {
				testkit.WriteFile(t, endpoint, runtime.previewEndpoint(context.Background()), 0o644)
			}
			switch fault {
			case "missing":
			case "directory":
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(source, target); err != nil {
					t.Fatal(err)
				}
			default:
				if fault == "stale" {
					payload = []byte("stale")
				}
				if fault == "permissions" {
					mode = 0o777
				}
				if fault == "owner" {
					if component == "endpoint" {
						fixture.endpointOwner = "4294967294:4294967294"
					} else {
						fixture.helperOwner = "4294967294:4294967294"
					}
				}
				testkit.WriteFile(t, target, payload, mode)
			}
			runtime.Executor = fixture
			ready, err := runtime.previewConverged(context.Background())
			if err != nil || ready != (scenario == "ready") {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestPreviewRouteConvergenceWithoutOwnerTailAddress(t *testing.T) {
	bin := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin)
	for _, kind := range []domain.YardKind{domain.YardContainer, domain.YardVM} {
		runtime := Runtime{Yard: domain.Context{YardKind: kind}, Environment: []string{"WEB_PREVIEW_HOST_PORT=32222"}}
		for _, scenario := range []string{"absent", "receipt", "device", "local device", "receipt and device"} {
			t.Run(string(kind)+"/"+scenario, func(t *testing.T) {
				instance := ports.InstanceInfo{LocalConfig: map[string]string{}, Devices: map[string]map[string]string{}}
				if strings.Contains(scenario, "receipt") {
					instance.LocalConfig[previewroute.Key] = "v1:100.101.102.103:32222"
				}
				if scenario == "local device" {
					instance.LocalDevices = map[string]map[string]string{previewroute.DeviceName: previewroute.Device("100.101.102.103", "32222")}
				} else if strings.Contains(scenario, "device") {
					instance.Devices[previewroute.DeviceName] = previewroute.Device("100.101.102.103", "32222")
				}
				if ready := runtime.previewRouteConverged(context.Background(), instance); ready != (scenario == "absent") {
					t.Fatalf("route converged = %v", ready)
				}
			})
		}
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
