package ownerinventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const removalSchema = 1

type removalJournal struct {
	SchemaVersion int    `json:"schemaVersion"`
	HostID        string `json:"hostId"`
}

type RemovalPlan struct {
	HostID     string
	connection Connection
	snapshot   Snapshot
	digest     string
}

func (store Connections) PrepareRemoval(
	connection Connection, snapshot Snapshot,
) (RemovalPlan, error) {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return RemovalPlan{}, err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return RemovalPlan{}, err
	}
	return store.prepareRemovalLocked(connection, snapshot)
}

func (store Connections) prepareRemovalLocked(connection Connection, snapshot Snapshot) (RemovalPlan, error) {
	if snapshot.FetchedAt.IsZero() || snapshot.Inventory.HostID != connection.HostID {
		return RemovalPlan{}, errors.New("removal requires a fresh authoritative snapshot for the registered HostID")
	}
	if age := store.currentTime().Sub(snapshot.FetchedAt); age > Freshness || age < -Freshness {
		return RemovalPlan{}, errors.New("removal snapshot is stale; refresh it before removal")
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return RemovalPlan{}, err
	}
	registered, err := store.list()
	if err != nil {
		return RemovalPlan{}, err
	}
	found := false
	var current Connection
	for _, candidate := range registered {
		if candidate.HostID == connection.HostID {
			found = candidate.Destination == connection.Destination
			if !found {
				return RemovalPlan{}, errors.New("owner connection changed before removal")
			}
			current = candidate
		}
	}
	if !found {
		return RemovalPlan{}, fmt.Errorf("OwnerHost %q is not registered", connection.HostID)
	}
	currentDigest, err := connectionDigest(current)
	if err != nil {
		return RemovalPlan{}, err
	}
	requestedDigest, err := connectionDigest(connection)
	if err != nil {
		return RemovalPlan{}, err
	}
	if currentDigest != requestedDigest {
		return RemovalPlan{}, errors.New("owner connection changed before removal")
	}
	if err := ensureInventoryHasNoProjects(snapshot.Inventory); err != nil {
		return RemovalPlan{}, err
	}
	if err := ensureNoRoutingProjects(store.Root, connection.HostID); err != nil {
		return RemovalPlan{}, err
	}
	payload, err := json.Marshal(struct {
		Connection Connection
		Snapshot   Snapshot
	}{connection, snapshot})
	if err != nil {
		return RemovalPlan{}, err
	}
	digest := sha256.Sum256(payload)
	return RemovalPlan{HostID: connection.HostID, connection: connection, snapshot: snapshot,
		digest: hex.EncodeToString(digest[:])}, nil
}

func (store Connections) ApplyRemoval(
	ctx context.Context, plan RemovalPlan,
	refresh func(context.Context, Connection) (Snapshot, error),
) (Connection, error) {
	if err := store.validateRemovalPlan(plan); err != nil {
		return Connection{}, err
	}
	if refresh == nil {
		return Connection{}, errors.New("owner removal requires a fresh authoritative inventory refresh")
	}
	releaseMutation, err := store.acquireHostMutation(ctx, plan.HostID, true, false)
	if err != nil {
		return Connection{}, err
	}
	defer releaseMutation()
	if err := store.validateRemovalPlan(plan); err != nil {
		return Connection{}, err
	}

	connectionsMu.Lock()
	release, err := store.lock()
	if err != nil {
		connectionsMu.Unlock()
		return Connection{}, err
	}
	err = store.recoverPendingLocked(plan.HostID)
	if err == nil {
		err = store.validateConnectionLocked(plan.connection)
	}
	release()
	connectionsMu.Unlock()
	if err != nil {
		return Connection{}, err
	}

	snapshot, err := refresh(ctx, plan.connection)
	if err != nil {
		return Connection{}, err
	}

	connectionsMu.Lock()
	release, err = store.lock()
	if err != nil {
		connectionsMu.Unlock()
		return Connection{}, err
	}
	err = store.recoverPendingLocked(plan.HostID)
	if err == nil {
		err = store.validateConnectionLocked(plan.connection)
	}
	if err == nil {
		_, err = store.prepareRemovalLocked(plan.connection, snapshot)
	}
	if err == nil {
		err = store.writeRemovalJournal(plan.HostID)
	}
	if err == nil {
		err = store.applyRemovalLocked(plan.HostID)
	}
	release()
	connectionsMu.Unlock()
	if err != nil {
		return Connection{}, err
	}
	return plan.connection, nil
}

