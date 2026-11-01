package testvms_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type sourceWorkerStatusExecutor struct {
	probe, status                []byte
	probeExit, statusExit, calls int
}

func (p *sourceWorkerStatusExecutor) Execute(_ context.Context, _ domain.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	p.calls++
	if p.calls == 1 && len(request.Command) == 5 && request.Command[0] == "sh" && request.Command[4] == testvmsruntime.DefaultInstalledPath {
		return ports.InstanceExecResult{Stdout: p.probe, ExitCode: p.probeExit}, nil
	}
	if p.calls == 2 && len(request.Command) == 6 && request.Command[0] == "sh" && request.Command[4] == testvmsruntime.DefaultInstalledPath && request.Command[5] == strings.TrimSuffix(strings.TrimPrefix(string(p.probe), "shell "), "\n") {
		return ports.InstanceExecResult{Stdout: p.status, ExitCode: p.statusExit}, nil
	}
	return ports.InstanceExecResult{}, errors.New("unexpected worker command")
}
func (*sourceWorkerStatusExecutor) Stream(context.Context, domain.Context, ports.InstanceExecRequest, io.Reader) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{}, errors.New("unexpected stream")
}

func TestOriginalSourceWorkerStatusKeepsProtocolAndLeaseBoundaries(t *testing.T) {
	for _, scenario := range []string{"down", "expired", "active", "unknown-output", "modern-retained-source", "modern-missing-source", "no-source", "foreign-hash", "unknown-worker", "missing-source", "source-symlink", "probe-refusal", "replacement", "worker-error", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "scripts/e2e-lab/worker.sh")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			worker := []byte("#!/bin/sh\ncase ${1-} in status) printf 'test-vms: down\\n';; *) exit 1;; esac\n")
			testkit.WriteFile(t, path, worker, 0755)
			probe := &sourceWorkerStatusExecutor{probe: []byte(fmt.Sprintf("shell %x\n", sha256.Sum256(worker))), status: []byte("test-vms: down\n")}
			wantError, wantLegacy, wantCalls, inactive := false, true, 2, true
			switch scenario {
			case "expired":
				probe.status = []byte("ttl_remaining_seconds\t0\n")
			case "active":
				probe.status, inactive = []byte("ttl_remaining_seconds\t60\n"), false
			case "unknown-output":
				probe.status, inactive = []byte("unknown\n"), false
			case "modern-retained-source", "modern-missing-source":
				probe.probe, wantLegacy, wantCalls = []byte("elf\n"), false, 1
				if scenario == "modern-missing-source" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			case "no-source":
				root, wantLegacy, wantCalls = "", false, 0
			case "foreign-hash":
				probe.probe, wantError, wantCalls = []byte("shell "+strings.Repeat("0", 64)+"\n"), true, 1
			case "unknown-worker":
				probe.probe, wantError, wantCalls = []byte("unknown\n"), true, 1
			case "missing-source":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				wantError, wantCalls = true, 1
			case "source-symlink":
				other := filepath.Join(root, "other-worker")
				testkit.WriteFile(t, other, worker, 0755)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
				wantError, wantCalls = true, 1
			case "probe-refusal":
				probe.probeExit, wantError, wantCalls = 1, true, 1
			case "replacement", "worker-error":
				probe.statusExit, wantError = 1, true
			case "oversize":
				probe.status, wantError = bytes.Repeat([]byte("x"), (64<<10)+1), true
			}
			payload, legacy, err := testvmsruntime.ReadSourceWorkerStatus(context.Background(), root, domain.Context{}, probe)
			if (err != nil) != wantError || probe.calls != wantCalls {
				t.Fatalf("worker result error=%t calls=%d", err != nil, probe.calls)
			}
			if wantError {
				if legacy || len(payload) != 0 {
					t.Fatal("refused worker returned authority")
				}
				return
			}
			if legacy != wantLegacy {
				t.Fatal("worker protocol selection changed")
			}
			if legacy && ((testyardmigration.VerifyNoActiveOwnerLease(payload) == nil) != inactive) {
				t.Fatal("original lease decoder boundary changed")
			}
		})
	}
}
