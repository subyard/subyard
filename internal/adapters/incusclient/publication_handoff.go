package incusclient

import (
	"context"
	"maps"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
)

// ExchangePublicationHandoff changes one exact delivery sidecar using the same
// GET/ETag that proves the original target and v1 receipt. It cannot grant power
// or listener authority. A changed/foreign value is always retained.
func (client *Client) ExchangePublicationHandoff(ctx context.Context, h ownerpublication.Handoff, contracts []ownerpublication.Contract, before, after string, guard func() error) error {
	if _, err := ownerpublication.DecodeHandoff(h.Encode()); err != nil {
		return err
	}
	if before != "" {
		v, e := ownerpublication.DecodeHandoff(before)
		if e != nil || v.ID != h.ID {
			return domain.ErrPlanStale
		}
	}
	if after != "" {
		v, e := ownerpublication.DecodeHandoff(after)
		if e != nil || v.ID != h.ID {
			return domain.ErrPlanStale
		}
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	if err = client.validateServerExtensions(server); err != nil {
		return err
	}
	server = server.UseProject(h.Project)
	instance, etag, err := server.GetInstance(h.Instance)
	if err != nil {
		return normalizeError("get publication delivery", err)
	}
	if etag == "" || instance.Config[h.Key()] != before || instanceInfo(h.Project, instance).Config[h.Key()] != before {
		return domain.ErrPlanStale
	}
	if err = h.Check(instanceInfo(h.Project, instance), contracts, true); err != nil {
		return err
	}
	if guard != nil {
		if err = guard(); err != nil {
			return err
		}
	}
	instance.Config = maps.Clone(instance.Config)
	if after == "" {
		delete(instance.Config, h.Key())
	} else {
		instance.Config[h.Key()] = after
	}
	if err = beforeNative(ctx, server, "instance-publication-handoff", h.Project, h.Instance); err != nil {
		return err
	}
	operation, err := server.UpdateInstance(h.Instance, instance.Writable(), etag)
	if err != nil {
		return normalizeError("update publication delivery", err)
	}
	if err = waitNative(ctx, server, operation, "instance-publication-handoff", h.Project, h.Instance); err != nil {
		return normalizeOperationError("wait publication delivery", operation.Get(), err)
	}
	return nil
}
