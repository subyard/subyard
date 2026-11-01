package cli

import (
	"context"
	"errors"
	"reflect"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

// Physical observations are outputs of closed native actions, not new migration
// plans. The original source capsule and Rust programme remain authoritative.
// Only their digest crosses the native result boundary; a fresh process must
// reproduce that exact observation before using its physical handles.
type ownerPhysicalReceipt struct {
	group                            releaseowner.Binding
	program, recipe, token, physical string
}

type ownerPhysicalYard struct {
	Snapshot ownerTeardownSnapshot
	Network  *ownerTeardownNetwork
	Start    *ownerLifecycleRecord
}

func (c *releaseOwnerComponents) hasOwnerCreatedInstance(scope string) bool {
	for _, value := range c.ownerResults {
		if value.Intent.Scope == scope && value.Intent.Kind == "instance" {
			return true // BindOwnerResults already checked its exact capsule/ref.
		}
	}
	return false
}

func (c *releaseOwnerComponents) BindOwnerPhysical(_ context.Context, before releaseowner.OwnerBefore, token string, action releaseowner.OwnerActionReference, physical string) error {
	if c.ownerChildren == nil || before.PreparedRecipe != c.ownerChildren.binding() || ownerSemanticBefore(before) != c.ownerChildren.before || c.ownerResultsRecipe != before.PreparedRecipe {
		return domain.ErrPlanStale
	}
	c.ownerPhysical = &ownerPhysicalReceipt{action.Group, action.Program, before.PreparedRecipe, token, physical}
	c.ownerPhysicalValidated = false
	return nil
}

func (c *releaseOwnerComponents) ownerPhysicalCandidate(ctx context.Context) (map[string]preparedOwnerTeardownChild, *ownerLifecycleRecord, string, error) {
	p := c.ownerChildren
	if p == nil {
		return nil, nil, "", domain.ErrPlanStale
	}
	children := map[string]preparedOwnerTeardownChild{}
	projection := []ownerPhysicalYard{}
	var start *ownerLifecycleRecord
	for _, yard := range []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard} {
		original, ok := p.teardowns[yard]
		if !ok || original.execution == nil {
			// Adoption has no native children or physical mutation authority.
			if len(p.teardowns) == 0 {
				return children, nil, nativeValueDigest(projection), nil
			}
			return nil, nil, "", domain.ErrPlanStale
		}
		execution := &teardownExecution{teardownOptions: original.execution.teardownOptions}
		if err := p.operation.observeTeardownExecution(ports.WithoutNativeOperationObserver(ctx), original.loaded, execution); err != nil {
			return nil, nil, "", err
		}
		execution.retainOwnerSourceSettings(original.loaded)
		if err := c.checkOwnerPhysicalResources(ctx, yard, execution.snapshot()); err != nil {
			return nil, nil, "", err
		}
		// Reuse the existing lossless physical subrecord, without serializing
		// the execution plan or recovery bookkeeping. Process-local source FD
		// spellings use logical labels only in this digest, not live handles.
		snapshot := *execution.snapshot()
		snapshot.Registered = c.ownerCheckLabel(ownerChildCheck{Yards: snapshot.Registered}).Yards
		observed, err := describeOwnerTeardownSnapshot(&snapshot)
		if err != nil {
			return nil, nil, "", err
		}
		network, err := describeTeardownNetwork(execution.networkRemoval())
		if err != nil {
			return nil, nil, "", err
		}
		physical := ownerPhysicalYard{Snapshot: observed, Network: network}
		if yard == testyardmigration.LegacyYard && p.inits[yard].start != nil && execution.snapshot().State.InstanceFound {
			child := p.inits[yard]
			sources, err := p.restoreChildSources(ctx, yard)
			if err != nil {
				return nil, nil, "", err
			}
			runtime, err := child.runtime.OwnerSourceRuntime(ports.WithoutNativeOperationObserver(ctx), sources)
			if err != nil {
				return nil, nil, "", errors.Join(err, sources.Close())
			}
			operation, err := ownerResourceSourceCLI(p.operation, child.runtime, runtime)
			if err == nil {
				start, err = captureOwnerStart(ports.WithoutNativeOperationObserver(ctx), operation, child.loaded, child.runtime.LogicalRepositoryRoot)
			}
			if err = errors.Join(err, sources.Close()); err != nil {
				return nil, nil, "", err
			}
			start.Publication = child.start.Publication
			physical.Start = start
		}
		projection = append(projection, physical)
		children[yard] = preparedOwnerTeardownChild{original.loaded, execution}
	}
	return children, start, nativeValueDigest(projection), nil
}

