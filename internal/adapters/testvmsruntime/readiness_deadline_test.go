package testvmsruntime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestWaitForRejectsLateProbeSuccess(t *testing.T) {
	now := time.Unix(0, 0)
	rt := &Runtime{Config: Config{BootTimeout: time.Second}, Stdout: io.Discard, Now: func() time.Time { return now }}
	err := rt.waitFor(context.Background(), "diagnostic", func(context.Context) error {
		now = now.Add(2 * time.Second)
		return nil
	})
	if err == nil {
		t.Fatal("readiness accepted after 2s despite 1s BootTimeout")
	}
}

type blockedReadinessRunner struct{}

func (blockedReadinessRunner) LookPath(name string) (string, error) { return name, nil }
func (blockedReadinessRunner) Run(ctx context.Context, _ string, _ []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
	<-ctx.Done()
	return nil, nil, context.Cause(ctx)
}
func TestAgentReadinessBoundsBlockedProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rt := &Runtime{Config: Config{BootTimeout: 20 * time.Millisecond}, Runner: blockedReadinessRunner{}, Stdout: io.Discard, Now: time.Now, Sleep: sleepContext}
	started := time.Now()
	err := rt.waitAgent(ctx, "diagnostic-vm")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("blocked agent probe unexpectedly succeeded")
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("agent probe used outer deadline: elapsed=%s boot_timeout=20ms", elapsed)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("unexpected cancellation")
	}
}

func TestWaitForPreservesFailedProbeOnSleepDeadline(t *testing.T) {
	rt := &Runtime{Config: Config{BootTimeout: 20 * time.Millisecond}, Stdout: io.Discard, Now: time.Now, Sleep: sleepContext}
	err := rt.waitFor(context.Background(), "diagnostic-agent", func(context.Context) error { return errors.New("sensitive diagnostic input") })
	if err == nil || !strings.Contains(err.Error(), "diagnostic-agent did not become ready") || !strings.Contains(err.Error(), "last readiness probe: error_kind=probe_error") || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("lost or unsafe timeout metadata: %v", err)
	}
}
func TestWaitForCanceledRequestDoesNotProbe(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("request canceled")
	cancel(cause)
	rt := &Runtime{Config: Config{BootTimeout: time.Second}, Stdout: io.Discard, Now: time.Now, Sleep: sleepContext}
	called := false
	err := rt.waitFor(ctx, "diagnostic", func(context.Context) error { called = true; return nil })
	if called || !errors.Is(err, cause) {
		t.Fatalf("probe called=%v, cause preserved=%v", called, errors.Is(err, cause))
	}
}

func TestWaitForZeroBudgetDoesNotProbe(t *testing.T) {
	rt := &Runtime{Config: Config{BootTimeout: 0}, Stdout: io.Discard, Now: time.Now, Sleep: sleepContext}
	err := rt.waitFor(context.Background(), "fixture", func(context.Context) error { t.Fatal("expired budget invoked probe"); return nil })
	if err == nil || !strings.Contains(err.Error(), "error_kind=not_probed") {
		t.Fatalf("zero budget=%v", err)
	}
}

func TestWaitForPropagatesCancellationCauseDuringProbe(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("request interrupted")
	rt := &Runtime{Config: Config{BootTimeout: time.Second}, Stdout: io.Discard, Now: time.Now}
	err := rt.waitFor(ctx, "fixture", func(child context.Context) error {
		cancel(cause)
		<-child.Done()
		return &CommandError{Message: "private output"}
	})
	if err != cause {
		t.Fatalf("cancellation cause=%v", err)
	}
}

func TestSSHReadinessProbesUseTheirBootBudget(t *testing.T) {
	for _, phase := range []string{"host key", "sudo smoke"} {
		t.Run(phase, func(t *testing.T) {
			runner := &fakeRunner{}
			runner.handler = func(name string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if name == "ssh-keyscan" || name == "ssh" {
					child := runner.contexts[len(runner.contexts)-1]
					<-child.Done()
					return nil, nil, context.Cause(child)
				}
				if strings.Contains(strings.Join(args, " "), "ip -4 route show default") {
					return []byte("default via 10.0.0.1 dev eth0"), nil, nil
				}
				return []byte(`[{"state":{"network":{"eth0":{"addresses":[{"family":"inet","scope":"global","address":"10.0.0.2"}]}}}}]`), nil, nil
			}
			rt := &Runtime{Config: Config{Incus: "incus", BootTimeout: 20 * time.Millisecond}, Runner: runner, Stdout: io.Discard, Now: time.Now, Sleep: sleepContext}
			parent, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			if phase == "host key" {
				err = rt.recordHostKey(parent, "e2e-vm-1")
			} else {
				err = rt.sshSmoke(parent, "e2e-vm-1")
			}
			var failure *readinessFailure
			if !errors.As(err, &failure) || failure.VM != "e2e-vm-1" || parent.Err() != nil {
				t.Fatalf("probe did not use independent budget: %v parent=%v", err, parent.Err())
			}
		})
	}
}

func TestProgressReapsNativeOperationBeforeCancellationReturns(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("native lifecycle interrupted")
	started, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	rt := &Runtime{Stdout: io.Discard, Now: time.Now}
	go func() {
		result <- rt.progress(ctx, "fixture lifecycle", func() error { close(started); <-ctx.Done(); <-release; return nil })
	}()
	<-started
	cancel(cause)
	select {
	case err := <-result:
		close(release)
		t.Fatalf("returned before operation completion: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != cause {
		t.Fatalf("cancellation lost: %v", err)
	}
}