func (store Connections) validateRemovalPlan(plan RemovalPlan) error {
	if plan.HostID == "" || plan.HostID != plan.connection.HostID {
		return errors.New("owner removal plan is invalid")
	}
	current, err := store.removalPlanFor(plan.connection, plan.snapshot)
	if err != nil {
		return err
	}
	if current.digest != plan.digest {
		return errors.New("owner removal plan is stale")
	}
	return nil
}

func (store Connections) removalPlanFor(connection Connection, snapshot Snapshot) (RemovalPlan, error) {
	if snapshot.FetchedAt.IsZero() || snapshot.Inventory.HostID != connection.HostID {
		return RemovalPlan{}, errors.New("removal requires a fresh authoritative snapshot for the registered HostID")
	}
	if age := store.currentTime().Sub(snapshot.FetchedAt); age > Freshness || age < -Freshness {
		return RemovalPlan{}, errors.New("removal snapshot is stale; refresh it before removal")
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return RemovalPlan{}, err
	}
	payload, err := json.Marshal(struct {
		Connection Connection
		Snapshot   Snapshot
	}{connection, snapshot})
	if err != nil {
		return RemovalPlan{}, err
	}
	digest := sha256.Sum256(payload)
	return RemovalPlan{HostID: connection.HostID, connection: connection, snapshot: snapshot,
		digest: hex.EncodeToString(digest[:])}, nil
}

func (store Connections) removalPath() string {
	return filepath.Join(store.Root, "removal.json")
}

func (store Connections) writeRemovalJournal(hostID string) error {
	journal := removalJournal{SchemaVersion: removalSchema, HostID: hostID}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return store.writeJournal(store.removalPath(), append(payload, '\n'))
}

func (store Connections) recoverRemovalLocked(heldHostIDs ...string) error {
	payload, err := os.ReadFile(store.removalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(payload) > 8*1024*1024 {
		return errors.New("owner removal journal is too large")
	}
	var journal removalJournal
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("owner removal journal has trailing data")
	}
	if journal.SchemaVersion != removalSchema || validateHostID(journal.HostID) != nil {
		return errors.New("owner removal journal is invalid")
	}
	release := func() {}
	if !containsHostID(heldHostIDs, journal.HostID) {
		var lockErr error
		release, lockErr = store.acquireHostMutation(context.Background(), journal.HostID, true, true)
		if lockErr != nil {
			return lockErr
		}
	}
	defer release()
	return store.applyRemovalLocked(journal.HostID)
}

func (store Connections) applyRemovalLocked(hostID string) error {
	if err := store.hitIO(ownerIOStateDelete); err != nil {
		return err
	}
	connectionPath := filepath.Join(store.Root, "connections", hostID+".json")
	cachePath := store.cache().path(hostID)
	routingPath := store.routingPath(hostID)
	for _, path := range []string{connectionPath, cachePath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(routingPath); err != nil {
		return err
	}
	for _, directory := range []string{
		filepath.Dir(connectionPath), filepath.Dir(cachePath), filepath.Dir(routingPath),
	} {
		if err := store.syncDirectory(directory); err != nil {
			return err
		}
	}
	if err := store.hitIO(ownerIOJournalCleanup); err != nil {
		return err
	}
	if err := os.Remove(store.removalPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return store.syncDirectory(filepath.Dir(store.removalPath()))
}
