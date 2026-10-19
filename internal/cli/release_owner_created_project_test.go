package cli

import (
	"context"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
)

type originalProjectCreateFixture struct {
	ownerNativeResources
	creates, checks int
	last            ports.NativeOwnerCreated
}

func (f *originalProjectCreateFixture) ApplyPreparedOwnerImageProject(_ context.Context, original ports.PreparedImageProject, scope string) (*ports.NativeOwnerCreated, error) {
	f.creates++
	value := ports.NativeOwnerCreated{Ref: ports.NativeOwnerCreateRef{Domain: "command", Index: uint64(f.creates)}, Intent: ports.NativeOwnerCreateIntent{Scope: scope, Kind: "project", Project: original.Project, Resource: original.Project}, Status: 200}
	f.last = value
	return &value, nil
}

func (f *originalProjectCreateFixture) CheckOwnerCreatedImageProject(_ context.Context, _ ports.PreparedImageProject, original ports.NativeOwnerCreated) error {
	f.checks++
	f.last = original
	return nil
}

type originalCreateObserverFixture struct{ ports.NativeOperationObserver }

func TestOwnerImageProjectDispatchRetainsExactOriginalScopeReference(t *testing.T) {
	ctx := ports.WithNativeOperationObserver(context.Background(), originalCreateObserverFixture{})
	p := &preparedOwnerChildren{}
	f := &originalProjectCreateFixture{}
	current := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, SharedImages: true}
	legacy := ports.PreparedImageProject{Project: "subyard-e2e-yard", Absent: true, SharedImages: true}
	if err := p.applyOwnerImageProject(ctx, f, current, "current"); err != nil {
		t.Fatal(err)
	}
	if err := p.applyOwnerImageProject(ctx, f, legacy, "legacy"); err != nil {
		t.Fatal(err)
	}
	if p.createdCurrentProject == nil || p.createdLegacyProject == nil || p.createdCurrentProject.Ref == p.createdLegacyProject.Ref {
		t.Fatal("canonical and compensation creates lost original references")
	}
	if err := p.applyOwnerImageProject(ctx, f, current, "current"); err != nil || f.creates != 2 || f.checks != 1 || f.last != *p.createdCurrentProject {
		t.Fatal("original current scope replayed or selected another result", err)
	}
	if err := p.applyOwnerImageProject(ctx, f, legacy, "legacy"); err != nil || f.creates != 2 || f.checks != 2 || f.last != *p.createdLegacyProject {
		t.Fatal("original compensation scope replayed or selected another result", err)
	}
}
