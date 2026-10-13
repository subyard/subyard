package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type bootNetworkGuard struct{}

func (bootNetworkGuard) Check(context.Context, []string) error { return nil }

type bootPowerManagerFunc func(context.Context, string, string, string, bool) error

type bootNetworkPolicyStub struct{}

func (bootNetworkPolicyStub) WithStart(
	_ context.Context,
	_ yardnetwork.Yard,
	start func() error,
) error {
	return start()
}

func bootLockStub() error { return nil }

func (function bootPowerManagerFunc) SetInstancePower(
	ctx context.Context,
	project string,
	name string,
	action string,
	force bool,
) error {
	return function(ctx, project, name, action, force)
}

func bootPowerFailureReconciler(powerError error) application.BootPowerReconciler {
	instance := ports.InstanceInfo{
		Project: "p", Name: "yard", Status: "Stopped",
		Config: map[string]string{
			"user.subyard.managed": "true", "user.subyard.initialized": "true",
			"user.subyard.desired_power": "running", "user.subyard.bridge": "incusbr0",
			"boot.autostart": "false",
		},
	}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/yard": instance}}
	return application.BootPowerReconciler{
		Inventory: fake,
		Instances: fake,
		Power: bootPowerManagerFunc(func(context.Context, string, string, string, bool) error {
			return powerError
		}),
		Network: bootNetworkGuard{}, NetworkPolicy: bootNetworkPolicyStub{},
		EnsureNetworkLock: bootLockStub,
	}
}

func TestRunBootPowerReturnsTempfailForUnavailableIncus(t *testing.T) {
	reconciler := bootPowerFailureReconciler(
		fmt.Errorf("wait for start instance: %w", ports.ErrIncusUnavailable),
	)
	if code := RunBootPower(
		context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{}, reconciler,
	); code != 75 {
		t.Fatalf("temporary Incus failure returned %d, want 75", code)
	}
}

func TestRunBootPowerPublicationConflictKeepsOtherYardRetry(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			r := bootPowerFailureReconciler(nil)
			fake := r.Inventory.(*testkit.Incus)
			instance := fake.Instances["p/yard"]
			delete(fake.Instances, "p/yard")
			instance.Name, instance.Status = "a", "Running"
			fake.Instances["p/a"] = instance
			instance.Name, instance.Status = "z", "Stopped"
			fake.Instances["p/z"] = instance
			r.Power = fake
			checks := 0
			r.ReconcileRunning = func(context.Context, yardnetwork.Yard) error { checks++; return nil }
			r.ReconcilePublications = func(_ context.Context, yard yardnetwork.Yard) ([]string, error) {
				if yard.Instance == "a" {
					return nil, errors.New("ownership conflict")
				}
				if pending {
					return []string{"tcp:192.0.2.12:8080"}, nil
				}
				return nil, nil
			}
			var stdout, stderr bytes.Buffer
			code := RunBootPower(context.Background(), nil, &stdout, &stderr, r)
			want := 1
			if pending {
				want = 75
			}
			if code != want || !strings.Contains(stderr.String(), "FAIL: reconcile optional publication p/a: ownership conflict") ||
				fake.Instances["p/z"].Status != "Running" || checks != 1 {
				t.Fatalf("publication conflict lost failure/retry or skipped independent yard/security checks: exit=%d stdout=%s stderr=%s checks=%d", code, stdout.String(), stderr.String(), checks)
			}
			if pending && !strings.Contains(stdout.String(), "publication-waiting-for-address p/z") {
				t.Fatal("missing address lost its retry diagnostic")
			}
		})
	}
}

