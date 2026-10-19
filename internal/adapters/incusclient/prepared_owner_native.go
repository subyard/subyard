package incusclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

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
		if err != nil || !maps.Equal(actual.Config, p.Config) {
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
	observer := ports.NativeOperationObserverFrom(ctx)
	var ordinal uint64
	if observer != nil {
		serverFact, err := nativeOperation(server, "project-create", p.Project, p.Project)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(struct {
			Kind, Server, Project string
			Shared                bool
		}{"project-create", serverFact.Server, p.Project, true})
		ordinal, err = observer.CommandStarted("incus-project-create", fmt.Sprintf("%x", sha256.Sum256(raw)))
		if err != nil {
			return err
		}
	}
	err = server.CreateProject(api.ProjectsPost{Name: p.Project, ProjectPut: api.ProjectPut{Config: map[string]string{"features.images": "false"}}})
	if err != nil {
		if observer != nil {
			observer.Uncertain()
		}
		return normalizeError("create prepared owner project", err)
	}
	if observer != nil {
		return observer.CommandTerminal(ordinal)
	}
	return nil
}

func (client *Client) PrepareTemporaryStop(ctx context.Context, project, instance string) (ports.PreparedTemporaryStop, error) {
	p := ports.PreparedTemporaryStop{Project: project, Instance: instance}
	if !domain.SafeName(project) || !domain.SafeName(instance) {
		return p, domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return p, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return p, err
	}
	actual, _, err := server.UseProject(project).GetInstance(instance)
	if err != nil {
		return p, normalizeError("prepare temporary stop", err)
	}
	p.UUID = actual.Config["volatile.uuid"]
	if p.UUID == "" {
		p.UUID = actual.ExpandedConfig["volatile.uuid"]
	}
	p.Desired = actual.Config["user.subyard.desired_power"]
	if p.UUID == "" || !strings.EqualFold(actual.Status, "stopped") || p.Desired != "running" {
		return p, domain.ErrPlanStale
	}
	return p, nil
}
func (client *Client) ApplyPreparedTemporaryStop(ctx context.Context, p ports.PreparedTemporaryStop) error {
	if !domain.SafeName(p.Project) || !domain.SafeName(p.Instance) || p.UUID == "" || p.Desired != "running" {
		return domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return err
	}
	server = server.UseProject(p.Project)
	actual, _, err := server.GetInstance(p.Instance)
	if err != nil {
		return normalizeError("read prepared temporary stop", err)
	}
	uuid := actual.Config["volatile.uuid"]
	if uuid == "" {
		uuid = actual.ExpandedConfig["volatile.uuid"]
	}
	if uuid != p.UUID || actual.Config["user.subyard.desired_power"] != p.Desired {
		return domain.ErrPlanStale
	}
	if strings.EqualFold(actual.Status, "stopped") {
		return nil
	}
	if !strings.EqualFold(actual.Status, "running") {
		return domain.ErrPlanStale
	}
	if err := beforeNative(ctx, server, "instance-power", p.Project, p.Instance); err != nil {
		return err
	}
	operation, err := server.UpdateInstanceState(p.Instance, api.InstanceStatePut{Action: "stop", Timeout: -1}, "")
	if err != nil {
		if observer := ports.NativeOperationObserverFrom(ctx); observer != nil {
			observer.Uncertain()
		}
		return normalizeError("stop prepared temporary owner", err)
	}
	if operation == nil {
		if observer := ports.NativeOperationObserverFrom(ctx); observer != nil {
			observer.Uncertain()
		}
		return errors.New("temporary stop acceptance unavailable")
	}
	return waitNative(ctx, server, operation, "instance-power", p.Project, p.Instance)
}
