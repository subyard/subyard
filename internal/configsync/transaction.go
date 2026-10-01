package configsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
)

type transaction struct {
	SchemaVersion     int                `json:"schemaVersion"`
	ID                string             `json:"id"`
	Phase             string             `json:"phase"`
	PlanDigest        string             `json:"planDigest"`
	NewManifestDigest string             `json:"newManifestDigest"`
	HostID            string             `json:"hostId,omitempty"`
	InitializeHostID  bool               `json:"initializeHostId,omitempty"`
	Applied           int                `json:"applied"`
	Entries           []transactionEntry `json:"entries"`
}

type transactionEntry struct {
	Path         string `json:"path"`
	Action       string `json:"action"`
	Existed      bool   `json:"existed"`
	BeforeDigest string `json:"beforeDigest,omitempty"`
	AfterDigest  string `json:"afterDigest,omitempty"`
	BeforeMode   uint32 `json:"beforeMode,omitempty"`
	AfterMode    uint32 `json:"afterMode,omitempty"`
}

func Apply(plan Plan) error {
	if !plan.NeedsApply() {
		return nil
	}
	if err := ensureConfigurationRoot(plan.options.ConfigHome); err != nil {
		return err
	}
	unlock, err := config.LockRoot(plan.options.ConfigHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverLocked(plan.options.ConfigHome); err != nil {
		return fmt.Errorf("recover previous config sync: %w", err)
	}
	recheckOptions := plan.options
	recheckOptions.ConfigLocked = true
	current, err := BuildPlan(recheckOptions)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPlanStale, err)
	}
	if current.Digest != plan.Digest {
		return ErrPlanStale
	}
	return applyLocked(current)
}

