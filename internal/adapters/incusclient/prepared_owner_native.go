package incusclient

import (
	"context"
	"maps"
	"net/http"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/lxc/incus/v6/shared/api"
)

func (client *Client) PrepareImageProject(ctx context.Context, project string, shared bool) (ports.PreparedImageProject, error) {
	p := ports.PreparedImageProject{Project: project, SharedImages: shared}
	if !domain.SafeName(project) {
		return p, domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return p, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return p, err
	}
	actual, _, err := server.GetProject(project)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		p.Absent = true
		return p, nil
	}
	if err != nil {
		return p, normalizeError("prepare owner project", err)
	}
	if actual == nil || actual.Name != project {
		return p, domain.ErrPlanStale
	}
	p.Config = maps.Clone(actual.Config)
	if p.Config["features.images"] != "" && p.Config["features.images"] != "true" && p.Config["features.images"] != "false" || shared != (p.Config["features.images"] == "false") {
		return p, domain.ErrPlanStale
	}
	return p, nil
}
func (client *Client) ApplyPreparedImageProject(ctx context.Context, p ports.PreparedImageProject) error {
	if !domain.SafeName(p.Project) || p.Absent && p.Config != nil {
		return domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return err
	}
	actual, _, err := server.GetProject(p.Project)
	if !p.Absent {
		if err != nil || actual == nil || actual.Name != p.Project || !maps.Equal(actual.Config, p.Config) {
			return domain.ErrPlanStale
		}
		return nil
	}
	if !api.StatusErrorCheck(err, http.StatusNotFound) {
		return domain.ErrPlanStale
	}
	// The existing migration creates a project only to preserve shared images.
	if !p.SharedImages {
		return nil
	}
	// A new image namespace is also the future yard project. Publish the
	// canonical installation boundary in its original atomic create, never
	// claim an already existing project by adding ownership afterward.
	if len(p.InitialConfig) == 0 {
		return domain.ErrPlanStale
	}
	request, err := ownerImageProjectRequest(p)
	if err != nil {
		return err
	}
	if err := server.CreateProject(request); err != nil {
		return normalizeError("create prepared owner project", err)
	}
	return nil
}
