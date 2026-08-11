package ownerinventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const connectionRepairSchema = 1

type RepairPlan struct {
	OldHostID      string
	NewHostID      string
	OldFingerprint string
	NewFingerprint string

	oldConnection Connection
	newConnection Connection
	snapshot      Snapshot
	oldDigest     string
}

type connectionRepairJournal struct {
	SchemaVersion int        `json:"schemaVersion"`
	OldConnection Connection `json:"oldConnection"`
	NewConnection Connection `json:"newConnection"`
	Snapshot      Snapshot   `json:"snapshot"`
}

func (store Connections) PrepareRepair(
	hostID string, candidate SSHHostTrust, snapshot Snapshot,
) (RepairPlan, error) {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return RepairPlan{}, err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return RepairPlan{}, err
	}
	return store.prepareRepairLocked(hostID, candidate, snapshot)
}

func (store Connections) prepareRepairLocked(
	hostID string, candidate SSHHostTrust, snapshot Snapshot,
) (RepairPlan, error) {
	if err := candidate.Validate(); err != nil {
		return RepairPlan{}, err
	}
	if snapshot.FetchedAt.IsZero() {
		return RepairPlan{}, errors.New("repair snapshot fetchedAt is required")
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return RepairPlan{}, err
	}
	connections, err := store.list()
	if err != nil {
		return RepairPlan{}, err
	}
	var old Connection
	found := false
	for _, connection := range connections {
		if connection.HostID == hostID {
			old, found = connection, true
		}
		if connection.HostID == snapshot.Inventory.HostID && connection.HostID != hostID {
			return RepairPlan{}, fmt.Errorf("HostID %q is already registered", snapshot.Inventory.HostID)
		}
	}
	if !found {
		return RepairPlan{}, fmt.Errorf("OwnerHost %q is not registered", hostID)
	}
	oldFingerprint := "unmanaged"
	if old.Trust != nil {
		oldTrust, err := old.RequireTrust()
		if err != nil {
			return RepairPlan{}, err
		}
		oldFingerprint = oldTrust.Fingerprint
	}
	if snapshot.Inventory.HostID != hostID {
		if _, err := os.Lstat(store.cache().path(snapshot.Inventory.HostID)); err == nil {
			return RepairPlan{}, fmt.Errorf("OwnerHost %q inventory cache already exists", snapshot.Inventory.HostID)
		} else if !errors.Is(err, os.ErrNotExist) {
			return RepairPlan{}, err
		}
		if _, err := os.Lstat(store.routingPath(snapshot.Inventory.HostID)); err == nil {
			return RepairPlan{}, fmt.Errorf("OwnerHost %q routing state already exists", snapshot.Inventory.HostID)
		} else if !errors.Is(err, os.ErrNotExist) {
			return RepairPlan{}, err
		}
	}
	updated := old
	updated.HostID = snapshot.Inventory.HostID
	updated.Trust = &candidate
	digest, err := connectionDigest(old)
	if err != nil {
		return RepairPlan{}, err
	}
	return RepairPlan{
		OldHostID: hostID, NewHostID: snapshot.Inventory.HostID,
		OldFingerprint: oldFingerprint, NewFingerprint: candidate.Fingerprint,
		oldConnection: old, newConnection: updated, snapshot: snapshot, oldDigest: digest,
	}, nil
}

func (store Connections) ApplyRepair(plan RepairPlan) error {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return err
	}
	current, err := store.prepareRepairLocked(plan.OldHostID, *plan.newConnection.Trust, plan.snapshot)
	if err != nil {
		return err
	}
	if current.oldDigest != plan.oldDigest || current.NewHostID != plan.NewHostID ||
		current.NewFingerprint != plan.NewFingerprint {
		return errors.New("owner repair plan is stale")
	}
	if plan.OldHostID != plan.NewHostID {
		_, routeErr := os.Lstat(store.routingPath(plan.OldHostID))
		journal := hostIDAdoptionJournal{
			SchemaVersion: hostIDAdoptionSchema, OldConnection: plan.oldConnection,
			NewConnection: plan.newConnection, Snapshot: &plan.snapshot,
			RoutingWasPresent: routeErr == nil,
		}
		if routeErr != nil && !errors.Is(routeErr, os.ErrNotExist) {
			return routeErr
		}
		if err := store.writeHostIDAdoptionJournal(journal); err != nil {
			return err
		}
		return store.applyHostIDAdoptionLocked(journal)
	}
	journal := connectionRepairJournal{
		SchemaVersion: connectionRepairSchema, OldConnection: plan.oldConnection,
		NewConnection: plan.newConnection, Snapshot: plan.snapshot,
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if err := store.writeJournal(store.connectionRepairPath(), append(payload, '\n')); err != nil {
		return err
	}
	return store.applyConnectionRepairLocked(journal)
}

func connectionDigest(connection Connection) (string, error) {
	payload, err := json.Marshal(connection)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func (store Connections) connectionRepairPath() string {
	return filepath.Join(store.Root, "connection-repair.json")
}

func (store Connections) recoverConnectionRepairLocked() error {
	payload, err := os.ReadFile(store.connectionRepairPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(payload) > 8*1024*1024 {
		return errors.New("owner connection repair journal is too large")
	}
	var journal connectionRepairJournal
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("owner connection repair journal has trailing data")
	}
	if journal.SchemaVersion != connectionRepairSchema ||
		journal.OldConnection.HostID != journal.NewConnection.HostID ||
		journal.Snapshot.Inventory.HostID != journal.NewConnection.HostID {
		return errors.New("owner connection repair journal is invalid")
	}
	if err := journal.NewConnection.Validate(); err != nil {
		return err
	}
	return store.applyConnectionRepairLocked(journal)
}

func (store Connections) applyConnectionRepairLocked(journal connectionRepairJournal) error {
	if err := store.writeConnectionFile(journal.NewConnection); err != nil {
		return err
	}
	if err := store.cache().Write(journal.Snapshot); err != nil {
		return err
	}
	if err := store.hitIO(ownerIOJournalCleanup); err != nil {
		return err
	}
	if err := os.Remove(store.connectionRepairPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return store.syncDirectory(filepath.Dir(store.connectionRepairPath()))
}
