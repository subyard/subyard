package testvmsruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestHungGuestCleanupLeavesContextForNativeStop(t *testing.T) {
	for _, scenario := range []string{"release keys", "recovery lease context", "cancelled recovery"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			rt, runner := cleanupDeadlineFixture(t)
			if scenario != "release keys" {
				rt.recoverySlot = &LeaseSlot{SlotID: "slot-001", ResourceGeneration: 7, LeaseEpoch: 3,
					State: SlotRecovering, Environment: rt.Config.Environment}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hung, graceful, forced, stopped := false, false, false, false
			runner.handler = func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				callCtx := runner.contexts[len(runner.contexts)-1]
				joined := strings.Join(args, " ")
				if args[0] == "exec" {
					stall := scenario != "recovery lease context" || strings.Contains(joined, "/run/subyard-e2e-lease.json")
					if stall && !hung {
						hung = true
						deadline, ok := callCtx.Deadline()
						if !ok || time.Until(deadline) > 5*time.Second {
							t.Fatal("guest cleanup lacks its local five-second deadline")
						}
						if scenario == "cancelled recovery" {
							cancel()
						}
						<-callCtx.Done()
						return nil, nil, callCtx.Err()
					}
				}
				if err := callCtx.Err(); err != nil {
					return nil, nil, err
				}
				switch {
				case strings.HasPrefix(joined, "stop "):
					if strings.Contains(joined, "--force") {
						forced, stopped = true, true
						return nil, nil, nil
					}
					graceful = true
					if scenario == "recovery lease context" {
						return nil, nil, errors.New("graceful stop timed out")
					}
					stopped = true
					return nil, nil, nil
				case strings.HasPrefix(joined, "list e2e-vm-1 "):
					if stopped {
						return []byte("STOPPED\n"), nil, nil
					}
					return []byte("RUNNING\n"), nil, nil
				default:
					return cleanupDeadlineInventory(rt, args), nil, nil
				}
			}
			evidence, err := rt.stopRetainedWithEvidence(ctx)
			if !hung {
				t.Fatal("fixture did not stall guest cleanup")
			}
			if scenario == "cancelled recovery" {
				if err == nil || forced || graceful || stopped {
					t.Fatalf("parent cancellation was bypassed: error=%v graceful=%v forced=%v stopped=%v", err, graceful, forced, stopped)
				}
				return
			}
			if err != nil || ctx.Err() != nil || !graceful || !stopped || forced != (scenario == "recovery lease context") {
				t.Fatalf("hung cleanup blocked native stop: error=%v parent=%v graceful=%v forced=%v stopped=%v", err, ctx.Err(), graceful, forced, stopped)
			}
			if evidence.guestKeyCleanupAttempts != 1 || evidence.guestKeyCleanupDeferred != 1 {
				t.Fatalf("cleanup timeout was not retained in stop evidence: %#v", evidence)
			}
		})
	}
}

func TestQuarantineGuestCleanupDeadlineLeavesContextForDiagnostics(t *testing.T) {
	rt, runner := cleanupDeadlineFixture(t)
	hung := false
	runner.handler = func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		ctx := runner.contexts[len(runner.contexts)-1]
		if args[0] == "exec" {
			hung = true
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				t.Fatal("quarantine cleanup lacks its local five-second deadline")
			}
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		if strings.HasPrefix(strings.Join(args, " "), "list e2e-vm-1 ") {
			return []byte("RUNNING\n"), nil, nil
		}
		return cleanupDeadlineInventory(rt, args), nil, nil
	}
	ctx := context.Background()
	if err := rt.removeQuarantinedGuestKeys(ctx); !hung || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung quarantine cleanup did not finish at its deadline: hung=%v error=%v", hung, err)
	}
	if ctx.Err() != nil || rt.recoveryDiagnostics(ctx, "")["vm_1_state"] != "RUNNING\n" {
		t.Fatal("quarantine cleanup timeout consumed subsequent diagnostics context")
	}
}

func cleanupDeadlineFixture(t *testing.T) (*Runtime, *fakeRunner) {
	t.Helper()
	cfg := fixtureConfig(t)
	cfg.AgentPublicKey = ""
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, cfg.keyPath()+".pub", []byte(fixturePublicKey(t)+"\n"), 0o600)
	runner := &fakeRunner{}
	rt := &Runtime{Config: cfg, Runner: runner}
	spec := EnvironmentSpec{Name: EnvironmentPair, Count: 1, CPU: 1, Memory: "512MiB", Disk: "10GiB", Lifecycle: DisposableLifecycle}
	rt.useLeaseSlot(LeaseSlot{SlotID: "slot-001", ResourceGeneration: 7, LeaseEpoch: 3, Environment: &spec})
	rt.prepareDefaults()
	return rt, runner
}

func cleanupDeadlineInventory(rt *Runtime, args []string) []byte {
	joined := strings.Join(args, " ")
	switch {
	case joined == "project list --format csv -c n":
		return []byte(rt.Config.Project + "\n")
	case strings.HasPrefix(joined, "list --project "):
		return []byte("e2e-vm-1\n")
	case strings.HasPrefix(joined, "config get ") && args[3] == "user.subyard.generation":
		return []byte("7\n")
	case strings.HasPrefix(joined, "config get ") && args[3] == "user.subyard.lease-epoch":
		return []byte("3\n")
	default:
		return []byte(managedMarker + "\n")
	}
}
