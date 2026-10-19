package incusclient

import (
	"context"
	"errors"
	"maps"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

// PrepareReadOnlyDisk captures one exact local device mutation. It never
// copies expanded profile devices into the local instance configuration.
func (client *Client) PrepareReadOnlyDisk(ctx context.Context, project, instance, device, source, destination string) (ports.PreparedReadOnlyDisk, error) {
	p := ports.PreparedReadOnlyDisk{Project: project, Instance: instance, Device: device, Source: source, Destination: destination}
	if !domain.SafeName(project) || !domain.SafeName(instance) || !domain.SafeName(device) || !filepath.IsAbs(source) || !filepath.IsAbs(destination) {
		return p, errors.New("invalid prepared disk scope")
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
		return p, normalizeError("prepare disk instance", err)
	}
	p.UUID = actual.Config["volatile.uuid"]
	if p.UUID == "" {
		p.UUID = actual.ExpandedConfig["volatile.uuid"]
	}
	if p.UUID == "" {
		return p, errors.New("prepared disk instance UUID unavailable")
	}
	p.Before = maps.Clone(actual.Devices[device])
	if inherited, ok := actual.ExpandedDevices[device]; ok && !maps.Equal(inherited, p.Before) {
		return p, errors.New("prepared disk conflicts with inherited device")
	}
	if p.Before != nil && !maps.Equal(p.Before, readOnlyDisk(p)) {
		return p, errors.New("prepared disk has foreign configuration")
	}
	return p, nil
}

func readOnlyDisk(p ports.PreparedReadOnlyDisk) map[string]string {
	return map[string]string{"type": "disk", "source": p.Source, "path": p.Destination, "readonly": "true"}
}

func (client *Client) readPreparedReadOnlyDisk(ctx context.Context, p ports.PreparedReadOnlyDisk) (incus.InstanceServer, *api.Instance, string, error) {
	if !domain.SafeName(p.Project) || !domain.SafeName(p.Instance) || !domain.SafeName(p.Device) || p.UUID == "" || !filepath.IsAbs(p.Source) || !filepath.IsAbs(p.Destination) || p.Before != nil && !maps.Equal(p.Before, readOnlyDisk(p)) {
		return nil, nil, "", domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, nil, "", err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return nil, nil, "", err
	}
	server = server.UseProject(p.Project)
	actual, etag, err := server.GetInstance(p.Instance)
	if err != nil {
		return nil, nil, "", normalizeError("read prepared disk instance", err)
	}
	uuid := actual.Config["volatile.uuid"]
	if uuid == "" {
		uuid = actual.ExpandedConfig["volatile.uuid"]
	}
	if uuid != p.UUID || !maps.Equal(actual.Devices[p.Device], p.Before) {
		return nil, nil, "", domain.ErrPlanStale
	}
	if inherited, ok := actual.ExpandedDevices[p.Device]; ok && !maps.Equal(inherited, p.Before) {
		return nil, nil, "", domain.ErrPlanStale
	}
	return server, actual, etag, nil
}

func (client *Client) CheckPreparedReadOnlyDisk(ctx context.Context, p ports.PreparedReadOnlyDisk) error {
	_, _, _, err := client.readPreparedReadOnlyDisk(ctx, p)
	return err
}

func (client *Client) ApplyPreparedReadOnlyDisk(ctx context.Context, p ports.PreparedReadOnlyDisk) error {
	server, actual, etag, err := client.readPreparedReadOnlyDisk(ctx, p)
	if err != nil {
		return err
	}
	if maps.Equal(p.Before, readOnlyDisk(p)) {
		return nil
	}
	if actual.Devices == nil {
		actual.Devices = map[string]map[string]string{}
	}
	actual.Devices[p.Device] = readOnlyDisk(p)
	return updateInstanceDevices(ctx, server, p.Instance, actual, etag)
}
