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
	AlreadyMigrated bool

	hostIDDigest       string
	manifestDigest     string
	manifestBefore     []byte
	manifestAfter      []byte
	manifestWasPresent bool
	source             *hostSourceRename
	options            Options
	sourceHostID       string
}

// StateDigest binds the owner identity and machine-local manifest baseline.
func (plan HostIDRenamePlan) StateDigest() string {
	payload, _ := json.Marshal(struct {
		OldHostID, NewHostID, HostID, Manifest string
		ManifestChanged                        bool
	}{plan.OldHostID, plan.NewHostID, plan.hostIDDigest, plan.manifestDigest, plan.ManifestChanged})
	if plan.source != nil {
		payload, _ = json.Marshal(struct {
			Baseline []byte
			Source   *hostSourceRename
		}{payload, plan.source})
	}
	return digestBytes(payload)
}

type hostIDRenameJournal struct {
	SchemaVersion      int               `json:"schemaVersion"`
	OldHostID          string            `json:"oldHostId"`
	NewHostID          string            `json:"newHostId"`
	HostIDDigest       string            `json:"hostIdDigest"`
	ManifestDigest     string            `json:"manifestDigest,omitempty"`
	ManifestWasPresent bool              `json:"manifestWasPresent"`
	ManifestBefore     []byte            `json:"manifestBefore,omitempty"`
	ManifestAfter      []byte            `json:"manifestAfter,omitempty"`
	Source             *hostSourceRename `json:"source,omitempty"`
	payloadDigest      string
}

func HostIDRenameTransactionPath(configHome string) string {
	return filepath.Join(configHome, ".sync", "host-id-rename.json")
}

func PrepareHostIDRename(configHome, newHostID string) (HostIDRenamePlan, error) {
	if _, exists, err := readSourceRecord(configHome); err != nil {
		return HostIDRenamePlan{}, err
	} else if exists {
		return HostIDRenamePlan{}, errors.New("registered source rename requires validated source migration")
	}
	return prepareHostIDRename(configHome, newHostID, false)
}

