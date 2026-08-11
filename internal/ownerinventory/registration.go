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

const registrationSchema = 1

type registrationJournal struct {
	SchemaVersion int        `json:"schemaVersion"`
	Connection    Connection `json:"connection"`
	Snapshot      Snapshot   `json:"snapshot"`
}

type RegistrationPlan struct {
	HostID      string
	Destination string
	Fingerprint string

	connection Connection
	snapshot   Snapshot
	digest     string
}

func (store Connections) PrepareRegistration(
	connection Connection, snapshot Snapshot,
) (RegistrationPlan, error) {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return RegistrationPlan{}, err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return RegistrationPlan{}, err
	}
	return store.prepareRegistrationLocked(connection, snapshot)
}

func (store Connections) prepareRegistrationLocked(
	connection Connection, snapshot Snapshot,
) (RegistrationPlan, error) {
	if err := validateRegistration(connection, snapshot); err != nil {
		return RegistrationPlan{}, err
	}
	existing, err := store.list()
	if err != nil {
		return RegistrationPlan{}, err
	}
	var legacyConnection *Connection
	var legacySnapshot *Snapshot
	for _, current := range existing {
		if current.HostID == connection.HostID {
			if current.Destination != connection.Destination || current.Trust != nil {
				return RegistrationPlan{}, fmt.Errorf("HostID %q is already registered", connection.HostID)
			}
			copy := current
			legacyConnection = &copy
			cached, readErr := (Cache{Root: store.Root}).Read(connection.HostID)
			if readErr != nil {
				return RegistrationPlan{}, fmt.Errorf("read legacy OwnerHost %q inventory: %w", connection.HostID, readErr)
			}
			legacySnapshot = &cached
			connection.LegacyNames = append([]string(nil), current.LegacyNames...)
			if current.Yards != nil {
				connection.Yards = make(map[string]YardRoute, len(current.Yards))
				for name, route := range current.Yards {
					connection.Yards[name] = route
				}
			}
			continue
		}
		if current.Destination == connection.Destination {
			return RegistrationPlan{}, fmt.Errorf("destination %q is already registered as HostID %q", connection.Destination, current.HostID)
		}
	}
	for label, path := range map[string]string{
		"inventory cache": (Cache{Root: store.Root}).path(connection.HostID),
		"routing state":   store.routingPath(connection.HostID),
	} {
		if legacyConnection != nil {
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			return RegistrationPlan{}, fmt.Errorf("OwnerHost %q %s already exists", connection.HostID, label)
		} else if !errors.Is(err, os.ErrNotExist) {
			return RegistrationPlan{}, err
		}
	}
	payload, err := json.Marshal(struct {
		Connection       Connection
		Snapshot         Snapshot
		LegacyConnection *Connection
		LegacySnapshot   *Snapshot
	}{connection, snapshot, legacyConnection, legacySnapshot})
	if err != nil {
		return RegistrationPlan{}, err
	}
	digest := sha256.Sum256(payload)
	trust, _ := connection.RequireTrust()
	return RegistrationPlan{HostID: connection.HostID, Destination: connection.Destination,
		Fingerprint: trust.Fingerprint, connection: connection, snapshot: snapshot,
		digest: hex.EncodeToString(digest[:])}, nil
}

func (store Connections) ApplyRegistration(plan RegistrationPlan) error {
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
	current, err := store.prepareRegistrationLocked(plan.connection, plan.snapshot)
	if err != nil {
		return err
	}
	if current.digest != plan.digest || current.HostID != plan.HostID || current.Destination != plan.Destination || current.Fingerprint != plan.Fingerprint {
		return errors.New("owner registration plan is stale")
	}
	return store.registerLocked(plan.connection, plan.snapshot)
}

func (store Connections) Register(connection Connection, snapshot Snapshot) error {
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
	if _, err := store.prepareRegistrationLocked(connection, snapshot); err != nil {
		return err
	}
	return store.registerLocked(connection, snapshot)
}

