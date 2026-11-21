package ownerinventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

type Snapshot struct {
	FetchedAt time.Time             `json:"fetchedAt"`
	Inventory domain.OwnerInventory `json:"inventory"`
}

type Cache struct {
	Root   string
	failIO func(string) error
}

func (cache Cache) Read(hostID string) (Snapshot, error) {
	if err := validateHostID(hostID); err != nil {
		return Snapshot{}, err
	}
	payload, err := os.ReadFile(cache.path(hostID))
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode owner inventory cache: %w", err)
	}
	if snapshot.FetchedAt.IsZero() {
		return Snapshot{}, errors.New("owner inventory cache has no fetchedAt")
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Inventory.HostID != hostID {
		return Snapshot{}, errors.New("owner inventory cache HostID mismatch")
	}
	return snapshot, nil
}

func (cache Cache) Write(snapshot Snapshot) error {
	if cache.failIO != nil {
		if err := cache.failIO(ownerIOCacheWrite); err != nil {
			return err
		}
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return err
	}
	if snapshot.FetchedAt.IsZero() {
		return errors.New("owner inventory fetchedAt is required")
	}
	path := cache.path(snapshot.Inventory.HostID)
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return writeOwnerRecord(path, payload, ".inventory-*", cache.failIO)
}

func (cache Cache) Invalidate(hostID string) error {
	if err := validateHostID(hostID); err != nil {
		return err
	}
	snapshot, err := cache.Read(hostID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	snapshot.FetchedAt = time.Unix(1, 0).UTC()
	return cache.Write(snapshot)
}

func (cache Cache) path(hostID string) string {
	return filepath.Join(cache.Root, "owners", hostID+".json")
}

func validateHostID(hostID string) error {
	inventory := domain.OwnerInventory{
		Schema: domain.OwnerInventorySchema, HostID: hostID, ObservedAt: time.Unix(1, 0),
	}
	if err := inventory.Validate(); err != nil {
		return fmt.Errorf("invalid cache HostID: %w", err)
	}
	return nil
}
