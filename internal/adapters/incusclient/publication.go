package incusclient

import (
	"context"
	"maps"
	"net/netip"

	"github.com/Subyard/Subyard/internal/ownerpublication"
)

// ReconcilePublications atomically changes only validated optional publications.
// It does not change power, and a failed restore leaves the durable receipt intact.
func (client *Client) ReconcilePublications(ctx context.Context, project, name string, contracts []ownerpublication.Contract, addresses []netip.Addr, beforeWrite ...func() error) ([]string, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	if err = client.validateServerExtensions(server); err != nil {
		return nil, err
	}
	server = server.UseProject(project)
	instance, etag, err := server.GetInstance(name)
	if err != nil {
		return nil, normalizeError("get publication instance", err)
	}
	config, devices, waiting, err := ownerpublication.Plan(instanceInfo(project, instance), contracts, addresses)
	if err != nil {
		return nil, err
	}
	if maps.Equal(config, instance.Config) && maps.EqualFunc(devices, instance.Devices, func(left, right map[string]string) bool {
		return maps.Equal(left, right)
	}) {
		return waiting, nil
	}
	for _, check := range beforeWrite {
		if err := check(); err != nil {
			return nil, err
		}
	}
	instance.Config, instance.Devices = config, devices
	if err := beforeNative(ctx, server, "instance-publication", project, name); err != nil {
		return nil, err
	}
	operation, err := server.UpdateInstance(name, instance.Writable(), etag)
	if err != nil {
		return nil, normalizeError("update optional publication", err)
	}
	if err = waitNative(ctx, server, operation, "instance-publication", project, name); err != nil {
		return nil, normalizeOperationError("wait for optional publication update", operation.Get(), err)
	}
	return waiting, nil
}
