package ownerpublication

import (
	"context"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// PublicationWorker has no power, CPU or guest-command capability. Delivery is
// a separate first pass so an original caller holding WithStart can observe ACK.
type PublicationWorker struct {
	Contracts []Contract
	Inventory func(context.Context) ([]ports.InstanceInfo, error)
	Consumer  func(context.Context, Consumer) error
	Ack       func(context.Context, Handoff, string, string) error
	Restore   func(context.Context, ports.InstanceInfo) ([]string, error)
}

func (w PublicationWorker) Run(ctx context.Context) (bool, error) {
	instances, err := w.Inventory(ctx)
	if err != nil {
		return false, err
	}
	for _, i := range instances {
		for key, raw := range i.LocalConfig {
			if !strings.HasPrefix(key, HandoffPrefix) || raw == "" {
				continue
			}
			h, e := DecodeHandoff(raw)
			if e != nil || key != h.Key() || h.Check(i, w.Contracts, true) != nil {
				continue
			}
			if e = w.Consumer(ctx, h.Consumer); e != nil {
				continue
			}
			if h.Ack == h.ID {
				continue
			}
			h.Ack = h.ID
			if e = w.Ack(ctx, h, raw, h.Encode()); e != nil {
				continue
			} // Preserve changed foreign/stale values.
		}
	}
	waiting := false
	for _, i := range instances {
		target := OriginalTarget{Project: i.Project, Name: i.Name, UUID: i.LocalConfig["volatile.uuid"], Kind: i.Type, Desired: domain.PowerRunning}
		if target.Check(i) != nil {
			continue
		}
		if len(Deferred(i)) == 0 {
			continue
		}
		remaining, e := w.Restore(ctx, i)
		if e != nil {
			return waiting, e
		}
		waiting = waiting || len(remaining) > 0
	}
	return waiting, nil
}