func Recover(configHome string) error {
	if _, err := os.Lstat(TransactionPath(configHome)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	unlock, err := config.LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	return recoverLocked(configHome)
}

func applyLocked(plan Plan) (returnErr error) {
	syncRoot := filepath.Join(plan.options.ConfigHome, ".sync")
	if err := ensureDirectory(plan.options.ConfigHome, syncRoot, 0o700); err != nil {
		return err
	}
	transactionID := fmt.Sprintf("%d-%s", plan.Generation, plan.Digest[:16])
	transactionRoot := filepath.Join(syncRoot, "transactions", transactionID)
	if err := ensureDirectory(plan.options.ConfigHome, transactionRoot, 0o700); err != nil {
		return err
	}
	journalWritten := false
	defer func() {
		if !journalWritten {
			_ = removeTransactionRoot(plan.options.ConfigHome, transactionRoot)
		}
	}()
	tx := transaction{
		SchemaVersion: 1, ID: transactionID, Phase: "prepared", PlanDigest: plan.Digest,
		HostID: plan.HostID, InitializeHostID: plan.InitializeHostID,
	}
	for _, change := range plan.Changes {
		if change.Action == "adopt" || change.Action == "record-converged" ||
			change.Action == "record-deleted" || change.Action == "retain-local" {
			continue
		}
		target := filepath.Join(plan.options.ConfigHome, filepath.FromSlash(change.Path))
		live, exists, err := inspectLiveFile(plan.options.ConfigHome, target)
		if err != nil {
			return err
		}
		entry := transactionEntry{
			Path: change.Path, Action: change.Action, Existed: exists,
			AfterDigest: change.AfterDigest, AfterMode: change.Mode,
		}
		if exists {
			content, mode, err := readLiveContent(plan.options.ConfigHome, target)
			if err != nil {
				return err
			}
			entry.BeforeDigest = live.Digest
			entry.BeforeMode = mode
			backup := filepath.Join(transactionRoot, "backup", filepath.FromSlash(change.Path))
			if err := writeFileDurable(
				plan.options.ConfigHome, backup, content, os.FileMode(mode),
			); err != nil {
				return err
			}
		}
		if change.Action != "delete" {
			candidate, ok := plan.desired[change.Path]
			if !ok {
				return fmt.Errorf("planned candidate disappeared for %s", change.Path)
			}
			staged := filepath.Join(transactionRoot, "candidate", filepath.FromSlash(change.Path))
			if err := writeFileDurable(
				plan.options.ConfigHome, staged, candidate.Content, os.FileMode(candidate.Mode),
			); err != nil {
				return err
			}
		}
		tx.Entries = append(tx.Entries, entry)
	}
	manifestContent, err := newManifestContent(plan)
	if err != nil {
		return err
	}
	tx.NewManifestDigest = digestBytes(manifestContent)
	if err := writeTransaction(plan.options.ConfigHome, tx); err != nil {
		return err
	}
	journalWritten = true
	defer func() {
		if returnErr == nil {
			return
		}
		if recoveryErr := recoverLocked(plan.options.ConfigHome); recoveryErr != nil {
			returnErr = fmt.Errorf("%w; rollback failed: %v", returnErr, recoveryErr)
		}
	}()
	if plan.InitializeHostID {
		if err := writeFileDurable(
			plan.options.ConfigHome, HostIDPath(plan.options.ConfigHome),
			[]byte(plan.HostID+"\n"), 0o600,
		); err != nil {
			return err
		}
	}
	tx.Phase = "applying"
	if err := writeTransaction(plan.options.ConfigHome, tx); err != nil {
		return err
	}
	for index, entry := range tx.Entries {
		target := filepath.Join(plan.options.ConfigHome, filepath.FromSlash(entry.Path))
		if entry.Action == "delete" {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := syncDirectory(filepath.Dir(target)); err != nil {
				return err
			}
		} else {
			candidate := plan.desired[entry.Path]
			if err := writeFileDurable(
				plan.options.ConfigHome, target, candidate.Content, os.FileMode(candidate.Mode),
			); err != nil {
				return err
			}
		}
		tx.Applied = index + 1
		if err := writeTransaction(plan.options.ConfigHome, tx); err != nil {
			return err
		}
	}
	tx.Phase = "publishing"
	if err := writeTransaction(plan.options.ConfigHome, tx); err != nil {
		return err
	}
	if err := writeFileDurable(
		plan.options.ConfigHome, ManifestPath(plan.options.ConfigHome), manifestContent, 0o600,
	); err != nil {
		return err
	}
	tx.Phase = "committed"
	if err := writeTransaction(plan.options.ConfigHome, tx); err != nil {
		return err
	}
	return cleanupTransaction(plan.options.ConfigHome, tx)
}

func recoverLocked(configHome string) error {
	tx, exists, err := readTransaction(configHome)
	if err != nil || !exists {
		return err
	}
	if tx.Phase == "committed" {
		return cleanupTransaction(configHome, tx)
	}
	if tx.Phase == "publishing" {
		content, err := os.ReadFile(ManifestPath(configHome))
		if err == nil && digestBytes(content) == tx.NewManifestDigest {
			return cleanupTransaction(configHome, tx)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	transactionRoot := filepath.Join(configHome, ".sync", "transactions", tx.ID)
	for index := len(tx.Entries) - 1; index >= 0; index-- {
		entry := tx.Entries[index]
		if !safeRelative(entry.Path) {
			return fmt.Errorf("transaction contains unsafe path %q", entry.Path)
		}
		target := filepath.Join(configHome, filepath.FromSlash(entry.Path))
		applied, err := transactionEntryApplied(configHome, entry, index < tx.Applied)
		if err != nil {
			return err
		}
		if !applied {
			continue
		}
		if entry.Existed {
			backup := filepath.Join(transactionRoot, "backup", filepath.FromSlash(entry.Path))
			content, err := os.ReadFile(backup)
			if err != nil {
				return fmt.Errorf("read recovery backup for %s: %w", entry.Path, err)
			}
			if digestBytes(content) != entry.BeforeDigest {
				return fmt.Errorf("recovery backup digest changed for %s", entry.Path)
			}
			if err := writeFileDurable(
				configHome, target, content, os.FileMode(entry.BeforeMode),
			); err != nil {
				return err
			}
		} else if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		} else if err == nil {
			if err := syncDirectory(filepath.Dir(target)); err != nil {
				return err
			}
		}
	}
	if tx.InitializeHostID {
		if err := rollbackInitializedHostID(configHome, tx.HostID); err != nil {
			return err
		}
	}
	return cleanupTransaction(configHome, tx)
}

func transactionEntryApplied(
	configHome string,
	entry transactionEntry,
	journalApplied bool,
) (bool, error) {
	target := filepath.Join(configHome, filepath.FromSlash(entry.Path))
	live, exists, err := inspectLiveFile(configHome, target)
	if err != nil {
		return false, fmt.Errorf("inspect recovery target %s: %w", entry.Path, err)
	}
	before := entry.Existed && exists &&
		live.Digest == entry.BeforeDigest && live.Mode == entry.BeforeMode
	after := false
	if entry.Action == "delete" {
		after = !exists
	} else {
		after = exists && live.Digest == entry.AfterDigest && live.Mode == entry.AfterMode
	}
	if after {
		return true, nil
	}
	if before || (!entry.Existed && !exists) {
		return false, nil
	}
	progress := "before it was recorded as applied"
	if journalApplied {
		progress = "after it was recorded as applied"
	}
	return false, fmt.Errorf(
		"recovery target %s changed outside the transaction %s; refusing to overwrite it",
		entry.Path, progress,
	)
}

func rollbackInitializedHostID(configHome, expected string) error {
	path := HostIDPath(configHome)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 {
		return errors.New("initialized owner host ID changed during recovery")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(content)) != expected {
		return errors.New("initialized owner host ID changed during recovery")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(configHome)
}

func newManifestContent(plan Plan) ([]byte, error) {
	paths := make([]string, 0, len(plan.desired))
	for path := range plan.desired {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	manifest := Manifest{
		SchemaVersion: manifestSchema, Generation: plan.Generation,
		SourceID: plan.SourceID, SourceCommit: plan.SourceCommit, HostID: plan.HostID,
		SourceSchema: sourceSchema, SourceDigest: plan.SourceDigest,
	}
	for _, path := range paths {
		candidate := plan.desired[path]
		manifest.Files = append(manifest.Files, ManagedFile{
			Path: path, Digest: candidate.Digest, Mode: candidate.Mode,
			Generation: plan.Generation,
		})
	}
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

func readTransaction(configHome string) (transaction, bool, error) {
	path := TransactionPath(configHome)
	if err := validateConfigurationAncestors(configHome, path); err != nil {
		return transaction{}, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return transaction{}, false, nil
	}
	if err != nil {
		return transaction{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 {
		return transaction{}, false, errors.New("config sync transaction must be a regular 0600 file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return transaction{}, false, err
	}
	var tx transaction
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tx); err != nil {
		return transaction{}, false, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return transaction{}, false, err
	}
	if tx.SchemaVersion != 1 || !safeTransactionID(tx.ID) ||
		len(tx.PlanDigest) != 64 || len(tx.NewManifestDigest) != 64 {
		return transaction{}, false, errors.New("config sync transaction is invalid")
	}
	if tx.InitializeHostID && !safeHostID(tx.HostID) {
		return transaction{}, false, errors.New("config sync transaction host ID is invalid")
	}
	switch tx.Phase {
	case "prepared", "applying", "publishing", "committed":
	default:
		return transaction{}, false, fmt.Errorf("unknown config sync transaction phase %q", tx.Phase)
	}
	if tx.Applied < 0 || tx.Applied > len(tx.Entries) {
		return transaction{}, false, errors.New("config sync transaction progress is invalid")
	}
	for _, entry := range tx.Entries {
		if !safeRelative(entry.Path) {
			return transaction{}, false, errors.New("config sync transaction path is invalid")
		}
		if !transactionAction(entry.Action) ||
			(entry.Existed && (!validHexDigest(entry.BeforeDigest, sha256.Size*2) ||
				entry.BeforeMode == 0)) ||
			(entry.Action != "delete" &&
				(!validHexDigest(entry.AfterDigest, sha256.Size*2) || entry.AfterMode == 0)) {
			return transaction{}, false, errors.New("config sync transaction entry is invalid")
		}
	}
	return tx, true, nil
}

func transactionAction(action string) bool {
	switch action {
	case "add", "adopt-update", "update", "restore-missing", "restore-drift", "delete":
		return true
	default:
		return false
	}
}

func writeTransaction(configHome string, tx transaction) error {
	content, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurable(
		configHome, TransactionPath(configHome), append(content, '\n'), 0o600,
	)
}

func cleanupTransaction(configHome string, tx transaction) error {
	transactionRoot := filepath.Join(configHome, ".sync", "transactions", tx.ID)
	if err := removeTransactionRoot(configHome, transactionRoot); err != nil {
		return err
	}
	if err := os.Remove(TransactionPath(configHome)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Join(configHome, ".sync"))
}

func removeTransactionRoot(configHome, transactionRoot string) error {
	expectedRoot := filepath.Join(configHome, ".sync", "transactions")
	if !pathWithin(transactionRoot, expectedRoot) || filepath.Dir(transactionRoot) != expectedRoot {
		return errors.New("refusing to clean unsafe transaction path")
	}
	if err := os.RemoveAll(transactionRoot); err != nil {
		return err
	}
	return syncDirectory(expectedRoot)
}

func ensureConfigurationRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return err
	}
	return validateConfigurationRootInfo(root, info)
}

func validateExistingConfigurationRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return validateConfigurationRootInfo(root, info)
}

func validateConfigurationRootInfo(root string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode().Perm()&0o022 != 0 {
		return errors.New("configuration root must be a private real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("configuration root is not operator-owned")
	}
	return nil
}

func validateConfigurationAncestors(configHome, path string) error {
	if !pathWithin(path, configHome) {
		return errors.New("configuration path escaped its root")
	}
	if err := validateExistingConfigurationRoot(configHome); err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Clean(configHome), filepath.Clean(path))
	if err != nil {
		return err
	}
	current := filepath.Clean(configHome)
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
			info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("unsafe configuration directory %s", current)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("configuration directory is not operator-owned: %s", current)
		}
	}
	return nil
}

func ensureDirectory(configHome, path string, mode os.FileMode) error {
	if !pathWithin(path, configHome) {
		return errors.New("directory escaped configuration root")
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	current := path
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
			info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("unsafe configuration directory %s", current)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("configuration directory is not operator-owned: %s", current)
		}
		if current == filepath.Clean(configHome) {
			break
		}
		next := filepath.Dir(current)
		if next == current || !pathWithin(next, configHome) {
			return errors.New("configuration directory escaped its root")
		}
		current = next
	}
	return nil
}

func writeFileDurable(
	configHome, target string,
	content []byte,
	mode os.FileMode,
) error {
	if !pathWithin(target, configHome) {
		return errors.New("write target escaped configuration root")
	}
	directory := filepath.Dir(target)
	if err := ensureDirectory(configHome, directory, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("write target is not a regular non-symlink file: %s", target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(directory, "."+filepath.Base(target)+".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(content); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, target); err != nil {
		return err
	}
	cleanup = false
	return syncDirectory(directory)
}

func readLiveContent(configHome, path string) ([]byte, uint32, error) {
	_, exists, err := inspectLiveFile(configHome, path)
	if err != nil {
		return nil, 0, err
	}
	if !exists {
		return nil, 0, os.ErrNotExist
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxAssetBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(content) > maxAssetBytes {
		return nil, 0, errors.New("live file exceeds backup limit")
	}
	return content, uint32(info.Mode().Perm()), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func safeTransactionID(value string) bool {
	if value == "" || strings.ContainsAny(value, `/\`) {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && char != '-' {
			return false
		}
	}
	return true
}
