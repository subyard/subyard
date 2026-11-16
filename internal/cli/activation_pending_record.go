package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// activationPendingRecord is the protected evidence that one yard-scoped owner
// failed with a typed, declared failure and now waits for that yard's init. It
// is bound to the yard instance, the native state the failure left and the
// owner's handler inputs. Any change of these facts, including a successful
// init, makes the record stale, so it never hides a later or different failure.
type activationPendingRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	Yard          string `json:"yard"`
	Project       string `json:"project"`
	Instance      string `json:"instance"`
	Owner         string `json:"owner"`
	Actual        string `json:"actual"`
	Desired       string `json:"desired"`
	Inputs        string `json:"inputs"`
	// Message is the owner's public diagnostic; it never contains native output.
	Message string `json:"message"`
}

const (
	activationPendingSchema = 1
	activationPendingBytes  = 4096
	// activationPendingMessageBytes bounds the public diagnostic of a record.
	activationPendingMessageBytes = 480
)

func (record activationPendingRecord) validate() error {
	if record.SchemaVersion != activationPendingSchema || !domain.SafeName(record.Yard) || !domain.SafeID(record.Owner) ||
		record.Project == "" || record.Instance == "" || record.Inputs == "" || record.Actual == "" || record.Desired == "" ||
		record.Actual == record.Desired || len(record.Message) > activationPendingMessageBytes || strings.ContainsFunc(record.Message, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("invalid pending activation record")
	}
	return nil
}

// matches reports whether the record still describes exactly these facts.
func (record activationPendingRecord) matches(current activationPendingRecord) bool {
	return record.SchemaVersion == current.SchemaVersion && record.Yard == current.Yard && record.Project == current.Project &&
		record.Instance == current.Instance && record.Owner == current.Owner && record.Actual == current.Actual &&
		record.Desired == current.Desired && record.Inputs == current.Inputs
}

func activationPendingRecordPath(configHome, yard, owner string) (string, error) {
	if !filepath.IsAbs(configHome) || !domain.SafeName(yard) || !domain.SafeID(owner) || strings.HasPrefix(owner, ".") {
		return "", errors.New("invalid pending activation record location")
	}
	return filepath.Join(filepath.Clean(configHome), "activation-pending", "v1", yard, owner+".json"), nil
}

func decodeActivationPendingRecord(payload []byte) (activationPendingRecord, error) {
	var record activationPendingRecord
	if len(payload) > activationPendingBytes {
		return record, errors.New("pending activation record exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return activationPendingRecord{}, fmt.Errorf("decode pending activation record: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return activationPendingRecord{}, errors.New("pending activation record has trailing data")
	}
	return record, record.validate()
}

// readActivationPendingRecord returns the record of one yard and owner. A
// missing record is not an error; an unreadable or invalid one is.
func readActivationPendingRecord(configHome, yard, owner string) (activationPendingRecord, config.PersistentFileSnapshot, error) {
	path, err := activationPendingRecordPath(configHome, yard, owner)
	if err != nil {
		return activationPendingRecord{}, config.PersistentFileSnapshot{}, err
	}
	snapshot, err := config.ReadPersistentFileSnapshot(configHome, path)
	if err != nil || !snapshot.Exists {
		return activationPendingRecord{}, snapshot, err
	}
	record, err := decodeActivationPendingRecord(snapshot.Content)
	if err == nil && (record.Yard != yard || record.Owner != owner) {
		err = errors.New("it is stored under another owner")
	}
	if err != nil {
		// Fail closed, naming the record to inspect or remove.
		relative, relErr := filepath.Rel(filepath.Clean(configHome), path)
		if relErr != nil {
			relative = path
		}
		err = fmt.Errorf("pending activation record %s is invalid: %w", filepath.ToSlash(relative), err)
	}
	return record, snapshot, err
}

// pendingActivation reports whether a matching record proves that the current
// facts are the declared failure that waits for init.
func pendingActivation(configHome string, current activationPendingRecord) (activationPendingRecord, bool, error) {
	record, snapshot, err := readActivationPendingRecord(configHome, current.Yard, current.Owner)
	if err != nil || !snapshot.Exists {
		return activationPendingRecord{}, false, err
	}
	current.SchemaVersion = activationPendingSchema
	return record, record.matches(current), nil
}

func writeActivationPendingRecord(configHome string, record activationPendingRecord) error {
	record.SchemaVersion = activationPendingSchema
	if err := record.validate(); err != nil {
		return err
	}
	path, err := activationPendingRecordPath(configHome, record.Yard, record.Owner)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return config.WritePersistentFile(configHome, path, append(payload, '\n'))
}

// removeActivationPendingRecord retires a record that no longer matches. The
// exact observed snapshot guards against removing a concurrently written one.
func removeActivationPendingRecord(configHome, yard, owner string) error {
	path, err := activationPendingRecordPath(configHome, yard, owner)
	if err != nil {
		return err
	}
	snapshot, err := config.ReadPersistentFileSnapshot(configHome, path)
	if err != nil || !snapshot.Exists {
		return err
	}
	return config.RemovePersistentFileSnapshot(configHome, path, snapshot)
}

// yardActivationPendingRecords lists the known owners with a pending activation
// record for the yard; it never inspects other entries of the record directory.
func yardActivationPendingRecords(configHome, yard string, owners []string) ([]string, error) {
	var present []string
	var refusals []error
	for _, owner := range owners {
		path, err := activationPendingRecordPath(configHome, yard, owner)
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			present = append(present, owner)
		} else if !os.IsNotExist(err) {
			relative, relErr := filepath.Rel(filepath.Clean(configHome), path)
			if relErr != nil {
				relative = path
			}
			refusals = append(refusals, fmt.Errorf("pending activation record %s cannot be read: %w", filepath.ToSlash(relative), err))
		}
	}
	// Every owner's refusal is reported, not only the first.
	if err := domain.CollectRefusals(refusals...); err != nil {
		return nil, err
	}
	return present, nil
}

// removeYardActivationPendingRecords retires the records of the known yard-scoped
// owners of one yard after that yard converged or was torn down. It never
// enumerates the directory: other entries, including symbolic links, stay
// untouched, and an unsafe entry at a record path refuses.
func removeYardActivationPendingRecords(configHome, yard string, owners []string) error {
	var failures []error
	for _, owner := range owners {
		failures = append(failures, removeActivationPendingRecord(configHome, yard, owner))
	}
	return domain.CollectRefusals(failures...)
}
