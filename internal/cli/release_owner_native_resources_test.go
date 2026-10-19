package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type ownerScopeReader struct {
	reconcileruntime.IncusReader
	instance ports.InstanceInfo
	stops    int
}

func (r *ownerScopeReader) Instance(context.Context, string, string) (ports.InstanceInfo, error) {
	return r.instance, nil
}
func (*ownerScopeReader) PrepareImageProject(_ context.Context, project string, shared bool) (ports.PreparedImageProject, error) {
	return ports.PreparedImageProject{Project: project, SharedImages: shared}, nil
}
func (*ownerScopeReader) ApplyPreparedImageProject(context.Context, ports.PreparedImageProject) error {
	panic("capture submitted a project effect")
}
func (r *ownerScopeReader) PrepareTemporaryStop(_ context.Context, project, instance string) (ports.PreparedTemporaryStop, error) {
	r.stops++
	return ports.PreparedTemporaryStop{Project: project, Instance: instance}, nil
}
func (*ownerScopeReader) ApplyPreparedTemporaryStop(context.Context, ports.PreparedTemporaryStop) error {
	panic("capture submitted a power effect")
}

func TestOwnerNativeCaptureCannotSelectReturnedForeignScope(t *testing.T) {
	for _, change := range []string{"none", "project", "instance"} {
		t.Run(change, func(t *testing.T) {
			r := &ownerScopeReader{instance: ports.InstanceInfo{Project: "subyard-e2e-yard", Name: "yard-e2e-yard", Status: "STOPPED"}}
			if change == "project" {
				r.instance.Project = "foreign"
			}
			if change == "instance" {
				r.instance.Name = "foreign"
			}
			c := &releaseOwnerComponents{cli: &CLI{options: Options{Incus: r}}}
			err := c.captureOwnerNativeResources(context.Background(), &ownerChildrenRecord{})
			if change == "none" {
				if err != nil || r.stops != 1 {
					t.Fatal("original stopped scope refused", err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) || r.stops != 0 {
				t.Fatal("returned foreign scope gained stop authority", err)
			}
		})
	}
}

func (*ownerScopeReader) OwnerCreateServer(context.Context) (string, error) {
	return "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", nil
}
