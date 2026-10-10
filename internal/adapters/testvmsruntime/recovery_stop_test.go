package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestForceStopRecoveryRequiresDurableEvidenceAndFreshOwnership(t *testing.T) {
	for _, scenario := range []string{
		"success", "concurrent stop", "legacy", "identity mismatch", "not recovering", "cancelled", "no environment", "no allocation",
		"state unavailable", "unknown state", "incident unavailable", "event unavailable",
		"project changed", "generation changed", "epoch changed", "state changed", "cancelled after diagnostics",
		"force failed", "verification unavailable", "still running",
	} {
		t.Run(scenario, func(t *testing.T) {
			cfg := fixtureConfig(t)
			spec := EnvironmentSpec{Name: EnvironmentPair, Count: 1, CPU: 1, Memory: "512MiB", Disk: "10GiB", Lifecycle: DisposableLifecycle}
			slot := LeaseSlot{SlotID: "slot-001", ResourceGeneration: 7, LeaseEpoch: 3, State: SlotRecovering, Environment: &spec,
				IncidentID: "00000000000000000001-0000000000000001"}
			rt := &Runtime{Config: cfg, recoverySlot: &slot, allocation: &LeaseIdentity{SlotID: slot.SlotID, ResourceGeneration: 7, LeaseEpoch: 3}}
			rt.prepareDefaults()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "legacy":
				slot.LegacyRetained = true
			case "identity mismatch":
				rt.allocation.LeaseEpoch++
			case "not recovering":
				slot.State = SlotDraining
			case "cancelled":
				cancel()
			case "no environment":
				slot.Environment = nil
			case "no allocation":
				rt.allocation = nil
			case "incident unavailable", "event unavailable":
				path := rt.eventRecorder().incidentDirectory()
				if scenario == "event unavailable" {
					path = rt.eventRecorder().eventDirectory()
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("blocked"), 0o600)
			}
			stateReads, forced := 0, false
			runner := &fakeRunner{}
			runner.handler = func(name string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				joined := strings.Join(args, " ")
				if name == "journalctl" {
					if scenario == "cancelled after diagnostics" {
						cancel()
					}
					return []byte("journal"), nil, nil
				}
				switch {
				case strings.HasPrefix(joined, "list e2e-vm-1 "):
					stateReads++
					if scenario == "concurrent stop" {
						return []byte("STOPPED\n"), nil, nil
					}
					if scenario == "state unavailable" || (forced && scenario == "verification unavailable") {
						return nil, nil, errors.New("state probe unavailable")
					}
					if scenario == "unknown state" || (scenario == "state changed" && stateReads >= 3) {
						return []byte("FROZEN\n"), nil, nil
					}
					if forced && scenario != "still running" && scenario != "force failed" {
						return []byte("STOPPED\n"), nil, nil
					}
					return []byte("RUNNING\n"), nil, nil
				case strings.HasPrefix(joined, "project get "):
					if scenario == "project changed" {
						return []byte("foreign"), nil, nil
					}
					return []byte(managedMarker), nil, nil
				case strings.HasPrefix(joined, "config get "):
					value := managedMarker
					if args[3] == "user.subyard.generation" {
						value = strconv.Itoa(7)
						if scenario == "generation changed" {
							value = "8"
						}
					}
					if args[3] == "user.subyard.lease-epoch" {
						value = "3"
						if scenario == "epoch changed" {
							value = "4"
						}
					}
					return []byte(value), nil, nil
				case strings.HasPrefix(joined, "stop ") && strings.Contains(joined, "--force"):
					forced = true
					batch, err := rt.eventRecorder().Export()
					if err != nil || len(batch.Incidents) != 1 || len(batch.Events) != 1 || batch.Events[0].Kind != "vm.force_stop_planned" {
						t.Fatalf("force began without durable artifact/event: %#v, %v", batch, err)
					}
					incident := batch.Incidents[0]
					if incident.Diagnostics["original_incident_id"] != slot.IncidentID ||
						!strings.Contains(incident.Diagnostics["vm_1_console_log"], "console evidence") ||
						strings.Contains(incident.Diagnostics["vm_1_console_log"], "synthetic-secret") ||
						!strings.Contains(incident.Diagnostics["vm_1_qmp_log"], "QMP evidence") ||
						strings.Contains(incident.Diagnostics["vm_1_qmp_log"], "synthetic-secret") ||
						incident.Command == nil || incident.Command.DurationMS != 60000 {
						t.Fatalf("pre-force evidence missing or unredacted: %#v", incident)
					}
					deadline, ok := runner.contexts[len(runner.contexts)-1].Deadline()
					if !ok || time.Until(deadline) > 30*time.Second {
						t.Fatal("force command lacks bounded deadline")
					}
					if scenario == "force failed" {
						return nil, nil, errors.New("force operation failed")
					}
					return nil, nil, nil
				case strings.HasPrefix(joined, "console "):
					return []byte("console evidence token=synthetic-secret"), nil, nil
				case strings.HasPrefix(joined, "query /1.0/instances/"):
					return []byte("QMP evidence token=synthetic-secret"), nil, nil
				default:
					return []byte("native evidence"), nil, nil
				}
			}
			rt.Runner = runner
			cause := &CommandError{Name: "incus", Args: []string{"stop", "e2e-vm-1", "--timeout", "60"}, ExitCode: 1,
				Duration: time.Minute, Message: "context deadline exceeded"}
			err := rt.forceStopRecoveryVM(ctx, "e2e-vm-1", cause)
			wantForce := scenario == "success" || scenario == "force failed" || scenario == "verification unavailable" || scenario == "still running"
			if forced != wantForce {
				t.Fatalf("forced=%v, want %v; error=%v", forced, wantForce, err)
			}
			wantSuccess := scenario == "success" || scenario == "concurrent stop"
			if (err == nil) != wantSuccess {
				t.Fatalf("error=%v, want success=%v", err, wantSuccess)
			}
			if forced {
				batch, exportErr := rt.eventRecorder().Export()
				if exportErr != nil || len(batch.Events) != 2 {
					t.Fatalf("force result missing: %#v, %v", batch, exportErr)
				}
				wantKind := "vm.force_stop_failed"
				if wantSuccess {
					wantKind = "vm.force_stop_succeeded"
				}
				if batch.Events[1].Kind != wantKind {
					t.Fatalf("result kind=%s, want %s", batch.Events[1].Kind, wantKind)
				}
			}
		})
	}
}

