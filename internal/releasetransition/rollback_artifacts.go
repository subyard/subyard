package releasetransition

import (
	"bytes"
	"errors"
	"slices"
	"strings"
)

// A closed artifact inventory proves bytes, not migration semantics or history.
// S must establish closure with its typed handlers before publishing it. R can
// subsequently compare this finite inventory without loading those handlers.
type rollbackArtifact struct {
	Path        string      `json:"path"`
	Fingerprint Fingerprint `json:"fingerprint"`
}

const (
	maxRollbackArtifactEntries = 8192
	maxRollbackArtifactBytes   = 32 << 20
)

func (store *POSIXV2Store) rollbackArtifactInventory(transaction TransactionID) ([]rollbackArtifact, error) {
	if err := validateTransactionID(transaction); err != nil {
		return nil, err
	}
	result := []rollbackArtifact{}
	remainingBytes := int64(maxRollbackArtifactBytes)
	var visit func([]string, string, int) error
	visit = func(parts []string, relative string, depth int) error {
		entries, present, err := store.readDirectoryEntries(parts, maxRollbackArtifactEntries-len(result))
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		if len(entries)+len(result) >= maxRollbackArtifactEntries {
			return errors.New("rollback artifact inventory exceeds its entry bound")
		}
		// Directory existence joins the inventory, including empty directories.
		result = append(result, rollbackArtifact{Path: relative + "/", Fingerprint: fingerprintPayload(nil)})
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".pending") {
				return errors.New("rollback artifacts contain an unfinished publication")
			}
			if entry.IsDir() {
				if relative != "transaction" || depth != 0 || name != "evidence" && name != "recovery" {
					return errors.New("rollback artifacts contain an unsupported directory")
				}
				if err := visit(append(slices.Clone(parts), name), relative+"/"+name, depth+1); err != nil {
					return err
				}
				continue
			}
			if err := validateSafeID(name, "rollback artifact name"); err != nil {
				return err
			}
			if !strings.HasSuffix(name, ".json") || relative == "transaction" && name != "superseded-journal.json" {
				return errors.New("rollback artifacts contain an unsupported record")
			}
			snapshot, err := store.readRecord(parts, name)
			if err != nil {
				return err
			}
			if !snapshot.Exists || len(snapshot.Payload) == 0 || len(snapshot.Payload) > MaxProtectedRecordBytes {
				return errors.New("rollback artifact is missing or exceeds its byte bound")
			}
			remainingBytes -= int64(len(snapshot.Payload))
			if remainingBytes < 0 {
				return errors.New("rollback artifact inventory exceeds its byte bound")
			}
			if relative == "transaction/evidence" {
				evidence, err := ParseEvidence(snapshot.Payload)
				canonical, marshalErr := MarshalEvidence(evidence)
				expected, nameErr := checkpointEvidenceName(evidence.Transaction, evidence.Step, evidence.Checkpoint)
				if err != nil || marshalErr != nil || nameErr != nil || evidence.Transaction != transaction || expected != name || !bytes.Equal(canonical, snapshot.Payload) {
					return errors.New("rollback artifact evidence is not canonical or bound to its transaction")
				}
			}
			result = append(result, rollbackArtifact{Path: relative + "/" + name, Fingerprint: snapshot.Fingerprint})
		}
		return nil
	}
	if err := visit([]string{"release-transition", "v2", "transactions", string(transaction)}, "transaction", 0); err != nil {
		return nil, err
	}
	if err := visit(nativeIdentityParts(transaction), "native-identities", 0); err != nil {
		return nil, err
	}
	if len(result) > maxRollbackArtifactEntries {
		return nil, errors.New("rollback artifact inventory exceeds its entry bound")
	}
	slices.SortFunc(result, func(left, right rollbackArtifact) int { return strings.Compare(left.Path, right.Path) })
	return result, nil
}

func (store *POSIXV2Store) checkRollbackArtifactInventory(transaction TransactionID, expected []rollbackArtifact) error {
	actual, err := store.rollbackArtifactInventory(transaction)
	if err != nil {
		return err
	}
	if !slices.Equal(actual, expected) {
		return errors.New("closed rollback artifact inventory changed")
	}
	return nil
}
