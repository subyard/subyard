package ownerinventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
)

const hostIDAdoptionSchema = 1

type IdentityProof struct {
	ExpectedHostID   string
	ObservedHostID   string
	Destination      string
	TrustFingerprint string
}

type hostIDAdoptionJournal struct {
	SchemaVersion     int        `json:"schemaVersion"`
	OldConnection     Connection `json:"oldConnection"`
	NewConnection     Connection `json:"newConnection"`
	Snapshot          *Snapshot  `json:"snapshot,omitempty"`
	RoutingWasPresent bool       `json:"routingWasPresent"`
}

func (store Connections) AdoptHostID(proof IdentityProof, snapshot Snapshot) error {
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
	inventory := snapshot.Inventory
	if proof.TrustFingerprint == "" || proof.ExpectedHostID == proof.ObservedHostID ||
		inventory.HostID != proof.ObservedHostID {
		return errors.New("owner HostID adoption lacks authenticated identity continuity")
	}
	if err := validateHostID(proof.ExpectedHostID); err != nil {
		return err
	}
	if err := validateHostID(proof.ObservedHostID); err != nil {
		return err
	}
	if !domain.SafeSSHTarget(proof.Destination) {
		return fmt.Errorf("invalid owner destination %q", proof.Destination)
	}
	connections, err := store.list()
	if err != nil {
		return err
	}
	var old Connection
	found := false
	for _, connection := range connections {
		if connection.HostID == proof.ObservedHostID {
			return fmt.Errorf("HostID %q is already registered", proof.ObservedHostID)
		}
		if connection.HostID == proof.ExpectedHostID {
			old, found = connection, true
		}
	}
	if !found {
		return fmt.Errorf("owner HostID %q is not registered", proof.ExpectedHostID)
	}
	if old.Destination != proof.Destination {
		return errors.New("owner HostID adoption destination does not match the registered connection")
	}
	trust, err := old.RequireTrust()
	if err != nil || trust.Fingerprint != proof.TrustFingerprint {
		return errors.New("owner HostID adoption lacks verified SSH host trust continuity")
	}
	updated := old
	updated.HostID = proof.ObservedHostID
	if snapshot.FetchedAt.IsZero() {
		return errors.New("owner HostID adoption requires the authoritative fetch time")
	}
	oldRouting := store.routingPath(proof.ExpectedHostID)
	newRouting := store.routingPath(proof.ObservedHostID)
	if _, err := os.Lstat(store.cache().path(proof.ObservedHostID)); err == nil {
		return fmt.Errorf("OwnerHost %q inventory cache already exists", proof.ObservedHostID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, oldRoutingErr := os.Lstat(oldRouting)
	routingWasPresent := oldRoutingErr == nil
	if oldRoutingErr != nil && !errors.Is(oldRoutingErr, os.ErrNotExist) {
		return oldRoutingErr
	}
	if _, err := os.Lstat(newRouting); err == nil {
		return fmt.Errorf("OwnerHost %q routing state already exists", proof.ObservedHostID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	journal := hostIDAdoptionJournal{
		SchemaVersion: hostIDAdoptionSchema, OldConnection: old,
		NewConnection: updated, Snapshot: &snapshot, RoutingWasPresent: routingWasPresent,
	}
	if err := store.writeHostIDAdoptionJournal(journal); err != nil {
		return err
	}
	return store.applyHostIDAdoptionLocked(journal)
}

func (store Connections) recoverHostIDAdoptionLocked() error {
	journal, exists, err := store.readHostIDAdoptionJournal()
	if err != nil || !exists {
		return err
	}
	return store.applyHostIDAdoptionLocked(journal)
}

func (store Connections) applyHostIDAdoptionLocked(journal hostIDAdoptionJournal) error {
	if err := store.writeConnectionFile(journal.NewConnection); err != nil {
		return err
	}
	if journal.Snapshot != nil {
		if err := store.cache().Write(*journal.Snapshot); err != nil {
			return err
		}
	}
	if journal.RoutingWasPresent {
		oldRouting := store.routingPath(journal.OldConnection.HostID)
		newRouting := store.routingPath(journal.NewConnection.HostID)
		_, oldErr := os.Lstat(oldRouting)
		_, newErr := os.Lstat(newRouting)
		switch {
		case oldErr == nil && errors.Is(newErr, os.ErrNotExist):
			if err := os.MkdirAll(filepath.Dir(newRouting), 0o700); err != nil {
				return err
			}
			if err := store.hitIO(ownerIORoutingRename); err != nil {
				return err
			}
			if err := os.Rename(oldRouting, newRouting); err != nil {
				return err
			}
			if err := store.syncDirectory(filepath.Dir(oldRouting)); err != nil {
				return err
			}
			if err := store.syncDirectory(filepath.Dir(newRouting)); err != nil {
				return err
			}
		case errors.Is(oldErr, os.ErrNotExist) && newErr == nil:
			// A previous apply crossed the routing rename commit point.
		default:
			return errors.New("owner HostID routing migration state is inconsistent")
		}
	}
	oldConnection := filepath.Join(store.Root, "connections", journal.OldConnection.HostID+".json")
	if err := os.Remove(oldConnection); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	oldCache := store.cache().path(journal.OldConnection.HostID)
	if err := os.Remove(oldCache); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, directory := range []string{filepath.Dir(oldConnection), filepath.Dir(oldCache)} {
		if err := store.syncDirectory(directory); err != nil {
			return err
		}
	}
	path := store.hostIDAdoptionPath()
	if err := store.hitIO(ownerIOJournalCleanup); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return store.syncDirectory(filepath.Dir(path))
}

func (store Connections) hostIDAdoptionPath() string {
	return filepath.Join(store.Root, "host-id-adoption.json")
}

func (store Connections) routingPath(hostID string) string {
	return filepath.Join(store.Root, "routing", hostID)
}

func (store Connections) writeHostIDAdoptionJournal(journal hostIDAdoptionJournal) error {
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return store.writeJournal(store.hostIDAdoptionPath(), append(payload, '\n'))
}

func (store Connections) readHostIDAdoptionJournal() (hostIDAdoptionJournal, bool, error) {
	payload, err := os.ReadFile(store.hostIDAdoptionPath())
	if errors.Is(err, os.ErrNotExist) {
		return hostIDAdoptionJournal{}, false, nil
	}
	if err != nil {
		return hostIDAdoptionJournal{}, false, err
	}
	if len(payload) > 8*1024*1024 {
		return hostIDAdoptionJournal{}, false, errors.New("owner HostID adoption journal is too large")
	}
	var journal hostIDAdoptionJournal
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return hostIDAdoptionJournal{}, false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return hostIDAdoptionJournal{}, false, errors.New("owner HostID adoption journal has trailing data")
	}
	if journal.SchemaVersion != hostIDAdoptionSchema ||
		journal.OldConnection.HostID == journal.NewConnection.HostID ||
		journal.OldConnection.Destination != journal.NewConnection.Destination {
		return hostIDAdoptionJournal{}, false, errors.New("owner HostID adoption journal is invalid")
	}
	if err := journal.OldConnection.Validate(); err != nil {
		return hostIDAdoptionJournal{}, false, err
	}
	if err := journal.NewConnection.Validate(); err != nil {
		return hostIDAdoptionJournal{}, false, err
	}
	if journal.Snapshot == nil || journal.Snapshot.Inventory.HostID != journal.NewConnection.HostID ||
		journal.Snapshot.FetchedAt.IsZero() {
		return hostIDAdoptionJournal{}, false, errors.New("owner HostID adoption snapshot is invalid")
	}
	if err := journal.Snapshot.Inventory.Validate(); err != nil {
		return hostIDAdoptionJournal{}, false, err
	}
	return journal, true, nil
}

func writeOwnerFile(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".owner-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncOwnerDirectory(filepath.Dir(path))
}

func (store Connections) writeJournal(path string, payload []byte) error {
	if err := store.hitIO(ownerIOJournalWrite); err != nil {
		return err
	}
	if err := writeOwnerFile(path, payload); err != nil {
		return err
	}
	return store.hitIO(ownerIODirectorySync)
}

func (store Connections) syncDirectory(path string) error {
	if err := store.hitIO(ownerIODirectorySync); err != nil {
		return err
	}
	return syncOwnerDirectory(path)
}

func syncOwnerDirectory(path string) error {
	directory, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