// Reject expansion beyond the exact original resources and the finite roles
// already approved in the init capsules. A created identity must also have its
// authenticated result; finding a resource by name never creates that authority.
func (c *releaseOwnerComponents) checkOwnerPhysicalResources(ctx context.Context, yard string, snapshot *teardownSnapshot) error {
	if ports.NativeOperationObserverFrom(ctx) == nil {
		if reader, ok := c.ownerResultReader.(ports.NativeOperationObserver); ok {
			ctx = ports.WithNativeOperationObserver(ctx, reader)
		}
	}
	p := c.ownerChildren
	original := p.teardowns[yard].execution.snapshot()
	allowed := map[string]ports.TeardownResource{}
	key := func(v ports.TeardownResource) string { return v.Kind + "/" + v.Pool + "/" + v.Name }
	for _, resource := range original.Resources {
		allowed[key(resource)] = resource
	}
	values := append([]ports.NativeOwnerCreated(nil), c.ownerResults...)
	if observer, ok := ports.NativeOperationObserverFrom(ctx).(interface {
		OwnerCreatedResults() ([]ports.NativeOwnerCreated, error)
	}); ok {
		created, err := observer.OwnerCreatedResults()
		if err != nil {
			return err
		}
		values = append(values, created...)
	}
	incus, _ := c.cli.statusPorts()
	for _, value := range values {
		child := p.inits[yard]
		if value.Intent.Scope == "recreate" {
			child = p.recreate[yard]
		}
		if value.Intent.Project != child.loaded.Context.IncusProject {
			continue
		}
		switch value.Intent.Kind {
		case "instance":
			if !snapshot.State.InstanceFound {
				continue
			}
			checker, ok := incus.(interface {
				CheckOwnerCreatedInstance(context.Context, ports.PreparedOwnerInstance, ports.NativeOwnerCreated) error
			})
			if !ok || child.instance == nil || checker.CheckOwnerCreatedInstance(ctx, *child.instance, value) != nil {
				return domain.ErrPlanStale
			}
			if value.InstanceUUID != snapshot.State.Instance.Config["volatile.uuid"] {
				return domain.ErrPlanStale
			}
			allowed["instance//"+value.Intent.Resource] = ports.TeardownResource{}
			if value.Intent.Fingerprint != "" {
				allowed["image//"+value.Intent.Fingerprint] = ports.TeardownResource{}
			}
		case "srv-volume":
			if !snapshot.State.VolumeFound {
				continue
			}
			checker, ok := incus.(interface {
				CheckOwnerCreatedVolume(context.Context, ports.PreparedOwnerVolume, ports.NativeOwnerCreated) error
			})
			if !ok || child.volume == nil || checker.CheckOwnerCreatedVolume(ctx, *child.volume, value) != nil {
				return domain.ErrPlanStale
			}
			allowed["volume/"+value.Intent.Pool+"/"+value.Intent.Resource] = ports.TeardownResource{}
		case "project":
			if !snapshot.State.ProjectFound {
				continue
			}
			// Project results are already matched to their exact original
			// prepared namespace by BindOwnerResults / the current observer.
			allowed["project//"+value.Intent.Resource] = ports.TeardownResource{}
			allowed["profile//default"] = ports.TeardownResource{}
		}
	}
	if snapshot.State.InstanceFound && original.State.InstanceFound && snapshot.State.Instance.Config["volatile.uuid"] != original.State.Instance.Config["volatile.uuid"] {
		if allowed["instance//"+snapshot.State.Instance.Name].Binding != "" {
			return domain.ErrPlanStale
		}
	}
	for _, resource := range snapshot.Resources {
		prior, ok := allowed[key(resource)]
		if !ok || resource.SnapshotCount > 0 && resource.Binding != prior.Binding || (resource.Kind == "pool" || resource.Kind == "network" || resource.Kind == "host-profile") && resource.Binding != prior.Binding {
			return domain.ErrPlanStale
		}
	}
	return nil
}

