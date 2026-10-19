package incusclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/lxc/incus/v6/shared/api"
)

type cancelOwnerObserver struct {
	*custodyObserver
	cancel context.CancelFunc
}

func (o *cancelOwnerObserver) Accepted(value ports.NativeOperation) error {
	err := o.custodyObserver.Accepted(value)
	o.cancel()
	return err
}

func TestPreparedOwnerTemporaryStopChecksOriginalUUIDBeforeSubmit(t *testing.T) {
	for _, drift := range []string{"none", "uuid", "desired", "missing", "already-stopped", "known-failure", "cancel"} {
		t.Run(drift, func(t *testing.T) {
			fake, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { fake.Close() })
			instance := map[string]any{"name": "yard-e2e-yard", "type": "container", "status": "Stopped", "config": map[string]string{"volatile.uuid": "original-uuid", "user.subyard.desired_power": "running"}}
			fake.SetInstance("subyard-e2e-yard", "yard-e2e-yard", instance)
			client := New(fake.SocketPath, "projects")
			prepared, err := client.PrepareTemporaryStop(context.Background(), "subyard-e2e-yard", "yard-e2e-yard")
			if err != nil {
				t.Fatal(err)
			}
			instance["status"] = "Running"
			switch drift {
			case "uuid":
				instance["config"] = map[string]string{"volatile.uuid": "foreign-uuid", "user.subyard.desired_power": "running"}
			case "desired":
				instance["config"] = map[string]string{"volatile.uuid": "original-uuid", "user.subyard.desired_power": "stopped"}
			case "already-stopped":
				instance["status"] = "Stopped"
			case "known-failure":
				fake.QueuePowerErrors("fixed fixture failure")
			}
			fake.SetInstance("subyard-e2e-yard", "yard-e2e-yard", instance)
			if drift == "missing" {
				fake.ReplaceInstances(nil)
			}
			observer := &custodyObserver{}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var nativeObserver ports.NativeOperationObserver = observer
			if drift == "cancel" {
				nativeObserver = &cancelOwnerObserver{observer, cancel}
			}
			ctx = ports.WithNativeOperationObserver(ctx, nativeObserver)
			err = client.ApplyPreparedTemporaryStop(ctx, prepared)
			if drift == "cancel" {
				if err == nil || len(observer.accepted) != 1 {
					t.Fatal("cancellation invented successful completion", err)
				}
				for _, final := range observer.terminal {
					if final.ID != observer.accepted[0].ID || !nativeFinalStatus(api.StatusCode(final.Status)) {
						t.Fatal("cancellation invented final identity")
					}
				}
				if len(observer.terminal) == 0 && !observer.uncertain {
					t.Fatal("unproved cancellation lost UNKNOWN")
				}
				return
			}
			if drift == "uuid" || drift == "desired" || drift == "missing" {
				if err == nil || len(fake.PowerCalls()) != 0 || len(observer.accepted) != 0 {
					t.Fatal("changed original resource submitted", err)
				}
				if drift != "missing" && !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal(err)
				}
				return
			}
			if drift == "already-stopped" {
				if err != nil || len(fake.PowerCalls()) != 0 {
					t.Fatal("original stop no-op submitted", err)
				}
				return
			}
			if drift == "known-failure" {
				var failure *ports.NativeOperationFailure
				if !errors.As(err, &failure) || observer.uncertain {
					t.Fatal("known native failure lost", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(fake.PowerCalls()) != 1 || len(observer.accepted) != 1 || len(observer.terminal) != 1 {
				t.Fatal("missing exact accepted/final operation", err)
			}
			op := observer.accepted[0]
			if op.ID == "" || op.Server == "" || op.Project != prepared.Project || op.Resource != prepared.Instance {
				t.Fatal("operation scope not bound")
			}
			current, err := client.NativeOperationState(context.Background(), op)
			if err != nil || current != observer.terminal[0] {
				t.Fatal("exact durable operation query changed", err)
			}
		})
	}
}

func TestPreparedOwnerImageProjectRefusesOriginalScopeDrift(t *testing.T) {
	for _, change := range []string{"none", "config", "name", "absent-unshared"} {
		t.Run(change, func(t *testing.T) {
			fake, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { fake.Close() })
			project := map[string]any{"name": "subyard-e2e-yard", "config": map[string]string{"features.images": "false", "foreign": "keep"}}
			if change != "absent-unshared" {
				fake.SetProject("subyard-e2e-yard", project)
			}
			client := New(fake.SocketPath, "projects")
			prepared, err := client.PrepareImageProject(context.Background(), "subyard-e2e-yard", change != "absent-unshared")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "config":
				project["config"] = map[string]string{"features.images": "false", "foreign": "changed"}
			case "name":
				project["name"] = "foreign-project"
			}
			if change != "absent-unshared" {
				fake.SetProject("subyard-e2e-yard", project)
			}
			err = client.ApplyPreparedImageProject(context.Background(), prepared)
			if change == "none" || change == "absent-unshared" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("changed project admitted", err)
			}
		})
	}
}
