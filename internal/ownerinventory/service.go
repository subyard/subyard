package ownerinventory

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const Freshness = 30 * time.Second

type Result struct {
	Inventory domain.OwnerInventory
	FetchedAt time.Time
	Stale     bool
	Err       error
}

type Service struct {
	Cache Cache
	Store *Connections
	Clock Clock
	Fetch FetchFunc
	TTL   time.Duration
}

func (service Service) Read(ctx context.Context, hostID string, force bool) Result {
	now := time.Now()
	if service.Clock != nil {
		now = service.Clock.Now()
	}
	ttl := service.TTL
	if ttl <= 0 {
		ttl = Freshness
	}
	cached, cacheErr := service.Cache.Read(hostID)
	if cacheErr == nil && !force && now.Sub(cached.FetchedAt) <= ttl {
		return Result{Inventory: cached.Inventory, FetchedAt: cached.FetchedAt}
	}
	if service.Fetch == nil {
		return staleResult(cached, cacheErr, errors.New("owner inventory refresh is unavailable"))
	}
	inventory, err := service.Fetch(ctx, hostID)
	if err != nil {
		return staleResult(cached, cacheErr, err)
	}
	snapshot := Snapshot{FetchedAt: now.UTC(), Inventory: inventory}
	var writeErr error
	if service.Store != nil {
		writeErr = service.Store.WriteSnapshot(snapshot)
	} else {
		writeErr = service.Cache.Write(snapshot)
	}
	if writeErr != nil {
		return staleResult(cached, cacheErr, writeErr)
	}
	return Result{Inventory: inventory, FetchedAt: snapshot.FetchedAt}
}

func staleResult(cached Snapshot, cacheErr, refreshErr error) Result {
	if cacheErr == nil {
		return Result{
			Inventory: cached.Inventory, FetchedAt: cached.FetchedAt, Stale: true, Err: refreshErr,
		}
	}
	if errors.Is(cacheErr, os.ErrNotExist) {
		cacheErr = nil
	}
	if cacheErr != nil {
		refreshErr = errors.Join(refreshErr, cacheErr)
	}
	return Result{Err: refreshErr}
}
