package releasetransition

import (
	"encoding/json"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

var rollbackAbortArchiveParts = []string{"reconcile", "rollback", "v1", "aborted"}

type rollbackAbortArchive struct {
	SchemaVersion int                `json:"schemaVersion"`
	Authorization PlanToken          `json:"authorization"`
	Journal       []byte             `json:"journal"`
	Checkpoint    Fingerprint        `json:"checkpoint"`
	Projection    Fingerprint        `json:"projection"`
	Artifacts     []rollbackArtifact `json:"artifacts"`
}

// preserveRollbackAbortArchive writes exact copies before publishing the
// receipt. Its caller holds the transition lock and owns the abort grant.
// The selected journal and all live records remain in place.
func (store *POSIXV2Store) preserveRollbackAbortArchive(cut rollbackAbortCut, authorization PlanToken) error {
	if cut.store != store || store == nil {
		return invalid("rollback abort archive requires a cut proved by this store")
	}
	if err := validatePlanToken(authorization); err != nil {
		return err
	}
	fresh, err := store.prepareRollbackAbortCut(cut.journal)
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(fresh.history.AuthoritySnapshot, cut.history.AuthoritySnapshot) ||
		!sameProtectedSnapshot(fresh.history.ProjectionSnapshot, cut.history.ProjectionSnapshot) ||
		!sameProtectedSnapshot(fresh.origin, cut.origin) {
		return ErrProtectedStoreStale
	}
	journal, err := ParseJournal(cut.journal.Payload)
	if err != nil {
		return err
	}
	artifacts, err := store.rollbackArtifactInventory(journal.Transaction)
	if err != nil {
		return err
	}
	archive := rollbackAbortArchive{
		SchemaVersion: 1, Authorization: authorization, Journal: cut.journal.Payload,
		Checkpoint: cut.history.AuthoritySnapshot.Fingerprint, Projection: cut.history.ProjectionSnapshot.Fingerprint,
		Artifacts: artifacts,
	}
	payload, err := json.Marshal(archive)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := validateProtectedPayload(payload); err != nil {
		return err
	}
	destination := append(slices.Clone(rollbackAbortArchiveParts), string(journal.Transaction))
	existing, err := store.readRecord(destination, "archive.json")
	if err != nil {
		return err
	}
	if existing.Exists && !sameProtectedSnapshot(existing, protectedSnapshotFromPayload(payload)) {
		return ErrProtectedStoreStale
	}
	for _, artifact := range artifacts {
		parts, name, directory, err := rollbackArtifactLocation(journal.Transaction, artifact.Path)
		if err != nil {
			return err
		}
		path := strings.Split(strings.TrimSuffix(artifact.Path, "/"), "/")
		if directory {
			fd, _, err := store.openParent(append(slices.Clone(destination), path...), true)
			if err != nil {
				return err
			}
			_ = unix.Close(fd)
			continue
		}
		snapshot, err := store.readRecord(parts, name)
		if err != nil {
			return err
		}
		if !snapshot.Exists || snapshot.Fingerprint != artifact.Fingerprint {
			return ErrProtectedStoreStale
		}
		if err := store.createImmutable(append(slices.Clone(destination), path[:len(path)-1]...), name, snapshot.Payload); err != nil {
			return err
		}
		if err := store.inject("after-rollback-abort-artifact-copy"); err != nil {
			return err
		}
	}
	if err := store.checkRollbackArtifactInventory(journal.Transaction, artifacts); err != nil {
		return err
	}
	return store.createImmutable(destination, "archive.json", payload)
}
