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
)

func (record activationPendingRecord) validate() error {
	if record.SchemaVersion != activationPendingSchema || !domain.SafeName(record.Yard) || !domain.SafeID(record.Owner) ||
		record.Project == "" || record.Instance == "" || record.Inputs == "" || record.Actual == "" || record.Desired == "" ||
		record.Actual == record.Desired || len(record.Message) > 480 || strings.ContainsFunc(record.Message, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
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
		err = errors.New("pending activation record is stored under another owner")
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

// removeYardActivationPendingRecords retires every record of one yard after
// that yard converged all its runtimes or was torn down.
func removeYardActivationPendingRecords(configHome, yard string) error {
	if !filepath.IsAbs(configHome) || !domain.SafeName(yard) {
		return errors.New("invalid pending activation record location")
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Clean(configHome), "activation-pending", "v1", yard))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		owner, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !entry.Type().IsRegular() {
			failures = append(failures, fmt.Errorf("unexpected pending activation entry %q", entry.Name()))
			continue
		}
		failures = append(failures, removeActivationPendingRecord(configHome, yard, owner))
	}
	return domain.CollectRefusals(failures...)
}