// RegisterLegacy atomically persists the one-minor compatibility snapshot.
// The resulting connection is deliberately untrusted and may only be upgraded
// to managed trust by a confirmed canonical registration of the same endpoint
// and authoritative HostID.
func (store Connections) RegisterLegacy(connection Connection, snapshot Snapshot) error {
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
	if err := validateRegistrationState(connection, snapshot, false); err != nil {
		return err
	}
	existing, err := store.list()
	if err != nil {
		return err
	}
	for _, current := range existing {
		if current.HostID == connection.HostID || current.Destination == connection.Destination {
			return fmt.Errorf("legacy OwnerHost %q is already registered", connection.HostID)
		}
	}
	for _, path := range []string{(Cache{Root: store.Root}).path(connection.HostID), store.routingPath(connection.HostID)} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("legacy OwnerHost %q state already exists", connection.HostID)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return store.registerLocked(connection, snapshot)
}

func (store Connections) registerLocked(connection Connection, snapshot Snapshot) error {
	journal := registrationJournal{
		SchemaVersion: registrationSchema, Connection: connection, Snapshot: snapshot,
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if err := store.writeJournal(store.registrationPath(), append(payload, '\n')); err != nil {
		return err
	}
	return store.applyRegistrationLocked(journal)
}

func validateRegistration(connection Connection, snapshot Snapshot) error {
	return validateRegistrationState(connection, snapshot, true)
}

func validateRegistrationState(connection Connection, snapshot Snapshot, requireTrust bool) error {
	if err := connection.Validate(); err != nil {
		return err
	}
	if requireTrust {
		if _, err := connection.RequireTrust(); err != nil {
			return err
		}
	} else if connection.Trust != nil {
		return errors.New("legacy owner registration must not contain managed SSH trust")
	}
	if snapshot.FetchedAt.IsZero() {
		return errors.New("owner inventory fetchedAt is required")
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return err
	}
	if snapshot.Inventory.HostID != connection.HostID {
		return errors.New("registration connection and inventory HostID differ")
	}
	return nil
}

func (store Connections) registrationPath() string {
	return filepath.Join(store.Root, "registration.json")
}

func (store Connections) recoverRegistrationLocked() error {
	payload, err := os.ReadFile(store.registrationPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(payload) > 8*1024*1024 {
		return errors.New("owner registration journal is too large")
	}
	var journal registrationJournal
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("owner registration journal has trailing data")
	}
	if journal.SchemaVersion != registrationSchema {
		return errors.New("unsupported owner registration journal schema")
	}
	if err := validateRegistrationState(journal.Connection, journal.Snapshot, journal.Connection.Trust != nil); err != nil {
		return fmt.Errorf("invalid owner registration journal: %w", err)
	}
	return store.applyRegistrationLocked(journal)
}

func (store Connections) applyRegistrationLocked(journal registrationJournal) error {
	if err := store.writeConnectionFile(journal.Connection); err != nil {
		return err
	}
	if err := store.cache().Write(journal.Snapshot); err != nil {
		return err
	}
	if err := store.hitIO(ownerIOJournalCleanup); err != nil {
		return err
	}
	if err := os.Remove(store.registrationPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return store.syncDirectory(filepath.Dir(store.registrationPath()))
}

func (store Connections) recoverPendingLocked() error {
	if err := store.recoverRegistrationLocked(); err != nil {
		return err
	}
	if err := store.recoverConnectionRepairLocked(); err != nil {
		return err
	}
	if err := store.recoverHostIDAdoptionLocked(); err != nil {
		return err
	}
	return store.recoverRemovalLocked()
}

// Recover completes any durable owner-inventory transaction left by an
// interrupted process. CLI dispatch calls this before unrelated work so a
// journal never remains dependent on a later inventory-specific command.
func (store Connections) Recover() error {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return err
	}
	defer release()
	return store.recoverPendingLocked()
}