func TestRecoveryDiagnosticsBoundsAndReportsMissingNativeEvidence(t *testing.T) {
	runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "console" {
			return nil, nil, errors.New("console log unavailable")
		}
		if len(args) == 2 && args[0] == "query" {
			if args[1] == "/1.0/instances/e2e-vm-1/logs/qemu.qmp.log?project=subyard-e2e-vms" {
				return []byte(`{"type":"error","error":"synthetic-missing-log","error_code":404}`), nil,
					&CommandError{Name: "incus", Args: args, ExitCode: 1, Message: "QMP log unavailable"}
			}
		}
		return nil, nil, nil
	}}
	rt := &Runtime{Config: fixtureConfig(t), Runner: runner}
	diagnostics := rt.recoveryDiagnostics(context.Background(), "")
	for selector := 1; selector <= 2; selector++ {
		if diagnostics[fmt.Sprintf("vm_%d_state", selector)] != "empty native response (no measurement)" ||
			!strings.Contains(diagnostics[fmt.Sprintf("vm_%d_console_log", selector)], "unavailable:") {
			t.Fatalf("missing evidence silently omitted: %#v", diagnostics)
		}
	}
	if !strings.Contains(diagnostics["vm_1_qmp_log"], "unavailable:") ||
		strings.Contains(diagnostics["vm_1_qmp_log"], "synthetic-missing-log") ||
		diagnostics["vm_2_qmp_log"] != "empty native response (no measurement)" {
		t.Fatalf("missing QMP evidence silently omitted: %#v", diagnostics)
	}
	for index, ctx := range runner.contexts {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatalf("diagnostic probe %d lacks bounded deadline", index)
		}
	}
	if !strings.HasPrefix(runner.calls[0][1], "list") || runner.calls[2][1] != "console" ||
		strings.Join(runner.calls[3][1:], " ") != "query /1.0/instances/e2e-vm-1/logs/qemu.qmp.log?project=subyard-e2e-vms" {
		t.Fatalf("native state/console/QMP did not precede project/journal: %#v", runner.calls)
	}
}

func TestRecoveryDiagnosticsKeepsFailedGuestEvidenceWhenPeerProbesStall(t *testing.T) {
	runner := &fakeRunner{}
	runner.handler = func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(args, " "), "e2e-vm-1") {
			ctx := runner.contexts[len(runner.contexts)-1]
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return []byte("failed guest evidence"), nil, nil
	}
	rt := &Runtime{Config: fixtureConfig(t), Runner: runner}
	diagnostics := rt.recoveryDiagnostics(context.Background(), "e2e-vm-2")
	if diagnostics["vm_2_console_log"] != "failed guest evidence" ||
		diagnostics["vm_2_qmp_log"] != "failed guest evidence" ||
		!strings.Contains(diagnostics["vm_1_qmp_log"], "unavailable:") ||
		!strings.Contains(diagnostics["vm_1_console_log"], "unavailable:") {
		t.Fatalf("failed guest console was lost behind stalled peer probes: %#v", diagnostics)
	}
}