func prepareHostIDRename(configHome, newHostID string, recovery bool) (HostIDRenamePlan, error) {
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
	if oldHostID == newHostID && !recovery {
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
	if manifest.HostID != oldHostID && !recovery {
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
	if plan.AlreadyMigrated {
		current, err := PrepareHostIDRenameWithSource(plan.options, plan.NewHostID, plan.sourceHostID)
		if err != nil {
			return err
		}
		if !current.AlreadyMigrated {
			return ErrPlanStale
		}
		return nil
	}
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
	if _, pending, err := AssessHostIDRenameRecovery(plan.ConfigHome); err != nil {
		return err
	} else if pending {
		return ErrRecoveryPending
	}
	identity, err := os.ReadFile(HostIDPath(plan.ConfigHome))
	if err != nil {
		return err
	}
	if digestBytes(identity) != plan.hostIDDigest {
		return ErrPlanStale
	}
	var current HostIDRenamePlan
	if plan.options.ConfigHome != "" {
		lockedOptions := plan.options
		lockedOptions.ConfigLocked = true
		current, err = PrepareHostIDRenameWithSource(lockedOptions, plan.NewHostID, plan.sourceHostID)
	} else {
		current, err = PrepareHostIDRename(plan.ConfigHome, plan.NewHostID)
	}
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
	if plan.source != nil && !sameSourceRenameBaseline(plan.source, current.source) {
		return ErrPlanStale
	}
	if plan.source != nil {
		if err := finalizeSourceRename(&plan); err != nil {
			return err
		}
	}
	journal := hostIDRenameJournal{
		SchemaVersion: hostIDRenameSchema, OldHostID: plan.OldHostID, NewHostID: plan.NewHostID,
		HostIDDigest: plan.hostIDDigest, ManifestDigest: plan.manifestDigest,
		ManifestWasPresent: plan.manifestWasPresent,
		ManifestBefore:     plan.manifestBefore, ManifestAfter: plan.manifestAfter, Source: plan.source,
	}
	if plan.source != nil {
		journal.SchemaVersion = 2
	}
	if err := writeHostIDRenameJournal(plan.ConfigHome, journal); err != nil {
		return err
	}
	encodedJournal, _ := json.MarshalIndent(journal, "", "  ")
	expectedJournalDigest := digestBytes(append(encodedJournal, '\n'))
	defer func() {
		if returnErr != nil {
			if recoveryErr := recoverHostIDRenameLocked(plan.ConfigHome, expectedJournalDigest); recoveryErr != nil {
				returnErr = fmt.Errorf("%w; rollback failed: %v", returnErr, recoveryErr)
			}
		}
	}()
	if plan.source != nil {
		return recoverHostIDRenameLocked(plan.ConfigHome, expectedJournalDigest)
	}
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

// HostIDRenameRecoveryPlan binds durable consent to exact journal bytes and type.
// Callers fence this Target and, for Source plans, verify release authority.
type HostIDRenameRecoveryPlan struct {
	ConfigHome    string
	Target        string
	Source        bool
	SchemaVersion int
	payloadDigest string
}

func AssessHostIDRenameRecovery(configHome string) (HostIDRenameRecoveryPlan, bool, error) {
	journal, pending, err := readHostIDRenameJournal(configHome)
	if err != nil || !pending {
		return HostIDRenameRecoveryPlan{}, pending, err
	}
	return HostIDRenameRecoveryPlan{ConfigHome: configHome, Target: journal.NewHostID, Source: journal.Source != nil, SchemaVersion: journal.SchemaVersion, payloadDigest: journal.payloadDigest}, true, nil
}
func RecoverHostIDRename(configHome string) error {
	plan, pending, err := AssessHostIDRenameRecovery(configHome)
	if err != nil || !pending {
		return err
	}
	return RecoverHostIDRenameExact(plan)
}
func RecoverHostIDRenameExact(plan HostIDRenameRecoveryPlan) error {
	if !validHexDigest(plan.payloadDigest, 64) {
		return ErrPlanStale
	}
	unlock, err := config.LockRoot(plan.ConfigHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	journal, pending, err := readHostIDRenameJournal(plan.ConfigHome)
	if err != nil {
		return fmt.Errorf("%w: recovery journal changed: %v", ErrPlanStale, err)
	}
	if !pending || journal.payloadDigest != plan.payloadDigest || journal.NewHostID != plan.Target || journal.SchemaVersion != plan.SchemaVersion || (journal.Source != nil) != plan.Source {
		return ErrPlanStale
	}
	return recoverHostIDRenameJournalLocked(plan.ConfigHome, journal)
}

func recoverHostIDRenameLocked(configHome, expectedDigest string) error {
	journal, exists, err := readHostIDRenameJournal(configHome)
	if err != nil || !exists {
		return err
	}
	if journal.payloadDigest != expectedDigest {
		return ErrPlanStale
	}
	return recoverHostIDRenameJournalLocked(configHome, journal)
}

func recoverHostIDRenameJournalLocked(configHome string, journal hostIDRenameJournal) error {
	if journal.Source != nil {
		return recoverHostSourceRename(configHome, journal)
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
	if len(payload) > 64*1024*1024 {
		return errors.New("owner HostID rename journal is too large")
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
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return hostIDRenameJournal{}, false, nil
	}
	if err != nil {
		return hostIDRenameJournal{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return hostIDRenameJournal{}, false, errors.New("unsafe owner rename journal")
	}
	if err := validateOwnedMode(path, info, false); err != nil {
		return hostIDRenameJournal{}, false, err
	}
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hostIDRenameJournal{}, false, nil
	}
	if err != nil {
		return hostIDRenameJournal{}, false, err
	}
	if len(payload) > 64*1024*1024 {
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
	if (journal.SchemaVersion != hostIDRenameSchema && journal.SchemaVersion != 2) || (journal.SchemaVersion == 2) != (journal.Source != nil) || !safeHostID(journal.OldHostID) ||
		!safeHostID(journal.NewHostID) || journal.OldHostID == journal.NewHostID && journal.Source == nil ||
		!validHexDigest(journal.HostIDDigest, 64) {
		return hostIDRenameJournal{}, false, errors.New("owner HostID rename journal is invalid")
	}
	if journal.ManifestWasPresent {
		if !validHexDigest(journal.ManifestDigest, 64) || len(journal.ManifestBefore) == 0 ||
			len(journal.ManifestAfter) == 0 || digestBytes(journal.ManifestBefore) != journal.ManifestDigest {
			return hostIDRenameJournal{}, false, errors.New("owner HostID rename manifest journal is invalid")
		}
	}
	journal.payloadDigest = digestBytes(payload)
	return journal, true, nil
}

func cleanupHostIDRename(configHome string) error {
	path := HostIDRenameTransactionPath(configHome)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