func (c *releaseOwnerComponents) restoreOwnerPhysical(ctx context.Context, before releaseowner.OwnerBefore, token string, action releaseowner.OwnerActionReference) error {
	receipt := c.ownerPhysical
	if receipt != nil && (receipt.group != action.Group || receipt.program != action.Program || receipt.recipe != before.PreparedRecipe || receipt.token != token) {
		return domain.ErrPlanStale
	}
	children, start, physical, err := c.ownerPhysicalCandidate(ctx)
	if err != nil || receipt != nil && physical != receipt.physical {
		return errors.Join(domain.ErrPlanStale, err)
	}
	if receipt == nil {
		if err := c.checkOriginalOwnerPhysical(ctx, children, start); err != nil {
			return err
		}
	}
	// Rebuild only the physical handles. All original loaded settings, source
	// capsules, publication contracts and Rust action selection stay intact.
	c.ownerChildren.teardowns = children
	if start != nil {
		child := c.ownerChildren.inits[testyardmigration.LegacyYard]
		child.start = start
		c.ownerChildren.inits[testyardmigration.LegacyYard] = child
	}
	c.ownerPhysicalValidated = true
	return nil
}

func (c *releaseOwnerComponents) CaptureOwnerPhysical(ctx context.Context, before releaseowner.OwnerBefore, token string, action releaseowner.OwnerActionReference) (string, error) {
	children, start, physical, err := c.ownerPhysicalCandidate(ctx)
	if err != nil {
		return "", err
	}
	if !c.ownerPhysicalValidated {
		if c.ownerPhysical != nil {
			return "", domain.ErrPlanStale
		}
		if err := c.checkOriginalOwnerPhysical(ctx, children, start); err != nil {
			return "", err
		}
	}
	return physical, nil
}

func (c *releaseOwnerComponents) checkOriginalOwnerPhysical(ctx context.Context, children map[string]preparedOwnerTeardownChild, start *ownerLifecycleRecord) error {
	for yard, child := range children {
		original := c.ownerChildren.teardowns[yard].execution
		before, after := *original.snapshot(), *child.execution.snapshot()
		before.Registered = c.ownerCheckLabel(ownerChildCheck{Yards: before.Registered}).Yards
		after.Registered = c.ownerCheckLabel(ownerChildCheck{Yards: after.Registered}).Yards
		if err := checkTeardownSnapshot(&before, &after); err != nil {
			return err
		}
		if !reflect.DeepEqual(original.networkRemoval(), child.execution.networkRemoval()) {
			return domain.ErrPlanStale
		}
	}
	if child, ok := c.ownerChildren.inits[testyardmigration.LegacyYard]; ok && child.start != nil {
		if start == nil {
			return domain.ErrPlanStale
		}
		execution, err := child.start.restore(child.loaded)
		if err != nil {
			return err
		}
		if err := c.ownerChildren.operation.observeLifecycleExecution(ctx, child.loaded.Context, execution); err != nil {
			return err
		}
		// The existing lifecycle guard permits convergence to running and
		// derived skip decisions. It still binds all target metadata and the
		// original resource-startup contract.
		allowed := *start
		allowed.Status, allowed.Changed, allowed.VMCPUReady = child.start.Status, child.start.Changed, child.start.VMCPUReady
		allowed.Observed.Instance.Status, allowed.Steps = child.start.Observed.Instance.Status, child.start.Steps
		if nativeValueDigest(allowed) != nativeValueDigest(*child.start) {
			return domain.ErrPlanStale
		}
	}
	return nil
}
