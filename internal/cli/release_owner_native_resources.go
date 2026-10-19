package cli

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type ownerNativeResources interface {
	PrepareImageProject(context.Context, string, bool) (ports.PreparedImageProject, error)
	ApplyPreparedImageProject(context.Context, ports.PreparedImageProject) error
	PrepareTemporaryStop(context.Context, string, string) (ports.PreparedTemporaryStop, error)
	ApplyPreparedTemporaryStop(context.Context, ports.PreparedTemporaryStop) error
}

func (c *releaseOwnerComponents) captureOwnerNativeResources(ctx context.Context, record *ownerChildrenRecord) error {
	incus, _ := c.cli.statusPorts()
	native, ok := incus.(ownerNativeResources)
	if !ok {
		return errors.New("owner migration requires prepared SDK resource handles")
	}
	connection, ok := incus.(interface {
		OwnerCreateServer(context.Context) (string, error)
	})
	if !ok {
		return domain.ErrPlanStale
	}
	originalServer, err := connection.OwnerCreateServer(ctx)
	if err != nil {
		return err
	}
	for _, yard := range []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard} {
		p, err := native.PrepareImageProject(ctx, "subyard-"+yard, record.Before.SharedImages)
		if err != nil {
			return err
		}
		p.Server = originalServer
		record.Projects = append(record.Projects, p)
	}
	instance, err := incus.Instance(ctx, "subyard-"+testyardmigration.LegacyYard, "yard-"+testyardmigration.LegacyYard)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if instance.Project != "subyard-"+testyardmigration.LegacyYard || instance.Name != "yard-"+testyardmigration.LegacyYard {
		return domain.ErrPlanStale
	}
	if strings.EqualFold(instance.Status, "stopped") {
		p, err := native.PrepareTemporaryStop(ctx, instance.Project, instance.Name)
		if err != nil {
			return err
		}
		record.TemporaryStop = &p
	}
	return nil
}

func (p *preparedOwnerChildren) applyOwnerImageProject(ctx context.Context, native ownerNativeResources, original ports.PreparedImageProject, scope string) error {
	if ports.NativeOperationObserverFrom(ctx) == nil {
		return native.ApplyPreparedImageProject(ctx, original)
	}
	creator, ok := native.(interface {
		ApplyPreparedOwnerImageProject(context.Context, ports.PreparedImageProject, string) (*ports.NativeOwnerCreated, error)
		CheckOwnerCreatedImageProject(context.Context, ports.PreparedImageProject, ports.NativeOwnerCreated) error
	})
	if !ok || scope != "current" && scope != "legacy" && scope != "recreate" {
		return domain.ErrPlanStale
	}
	retained := &p.createdCurrentProject
	if scope == "legacy" {
		retained = &p.createdLegacyProject
	} else if scope == "recreate" {
		retained = &p.createdRecreateProject
	}
	if *retained != nil {
		if (*retained).Intent.Scope != scope {
			return domain.ErrPlanStale
		}
		return creator.CheckOwnerCreatedImageProject(ctx, original, **retained)
	}
	created, err := creator.ApplyPreparedOwnerImageProject(ctx, original, scope)
	if err != nil {
		return err
	}
	if created != nil {
		if created.Ref.Domain != "command" || created.Ref.Index == 0 || created.Ref.Index > 256 || created.Intent.Scope != scope || created.Intent.Kind != "project" || created.Intent.Project != original.Project || created.Intent.Resource != original.Project || created.Status != 200 {
			return domain.ErrPlanStale
		}
		*retained = created
	}
	return nil
}

func (c *releaseOwnerComponents) ownerNativeOptions(record ownerChildrenRecord, options *testyardmigration.Options, children *preparedOwnerChildren) error {
	incus, _ := c.cli.statusPorts()
	native, ok := incus.(ownerNativeResources)
	if !ok || len(record.Projects) != 2 || children == nil {
		return domain.ErrPlanStale
	}
	for i, p := range record.Projects {
		if p.Project != []string{"subyard-" + testyardmigration.CurrentYard, "subyard-" + testyardmigration.LegacyYard}[i] || p.SharedImages != record.Before.SharedImages || p.Absent && p.Config != nil {
			return domain.ErrPlanStale
		}
	}
	options.EnsureImageProject = func(ctx context.Context, yard string, shared bool) error {
		if !slices.Contains([]string{testyardmigration.CurrentYard, testyardmigration.LegacyYard}, yard) || shared != record.Before.SharedImages {
			return domain.ErrPlanStale
		}
		for _, p := range record.Projects {
			if p.Project == "subyard-"+yard {
				scope := "current"
				if yard == testyardmigration.LegacyYard {
					scope = "legacy"
				}
				return children.applyOwnerImageProject(ctx, native, p, scope)
			}
		}
		return domain.ErrPlanStale
	}
	options.RestoreLegacyStop = func(ctx context.Context) error {
		p := record.TemporaryStop
		if p == nil || p.Project != "subyard-"+testyardmigration.LegacyYard || p.Instance != "yard-"+testyardmigration.LegacyYard {
			return domain.ErrPlanStale
		}
		return native.ApplyPreparedTemporaryStop(ctx, *p)
	}
	return nil
}