func TestRunBootPowerRecoversAfterAsyncStorageUnavailable(t *testing.T) {
	server, err := testkit.NewIncusServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	// The named yard sorts before the default yard, as in the boot failure.
	for project, desired := range map[string]string{
		"subyard-test": "running", "subyard": "running", "subyard-stopped": "stopped",
	} {
		config := map[string]string{
			"user.subyard.managed": "true", "user.subyard.initialized": "true",
			"user.subyard.desired_power": desired, "user.subyard.bridge": "incusbr0",
			"boot.autostart": "false",
		}
		server.SetInstance(project, "yard", map[string]any{
			"name": "yard", "project": project, "type": "container", "status": "Stopped",
			"config": config, "expanded_config": config,
		})
	}
	server.QueuePowerErrors(`Storage pool "default" unavailable on this server`)
	client := incusclient.New(server.SocketPath)
	reconciler := application.BootPowerReconciler{
		Inventory: client, Instances: client, Power: client, Network: bootNetworkGuard{},
		NetworkPolicy: bootNetworkPolicyStub{}, EnsureNetworkLock: bootLockStub,
	}
	var stdout, stderr bytes.Buffer
	if code := RunBootPower(context.Background(), nil, &stdout, &stderr, reconciler); code != 75 {
		t.Fatalf("async storage failure returned %d, want retry: %s", code, stderr.String())
	}
	if calls := server.PowerCalls(); len(calls) != 1 || calls[0].Project != "subyard-test" {
		t.Fatalf("unexpected failed boot start: %+v", calls)
	}
	for range 2 {
		if code := RunBootPower(context.Background(), nil, &stdout, &stderr, reconciler); code != 0 {
			t.Fatalf("recovery returned %d: %s", code, stderr.String())
		}
	}
	if calls := server.PowerCalls(); len(calls) != 3 ||
		calls[1].Project != "subyard-test" || calls[2].Project != "subyard" {
		t.Fatalf("retry did not restore both yards exactly once: %+v", calls)
	}
	for project, status := range map[string]string{
		"subyard-test": "Running", "subyard": "Running", "subyard-stopped": "Stopped",
	} {
		instance, err := client.Instance(context.Background(), project, "yard")
		if err != nil || instance.Status != status || instance.Config["user.subyard.desired_power"] != strings.ToLower(status) {
			t.Fatalf("unexpected recovered state for %s: %+v, %v", project, instance, err)
		}
	}
}

func TestRunBootPowerReturnsFailureForPermanentError(t *testing.T) {
	reconciler := bootPowerFailureReconciler(errors.New("invalid instance configuration"))
	if code := RunBootPower(
		context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{}, reconciler,
	); code != 1 {
		t.Fatalf("permanent power failure returned %d, want 1", code)
	}
}

func TestRunBootPowerAndHasManaged(t *testing.T) {
	instance := ports.InstanceInfo{
		Project: "p", Name: "yard", Status: "Stopped",
		Config: map[string]string{
			"user.subyard.managed": "true", "user.subyard.initialized": "true",
			"user.subyard.desired_power": "running", "user.subyard.bridge": "incusbr0",
			"boot.autostart": "false",
		},
	}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/yard": instance}}
	reconciler := application.BootPowerReconciler{
		Inventory: fake, Instances: fake, Power: fake, Network: bootNetworkGuard{},
		NetworkPolicy: bootNetworkPolicyStub{}, EnsureNetworkLock: bootLockStub,
	}
	var stdout, stderr bytes.Buffer
	if code := RunBootPower(context.Background(), []string{"has-managed"}, &stdout, &stderr, reconciler); code != 0 {
		t.Fatalf("has-managed failed with %d: %s", code, stderr.String())
	}
	if code := RunBootPower(context.Background(), nil, &stdout, &stderr, reconciler); code != 0 {
		t.Fatalf("reconcile failed with %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "started p/yard") {
		t.Fatalf("missing reconcile output: %q", stdout.String())
	}
}

func TestRunBootPowerHasManagedExitCodes(t *testing.T) {
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	reconciler := application.BootPowerReconciler{Inventory: fake}
	if code := RunBootPower(context.Background(), []string{"has-managed"}, &bytes.Buffer{}, &bytes.Buffer{}, reconciler); code != 1 {
		t.Fatalf("unmanaged inventory returned %d", code)
	}
	if code := RunBootPower(context.Background(), []string{"unknown"}, &bytes.Buffer{}, &bytes.Buffer{}, reconciler); code != 2 {
		t.Fatalf("unknown argument returned %d", code)
	}
}
