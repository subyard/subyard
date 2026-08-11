package configsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
)

const hostIDRenameSchema = 1

type HostIDRenamePlan struct {
	ConfigHome      string
	OldHostID       string
	NewHostID       string
	ManifestChanged bool

	hostIDDigest       string
	manifestDigest     string
	manifestBefore     []byte
	manifestAfter      []byte
	manifestWasPresent bool
}

type hostIDRenameJournal struct {
	SchemaVersion      int    `json:"schemaVersion"`
	OldHostID          string `json:"oldHostId"`
	NewHostID          string `json:"newHostId"`
	HostIDDigest       string `json:"hostIdDigest"`
	ManifestDigest     string `json:"manifestDigest,omitempty"`
	ManifestWasPresent bool   `json:"manifestWasPresent"`
	ManifestBefore     []byte `json:"manifestBefore,omitempty"`
	ManifestAfter      []byte `json:"manifestAfter,omitempty"`
}

func HostIDRenameTransactionPath(configHome string) string {
	return filepath.Join(configHome, ".sync", "host-id-rename.json")
}

func PrepareHostIDRename(configHome, newHostID string) (HostIDRenamePlan, error) {
	if !safeHostID(newHostID) {
		return HostIDRenamePlan{}, fmt.Errorf("invalid new owner HostID %q", newHostID)
	}
	if _, err := os.Lstat(TransactionPath(configHome)); err == nil {
		return HostIDRenamePlan{}, ErrRecoveryPending
	} else if !errors.Is(err, os.ErrNotExist) {
		return HostIDRenamePlan{}, err
	}
	if _, err := os.Lstat(HostIDRenameTransactionPath(configHome)); err == nil {
		return HostIDRenamePlan{}, ErrRecoveryPending
	} else if !errors.Is(err, os.ErrNotExist) {
		return HostIDRenamePlan{}, err
	}
	oldHostID, pending, err := ResolveHostID(configHome, nil)
	if err != nil {
		return HostIDRenamePlan{}, err
	}
	if pending {
		return HostIDRenamePlan{}, errors.New("owner HostID must be initialized before it can be renamed")
	}
	if oldHostID == newHostID {
		return HostIDRenamePlan{}, errors.New("new owner HostID matches the current HostID")
	}
	identity, err := os.ReadFile(HostIDPath(configHome))
	if err != nil {
		return HostIDRenamePlan{}, err
	}
	plan := HostIDRenamePlan{
		ConfigHome: configHome, OldHostID: oldHostID, NewHostID: newHostID,
		hostIDDigest: digestBytes(identity),
	}
	manifestPath := ManifestPath(configHome)
	manifestBytes, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return plan, nil
	}
	if err != nil {
		return HostIDRenamePlan{}, err
	}
	manifest, err := readManifest(configHome)
	if err != nil {
		return HostIDRenamePlan{}, err
	}
	if manifest.HostID != oldHostID {
		return HostIDRenamePlan{}, fmt.Errorf(
			"config sync manifest belongs to owner HostID %q, not %q", manifest.HostID, oldHostID,
		)
	}
	manifest.HostID = newHostID
	updated, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return HostIDRenamePlan{}, err
	}
	plan.ManifestChanged = true
	plan.manifestWasPresent = true
	plan.manifestDigest = digestBytes(manifestBytes)
	plan.manifestBefore = manifestBytes
	plan.manifestAfter = append(updated, '\n')
	return plan, nil
}

