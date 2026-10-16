package application

import (
	"context"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestBootOptionalPublicationDoesNotDelayOrRestartYard(t *testing.T) {
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/yard": managedPowerInstance("p", "yard", "Stopped", domain.PowerRunning)}}
	pending := true
	reconciler := BootPowerReconciler{Inventory: fake, Instances: fake, Power: fake, Network: networkGuardFunc(func(context.Context, []string) error { return nil }), NetworkPolicy: bootNetworkPolicyFunc(allowBootNetworkPolicy), EnsureNetworkLock: ensureBootNetworkLock,
		ReconcilePublications: func(context.Context, yardnetwork.Yard) ([]string, error) {
			if pending {
				return []string{"tcp:192.0.2.12:8080"}, nil
			}
			return nil, nil
		}}
	for range 2 {
		result, err := reconciler.Run(context.Background())
		if !errors.Is(err, ports.ErrHostAddressUnavailable) || len(result.Waiting) != 0 || len(result.PublicationWaiting) != 1 || fake.Instances["p/yard"].Status != "Running" {
			t.Fatalf("optional wait stopped yard: %+v %v", result, err)
		}
	}
	pending = false
	result, err := reconciler.Run(context.Background())
	if err != nil || len(result.PublicationWaiting) != 0 || len(fake.PowerUpdates) != 1 {
		t.Fatalf("publication restore restarted yard: %+v %v %+v", result, err, fake.PowerUpdates)
	}
	reconciler.ReconcilePublications = func(context.Context, yardnetwork.Yard) ([]string, error) {
		return nil, errors.New("publication conflict")
	}
	_, err = reconciler.Run(context.Background())
	if err == nil || fake.Instances["p/yard"].Status != "Running" || len(fake.PowerUpdates) != 1 {
		t.Fatal("optional publication failure force-stopped healthy yard")
	}
}

func TestBootPublicationFailureIsolatedAcrossYards(t *testing.T) {
	for _, status := range []string{"Running", "Stopped"} {
		t.Run(status, func(t *testing.T) {
			fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"p/a": managedPowerInstance("p", "a", status, domain.PowerRunning),
				"p/m": managedPowerInstance("p", "m", "Stopped", domain.PowerRunning),
				"p/z": managedPowerInstance("p", "z", "Stopped", domain.PowerRunning),
			}}
			conflict := errors.New("invalid publication ownership")
			var runningChecks []string
			r := BootPowerReconciler{Inventory: fake, Instances: fake, Power: fake,
				Network:       networkGuardFunc(func(context.Context, []string) error { return nil }),
				NetworkPolicy: bootNetworkPolicyFunc(allowBootNetworkPolicy), EnsureNetworkLock: ensureBootNetworkLock,
				ReconcilePublications: func(_ context.Context, yard yardnetwork.Yard) ([]string, error) {
					if yard.Instance == "a" {
						return nil, conflict
					}
					if yard.Instance == "m" {
						return []string{"tcp:192.0.2.12:8080"}, nil
					}
					return nil, nil
				},
				ReconcileRunning: func(_ context.Context, yard yardnetwork.Yard) error {
					runningChecks = append(runningChecks, yard.Instance)
					return nil
				},
			}
			result, err := r.Run(context.Background())
			if !errors.Is(err, conflict) || !errors.Is(err, ports.ErrHostAddressUnavailable) ||
				len(result.PublicationErrors) != 1 || len(result.PublicationWaiting) != 1 ||
				fake.Instances["p/z"].Status != "Running" || fake.Instances["p/m"].Status != "Running" ||
				fake.Instances["p/a"].Status != status {
				t.Fatalf("publication failure blocked independent yards or lost diagnostics: %+v %v", result, err)
			}
			if status == "Running" && (len(runningChecks) != 1 || runningChecks[0] != "a") {
				t.Fatalf("publication failure skipped running security checks: %v", runningChecks)
			}
		})
	}
}

func TestBootPublicationConflictStillStopsUnsafeRunningYard(t *testing.T) {
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/a": managedPowerInstance("p", "a", "Running", domain.PowerRunning)}}
	runtimeErr := errors.New("unsafe runtime")
	r := BootPowerReconciler{Inventory: fake, Instances: fake, Power: fake,
		Network:       networkGuardFunc(func(context.Context, []string) error { return nil }),
		NetworkPolicy: bootNetworkPolicyFunc(allowBootNetworkPolicy), EnsureNetworkLock: ensureBootNetworkLock,
		ReconcilePublications: func(context.Context, yardnetwork.Yard) ([]string, error) {
			return nil, errors.New("publication conflict")
		},
		ReconcileRunning: func(context.Context, yardnetwork.Yard) error { return runtimeErr },
	}
	_, err := r.Run(context.Background())
	if !errors.Is(err, runtimeErr) || fake.Instances["p/a"].Status != "Stopped" || len(fake.PowerUpdates) != 1 || !fake.PowerUpdates[0].Force {
		t.Fatalf("publication failure bypassed unsafe runtime stop: %v %+v", err, fake.PowerUpdates)
	}
}
