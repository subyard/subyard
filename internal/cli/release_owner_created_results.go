package cli

import (
	"context"
	"encoding/json"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

// BindOwnerResults matches only the original inert capsule. Live resource
// checks remain at each component dispatch; result identity is not closure.
func (c *releaseOwnerComponents) BindOwnerResults(ctx context.Context, before releaseowner.OwnerBefore, _ string, values []ports.NativeOwnerCreated) error {
	original := c.ownerChildren
	if original == nil || original.binding() != before.PreparedRecipe || original.before != ownerSemanticBefore(before) || !c.installedBound[before.PreparedRecipe] {
		return domain.ErrPlanStale
	}
	var record ownerChildrenRecord
	for _, file := range original.wire.Files {
		if file.Path == "prepared/owner-children.json" {
			if json.Unmarshal(file.Content, &record) != nil {
				return domain.ErrPlanStale
			}
		}
	}
	if record.Version != 1 || record.Before != ownerSemanticBefore(before) || len(values) > 9 {
		return domain.ErrPlanStale
	}
	seen := map[ports.NativeOwnerCreateRef]bool{}
	for _, value := range values {
		if seen[value.Ref] || value.Ref.Producer == "" || value.Ref.Index == 0 || value.Ref.Index > 256 || value.Status != 200 {
			return domain.ErrPlanStale
		}
		seen[value.Ref] = true
		matches := 0
		intent := value.Intent
		if intent.Kind == "project" && intent.Scope != "recreate" {
			for index, project := range record.Projects {
				scope := "current"
				if index == 1 {
					scope = "legacy"
				}
				if project.Absent && intent.Scope == scope && intent.Server == project.Server && intent.Project == project.Project && intent.Resource == project.Project && intent.Pool == "" && value.Ref.Domain == "command" {
					matches++
				}
			}
		}
		for _, child := range record.Inits {
			scope := "current"
			if child.Yard == testyardmigration.LegacyYard {
				scope = "legacy"
			}
			if child.Kind == "recreate" {
				scope = "recreate"
			}
			if intent.Scope != scope {
				continue
			}
			switch intent.Kind {
			case "project":
				project := child.Project
				if scope == "recreate" && project != nil && project.Absent && intent.Server == project.Server && intent.Project == project.Project && intent.Resource == project.Project && intent.Pool == "" && value.Ref.Domain == "command" {
					matches++
				}
			case "instance":
				instance := child.Instance
				if instance != nil && intent.Server == instance.Server && intent.Project == instance.Project && intent.Resource == instance.Instance && intent.Pool == "" && domain.SafeID(value.InstanceUUID) && value.Ref.Domain == "operation" {
					matches++
				}
			case "srv-volume":
				volume := child.Volume
				if volume != nil && volume.Absent && intent.Server == volume.Server && intent.Project == volume.Project && intent.Resource == volume.Volume && intent.Pool == volume.Pool && value.Ref.Domain == "command" {
					matches++
				}
			}
		}
		if matches != 1 {
			return domain.ErrPlanStale
		}
	}
	reader, ok := ports.NativeOperationObserverFrom(ctx).(interface {
		ReadOwnerCreated(ports.NativeOwnerCreateRef) (ports.NativeOwnerCreated, error)
	})
	if !ok {
		return domain.ErrPlanStale
	}
	for _, value := range values {
		actual, err := reader.ReadOwnerCreated(value.Ref)
		if err != nil || actual != value {
			return domain.ErrPlanStale
		}
	}
	c.ownerResultReader = reader
	c.ownerResultsRecipe = before.PreparedRecipe
	c.ownerResults = append([]ports.NativeOwnerCreated(nil), values...)
	return nil
}

func (c *releaseOwnerComponents) checkedOwnerResults() ([]ports.NativeOwnerCreated, error) {
	if c.ownerChildren == nil || c.ownerResultReader == nil || c.ownerResultsRecipe != c.ownerChildren.binding() || !c.installedBound[c.ownerResultsRecipe] {
		return nil, domain.ErrPlanStale
	}
	for _, value := range c.ownerResults {
		actual, err := c.ownerResultReader.ReadOwnerCreated(value.Ref)
		if err != nil || actual != value {
			return nil, domain.ErrPlanStale
		}
	}
	return c.ownerResults, nil
}

// ResolveOwnerTarget keeps the original approved target immutable. Only its
// explicit future-absent instance may acquire an original successful UUID.
func (c *releaseOwnerComponents) ResolveOwnerTarget(ctx context.Context, target releaseowner.Target) (releaseowner.Target, error) {
	values, err := c.checkedOwnerResults()
	if err != nil {
		return target, err
	}
	if target.InstanceUUID != "absent" {
		return target, nil
	}
	var result *ports.NativeOwnerCreated
	for index, value := range values {
		if value.Intent.Kind == "instance" && value.Intent.Project == target.Project && value.Intent.Resource == target.Instance {
			if result != nil {
				return target, domain.ErrPlanStale
			}
			result = &values[index]
		}
	}
	if result == nil {
		return target, nil
	}
	incus, _ := c.cli.statusPorts()
	instance, err := incus.Instance(ctx, target.Project, target.Instance)
	if err != nil || instance.Project != target.Project || instance.Name != target.Instance || instance.Config["volatile.uuid"] != result.InstanceUUID || instance.Config["user.subyard.owner_create"] != result.Intent.Token {
		return target, domain.ErrPlanStale
	}
	target.InstanceUUID = result.InstanceUUID
	return target, nil
}
