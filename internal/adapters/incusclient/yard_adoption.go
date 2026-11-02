package incusclient

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

// AdoptProject uses the ETag returned by the same read that checked the exact
// approved configuration. A concurrent project edit cannot be overwritten.
func (client *Client) AdoptProject(ctx context.Context, project string, before, metadata map[string]string) error {
	if !domain.SafeName(project) || project == "default" || metadata["user.subyard.installation"] != domain.AdoptedInstallation || metadata["user.subyard.adopted_uuid"] == "" {
		return domain.ErrPlanStale
	}
	server, err := client.reconcileServer(ctx)
	if err != nil {
		return err
	}
	actual, etag, err := server.GetProject(project)
	if err != nil {
		return normalizeError("read adoption project", err)
	}
	if actual == nil || actual.Name != project || etag == "" || !maps.Equal(actual.Config, before) {
		return domain.ErrPlanStale
	}
	desired := maps.Clone(before)
	for key, value := range metadata {
		if !strings.HasPrefix(key, "user.subyard.") || key == ownerCreateTokenKey {
			return domain.ErrPlanStale
		}
		desired[key] = value
	}
	if maps.Equal(before, desired) {
		return nil
	}
	request := actual.Writable()
	request.Config = desired
	if err := server.UpdateProject(project, request, etag); err != nil {
		return errors.Join(domain.ErrPlanStale, normalizeError("publish adoption project", err))
	}
	verified, _, err := server.GetProject(project)
	if err != nil {
		return normalizeError("verify adoption project", err)
	}
	if verified == nil || !maps.Equal(verified.Config, desired) {
		return domain.ErrPlanStale
	}
	return nil
}

// AdoptRegisteredInstance retains the exact local device/config snapshot through
// the native ETag CAS. It never changes workload devices or unrelated settings.
func (client *Client) AdoptRegisteredInstance(ctx context.Context, before ports.InstanceInfo, metadata map[string]string) error {
	if before.Config["volatile.uuid"] == "" || before.Project == "default" {
		return domain.ErrPlanStale
	}
	server, err := client.reconcileServer(ctx)
	if err != nil {
		return err
	}
	native := server.UseProject(before.Project)
	actual, etag, err := native.GetInstance(before.Name)
	if err != nil {
		return err
	}
	if actual == nil || actual.Name != before.Name || instanceInfo(before.Project, actual).Type != before.Type || actual.Config["volatile.uuid"] != before.Config["volatile.uuid"] || !slices.Equal(actual.Profiles, []string{"default"}) || etag == "" || !maps.Equal(actual.Config, before.LocalConfig) || !maps.Equal(actual.ExpandedConfig, before.Config) || !maps.EqualFunc(actual.ExpandedDevices, before.Devices, func(left, right map[string]string) bool { return maps.Equal(left, right) }) || !maps.EqualFunc(actual.Devices, before.LocalDevices, func(left, right map[string]string) bool { return maps.Equal(left, right) }) {
		return domain.ErrPlanStale
	}
	desired := maps.Clone(actual.Config)
	for key, value := range metadata {
		if !(strings.HasPrefix(key, "user.subyard.resource.") || key == "user.subyard.disk_tmp" && value == "v1") || value == "" || desired[key] != "" && desired[key] != value {
			return domain.ErrPlanStale
		}
		if strings.HasPrefix(key, "user.subyard.resource.") {
			device := actual.Devices[strings.TrimPrefix(key, "user.subyard.resource.")]
			if device["type"] != "proxy" || value != (resource.ProxyContract{}).OwnershipValue(device) {
				return domain.ErrPlanStale
			}
		}
		desired[key] = value
	}
	if maps.Equal(actual.Config, desired) {
		return nil
	}
	request := actual.Writable()
	request.Config = desired
	operation, err := native.UpdateInstance(before.Name, request, etag)
	if err != nil {
		return err
	}
	return waitNative(ctx, native, operation, "instance-config", before.Project, before.Name)
}
