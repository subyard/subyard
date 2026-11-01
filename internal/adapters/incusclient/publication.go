package incusclient

import (
	"context"
	"maps"
	"net/netip"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
)

// ReconcilePublications atomically changes only validated optional publications.
// It does not change power, and a failed restore leaves the durable receipt intact.
func (client *Client) ReconcilePublications(ctx context.Context, project, name string, contracts []ownerpublication.Contract, addresses []netip.Addr, beforeWrite ...func() error) ([]string, error) {
	return client.reconcilePublications(ctx, project, name, "", false, domain.YardVM, contracts, addresses, beforeWrite...)
}

// ReconcilePublicationsIf retains the original VM in the same GET/ETag update.
func (client *Client) ReconcilePublicationsIf(ctx context.Context, project, name, uuid string, contracts []ownerpublication.Contract, addresses []netip.Addr, beforeWrite ...func() error) ([]string, error) {
	if uuid == "" {
		return nil, domain.ErrPlanStale
	}
	return client.reconcilePublications(ctx, project, name, uuid, false, domain.YardVM, contracts, addresses, beforeWrite...)
}

// ReconcileOriginalPublicationsIf retains original kind/UUID before intent commit.
func (client *Client) ReconcileOriginalPublicationsIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, contracts []ownerpublication.Contract, addresses []netip.Addr, beforeWrite ...func() error) ([]string, error) {
	if uuid == "" || kind != domain.YardVM && kind != domain.YardContainer {
		return nil, domain.ErrPlanStale
	}
	return client.reconcilePublications(ctx, project, name, uuid, false, kind, contracts, addresses, beforeWrite...)
}

// ReconcileRunningPublicationsIf restores only the original finalized running instance.
func (client *Client) ReconcileRunningPublicationsIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, contracts []ownerpublication.Contract, addresses []netip.Addr, beforeWrite ...func() error) ([]string, error) {
	if uuid == "" || kind != domain.YardVM && kind != domain.YardContainer {
		return nil, domain.ErrPlanStale
	}
	return client.reconcilePublications(ctx, project, name, uuid, true, kind, contracts, addresses, beforeWrite...)
}

func (client *Client) reconcilePublications(ctx context.Context, project, name, uuid string, running bool, kind domain.YardKind, contracts []ownerpublication.Contract, addresses []netip.Addr, beforeWrite ...func() error) ([]string, error) {
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
	check := func(info ports.InstanceInfo) error {
		if running {
			return (ownerpublication.OriginalTarget{Project: project, Name: name, UUID: uuid, Kind: kind, Desired: domain.PowerRunning}).Check(info)
		}
		if kind == domain.YardVM {
			return checkVMCPUUUID(info, project, name, uuid)
		}
		return checkOriginalKindUUID(info, project, name, uuid, kind)
	}
	if err := check(instanceInfo(project, instance)); err != nil {
		return nil, err
	}
	postcheck := func() error {
		if !running {
			if kind == domain.YardVM {
				return client.checkVMUUID(ctx, project, name, uuid)
			}
			return client.checkOriginalKindUUID(ctx, project, name, uuid, kind)
		}
		info, err := client.Instance(ctx, project, name)
		if err != nil {
			return err
		}
		return check(info)
	}
	config, devices, waiting, err := ownerpublication.Plan(instanceInfo(project, instance), contracts, addresses)
	if err != nil {
		return nil, err
	}
	if maps.Equal(config, instance.Config) && maps.EqualFunc(devices, instance.Devices, func(left, right map[string]string) bool {
		return maps.Equal(left, right)
	}) {
		return waiting, postcheck()
	}
	if uuid != "" && etag == "" {
		return nil, domain.ErrPlanStale
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
	return waiting, postcheck()
}
