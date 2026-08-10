package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type bootNetworkGuard struct{}

func (bootNetworkGuard) Check(context.Context, []string) error { return nil }

type bootPowerManagerFunc func(context.Context, string, string, string, bool) error

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
		Network: bootNetworkGuard{},
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
