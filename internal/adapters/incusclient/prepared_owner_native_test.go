package incusclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/lxc/incus/v6/shared/api"
)

func TestOrdinarySharedProjectRequiresCanonicalInitialConfig(t *testing.T) {
	for _, change := range []string{"valid", "nil", "empty", "malformed", "duplicate", "foreign-context", "appeared", "conflict"} {
		t.Run(change, func(t *testing.T) {
			mode := "ordinary"
			if change == "conflict" {
				mode = "ordinary-conflict"
			}
			client, fixture := ownerCreateClientFixture(t, &ownerCreateObserverFixture{}, mode)
			p := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, SharedImages: true}
			p.InitialConfig, _ = renderOwnerProjectConfig(t, domain.Context{YardName: "test-yard", IncusProject: p.Project, YardInstanceName: "yard-test-yard"}, []string{"SUBYARD_HOME=/fixture/data", "SUBYARD_CONFIG_HOME=/fixture/config", "YARD_NAME=test-yard", "YARD_INSTANCE_NAME=yard-test-yard", "HOST_BASE=/fixture/host", "SRV_POOL=default", "SRV_VOLUME=yard-test-yard", "STORAGE_PATH=/fixture/storage"})
			switch change {
			case "nil":
				p.InitialConfig = nil
			case "empty":
				p.InitialConfig = []byte{}
			case "malformed":
				p.InitialConfig = []byte("user.subyard.installation=1")
			case "duplicate":
				p.InitialConfig = append(p.InitialConfig, []byte("user.subyard.installation=1\x00")...)
			case "foreign-context":
				p.InitialConfig = []byte(strings.Replace(string(p.InitialConfig), `"incusProject":"subyard-test-yard"`, `"incusProject":"foreign"`, 1))
			case "appeared":
				fixture.mu.Lock()
				fixture.project = &api.Project{Name: p.Project, ProjectPut: api.ProjectPut{Config: map[string]string{"foreign": "keep"}}}
				fixture.mu.Unlock()
			}
			err := client.ApplyPreparedImageProject(context.Background(), p)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if change == "valid" {
				if err != nil || fixture.posts != 1 || fixture.project.Config["user.subyard.installation"] != "1" || fixture.project.Config["features.images"] != "false" {
					t.Fatal("canonical atomic create failed", err)
				}
				if _, exists := fixture.project.Config[ownerCreateTokenKey]; exists {
					t.Fatal("ordinary project acquired native token")
				}
			} else {
				if err == nil {
					t.Fatal("invalid or raced creation admitted")
				}
				wantPosts := 0
				if change == "conflict" {
					wantPosts = 1
				}
				if fixture.posts != wantPosts {
					t.Fatal("invalid configuration submitted")
				}
				if change == "appeared" || change == "conflict" {
					if len(fixture.project.Config) != 1 || fixture.project.Config["foreign"] != "keep" {
						t.Fatal("foreign project changed")
					}
				}
			}
		})
	}
}

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