func ApplyHostIDRename(plan HostIDRenamePlan) (returnErr error) {
	if plan.ConfigHome == "" || !safeHostID(plan.OldHostID) || !safeHostID(plan.NewHostID) {
		return errors.New("invalid owner HostID rename plan")
	}
	if err := ensureConfigurationRoot(plan.ConfigHome); err != nil {
		return err
	}
	unlock, err := config.LockRoot(plan.ConfigHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverHostIDRenameLocked(plan.ConfigHome); err != nil {
		return fmt.Errorf("recover previous owner HostID rename: %w", err)
	}
	current, err := PrepareHostIDRename(plan.ConfigHome, plan.NewHostID)
	if err != nil {
		if strings.Contains(err.Error(), "matches the current HostID") {
			return ErrPlanStale
		}
		return err
	}
	if current.OldHostID != plan.OldHostID || current.hostIDDigest != plan.hostIDDigest ||
		current.manifestDigest != plan.manifestDigest || current.ManifestChanged != plan.ManifestChanged {
		return ErrPlanStale
	}
	journal := hostIDRenameJournal{
		SchemaVersion: hostIDRenameSchema, OldHostID: plan.OldHostID, NewHostID: plan.NewHostID,
		HostIDDigest: plan.hostIDDigest, ManifestDigest: plan.manifestDigest,
		ManifestWasPresent: plan.manifestWasPresent,
		ManifestBefore:     plan.manifestBefore, ManifestAfter: plan.manifestAfter,
	}
	if err := writeHostIDRenameJournal(plan.ConfigHome, journal); err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			if recoveryErr := recoverHostIDRenameLocked(plan.ConfigHome); recoveryErr != nil {
				returnErr = fmt.Errorf("%w; rollback failed: %v", returnErr, recoveryErr)
			}
		}
	}()
	if plan.manifestWasPresent {
		if err := writeFileDurable(plan.ConfigHome, ManifestPath(plan.ConfigHome), plan.manifestAfter, 0o600); err != nil {
			return err
		}
	}
	if err := writeFileDurable(
		plan.ConfigHome, HostIDPath(plan.ConfigHome), []byte(plan.NewHostID+"\n"), 0o600,
	); err != nil {
		return err
	}
	return cleanupHostIDRename(plan.ConfigHome)
}

func RecoverHostIDRename(configHome string) error {
	if _, err := os.Lstat(HostIDRenameTransactionPath(configHome)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	unlock, err := config.LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	return recoverHostIDRenameLocked(configHome)
}

func recoverHostIDRenameLocked(configHome string) error {
	journal, exists, err := readHostIDRenameJournal(configHome)
	if err != nil || !exists {
		return err
	}
	content, err := os.ReadFile(HostIDPath(configHome))
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(content))
	switch current {
	case journal.OldHostID:
		if digestBytes(content) != journal.HostIDDigest {
			return errors.New("owner HostID changed during rename recovery")
		}
		if journal.ManifestWasPresent {
			if err := writeFileDurable(configHome, ManifestPath(configHome), journal.ManifestBefore, 0o600); err != nil {
				return err
			}
		}
	case journal.NewHostID:
		if journal.ManifestWasPresent {
			if err := writeFileDurable(configHome, ManifestPath(configHome), journal.ManifestAfter, 0o600); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("owner HostID changed to %q during rename recovery", current)
	}
	return cleanupHostIDRename(configHome)
}

func writeHostIDRenameJournal(configHome string, journal hostIDRenameJournal) error {
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurable(
		configHome, HostIDRenameTransactionPath(configHome), append(payload, '\n'), 0o600,
	)
}

func readHostIDRenameJournal(configHome string) (hostIDRenameJournal, bool, error) {
	path := HostIDRenameTransactionPath(configHome)
	if err := validateConfigurationAncestors(configHome, path); err != nil {
		return hostIDRenameJournal{}, false, err
	}
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hostIDRenameJournal{}, false, nil
	}
	if err != nil {
		return hostIDRenameJournal{}, false, err
	}
	if len(payload) > 2*1024*1024 {
		return hostIDRenameJournal{}, false, errors.New("owner HostID rename journal is too large")
	}
	var journal hostIDRenameJournal
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return hostIDRenameJournal{}, false, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return hostIDRenameJournal{}, false, err
	}
	if journal.SchemaVersion != hostIDRenameSchema || !safeHostID(journal.OldHostID) ||
		!safeHostID(journal.NewHostID) || journal.OldHostID == journal.NewHostID ||
		!validHexDigest(journal.HostIDDigest, 64) {
		return hostIDRenameJournal{}, false, errors.New("owner HostID rename journal is invalid")
	}
	if journal.ManifestWasPresent {
		if !validHexDigest(journal.ManifestDigest, 64) || len(journal.ManifestBefore) == 0 ||
			len(journal.ManifestAfter) == 0 || digestBytes(journal.ManifestBefore) != journal.ManifestDigest {
			return hostIDRenameJournal{}, false, errors.New("owner HostID rename manifest journal is invalid")
		}
	}
	return journal, true, nil
}

func cleanupHostIDRename(configHome string) error {
	path := HostIDRenameTransactionPath(configHome)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
