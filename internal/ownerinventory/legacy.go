package ownerinventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

// LegacyService preserves the one-minor compatibility path for an explicitly
// configured legacy remote. Its records deliberately have no SSH trust and
// therefore can neither authorize HostID adoption nor create a trusted
// transport. Canonical OwnerHosts must be registered through host add.
type LegacyService struct {
	Store Connections
	Clock Clock
	Fetch FetchFunc
}

func (service LegacyService) Read(
	ctx context.Context, connection Connection, force bool,
) Result {
	if connection.Trust != nil {
		return Result{Err: errors.New("legacy inventory service rejects managed SSH trust")}
	}
	if connection.HostID != "" {
		fetch := service.Fetch
		if fetch != nil {
			fetch = func(fetchCtx context.Context, expected string) (inventory domain.OwnerInventory, err error) {
				inventory, err = service.Fetch(fetchCtx, expected)
				if err == nil && inventory.HostID != expected {
					err = fmt.Errorf(
						"%w: legacy owner HostID mismatch: connection is %q, response is %q",
						ErrIntegrity, expected, inventory.HostID,
					)
				}
				return inventory, err
			}
		}
		return (Service{
			Cache: Cache{Root: service.Store.Root}, Clock: service.Clock,
			Fetch: fetch,
		}).Read(ctx, connection.HostID, force)
	}
	if service.Fetch == nil {
		return Result{Err: errors.New("legacy owner inventory refresh is unavailable")}
	}
	inventory, err := service.Fetch(ctx, "")
	if err != nil {
		return Result{Err: err}
	}
	if err := inventory.Validate(); err != nil {
		return Result{Err: err}
	}
	now := time.Now().UTC()
	if service.Clock != nil {
		now = service.Clock.Now().UTC()
	}
	connection.HostID = inventory.HostID
	snapshot := Snapshot{FetchedAt: now, Inventory: inventory}
	if err := service.Store.RegisterLegacy(connection, snapshot); err != nil {
		return Result{Err: err}
	}
	return Result{Inventory: inventory, FetchedAt: now}
}
