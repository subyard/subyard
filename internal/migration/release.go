package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const migrationStateSchema = 1

// ReleaseOptions contains the trusted roots and runtime adapters needed to
// inspect a retained v1 release journal. The v1 path is read-only: all new
// transition planning and mutation belongs to releasetransition.V2Transition.
type ReleaseOptions struct {
	RegistryPath   string
	RepositoryRoot string
	RuntimeRoot    string
	ConfigHome     string
	DataHome       string
	Version        string
	Executable     string
	Incus          string
	Environment    []string
	Diagnostics    io.Writer
	Stderr         io.Writer
}

type transaction struct {
	SchemaVersion int                    `json:"schemaVersion"`
	FromLayout    int                    `json:"fromLayout"`
	ToLayout      int                    `json:"toLayout"`
	FromRuntime   string                 `json:"fromRuntime,omitempty"`
	ToRelease     string                 `json:"toRelease"`
	ToRuntime     string                 `json:"toRuntime,omitempty"`
	Phase         string                 `json:"phase"`
	Migrations    []string               `json:"migrations"`
	Entries       []transactionEntry     `json:"entries,omitempty"`
	Operations    []transactionOperation `json:"operations,omitempty"`
	RollbackOps   bool                   `json:"rollbackOperations,omitempty"`
}

type transactionEntry struct {
	MigrationID string `json:"migrationId"`
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Consumer    string `json:"consumer"`
	Recovery    string `json:"recovery"`
	Digest      string `json:"sha256,omitempty"`
	Mode        uint32 `json:"mode,omitempty"`
}

type transactionOperation struct {
	MigrationID string `json:"migrationId"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	Before      string `json:"before,omitempty"`
	Phase       string `json:"phase,omitempty"`
}

func transactionOperationIndex(tx transaction, migrationID, operationID string) int {
	for index, operation := range tx.Operations {
		if operation.MigrationID == migrationID && operation.OperationID == operationID {
			return index
		}
	}
	return -1
}

func flattenEntries(path []Definition) []transactionEntry {
	var entries []transactionEntry
	for _, definition := range path {
		for _, move := range definition.Moves {
			entries = append(entries, transactionEntry{
				MigrationID: definition.ID,
				Scope:       move.Scope,
				Source:      move.Source,
				Destination: move.Destination,
				Consumer:    move.Consumer,
				Recovery:    filepath.ToSlash(filepath.Join("recovery", fmt.Sprintf("%04d", len(entries)))),
			})
		}
	}
	return entries
}

func flattenOperations(path []Definition) []transactionOperation {
	var operations []transactionOperation
	for _, definition := range path {
		for _, operation := range definition.Operations {
			operations = append(operations, transactionOperation{
				MigrationID: definition.ID,
				OperationID: operation.ID,
				Kind:        operation.Kind,
			})
		}
	}
	return operations
}

func transactionOperationDefinitions(
	registry Registry,
	fromLayout int,
	path []Definition,
) []Definition {
	definitions := make([]Definition, 0, len(registry.Migrations)+len(path))
	for _, definition := range registry.Migrations {
		if definition.ToLayout > fromLayout {
			break
		}
		if len(definition.Operations) > 0 {
			definitions = append(definitions, definition)
		}
	}
	return append(definitions, path...)
}

func transactionOperationDefinitionsForTransaction(
	registry Registry,
	tx transaction,
) ([]Definition, error) {
	path, err := transactionDefinitions(registry, tx)
	if err != nil {
		return nil, err
	}
	return transactionOperationDefinitions(registry, tx.FromLayout, path), nil
}

func validateTransaction(options ReleaseOptions, registry Registry, tx transaction) error {
	if tx.SchemaVersion != migrationStateSchema || tx.ToRelease != options.Version {
		return errors.New("migration transaction identity does not match active release")
	}
	switch tx.Phase {
	case "preparing", "prepared", "committing", "committed", "rolling-back", "rolled-back":
	default:
		return fmt.Errorf("unknown migration transaction phase %q", tx.Phase)
	}
	if tx.RollbackOps && tx.Phase != "rolling-back" && tx.Phase != "rolled-back" {
		return errors.New("migration transaction has invalid operation rollback authority")
	}
	if (tx.FromRuntime != "" && !safeReleaseIdentity(tx.FromRuntime)) ||
		(tx.ToRuntime != "" && !safeReleaseIdentity(tx.ToRuntime)) {
		return errors.New("migration transaction contains an unsafe runtime identity")
	}
	path, err := transactionDefinitions(registry, tx)
	if err != nil {
		return err
	}
	expectedEntries := flattenEntries(path)
	if len(expectedEntries) != len(tx.Entries) {
		return errors.New("migration transaction entry count does not match release registry")
	}
	for index, entry := range tx.Entries {
		expected := expectedEntries[index]
		if entry.MigrationID != expected.MigrationID || entry.Scope != expected.Scope ||
			entry.Source != expected.Source || entry.Destination != expected.Destination ||
			entry.Consumer != expected.Consumer || entry.Recovery != expected.Recovery ||
			(tx.Phase != "preparing" && tx.Phase != "rolled-back" && entry.Digest == "") ||
			entry.Mode&0o022 != 0 {
			return errors.New("migration transaction entry does not match release registry")
		}
	}
	operationPath, err := transactionOperationDefinitionsForTransaction(registry, tx)
	if err != nil {
		return err
	}
	expectedOperations := flattenOperations(operationPath)
	if len(expectedOperations) != len(tx.Operations) {
		return errors.New("migration transaction operation count does not match release registry")
	}
	for index, operation := range tx.Operations {
		expected := expectedOperations[index]
		if operation.MigrationID != expected.MigrationID ||
			operation.OperationID != expected.OperationID || operation.Kind != expected.Kind {
			return errors.New("migration transaction operation does not match release registry")
		}
		if operation.Before == "" && tx.Phase != "preparing" && tx.Phase != "rolled-back" {
			return errors.New("migration transaction operation has no prepared state")
		}
		if operation.Before != "" {
			if err := validateOperationState(operation); err != nil {
				return err
			}
		}
	}
	return nil
}

func transactionDefinitions(registry Registry, tx transaction) ([]Definition, error) {
	if tx.FromLayout == tx.ToLayout {
		return nil, errors.New("same-layout v1 maintenance transactions are superseded")
	}
	path, err := registry.Path(tx.FromLayout)
	if err != nil {
		return nil, err
	}
	for index, definition := range path {
		if definition.ToLayout == tx.ToLayout {
			path = path[:index+1]
			break
		}
	}
	if len(path) == 0 || path[len(path)-1].ToLayout != tx.ToLayout {
		return nil, errors.New("migration transaction layout does not match release registry")
	}
	return path, nil
}

func safeReleaseIdentity(value string) bool {
	if value == "" || strings.ContainsAny(value, "\x00\n\r\t") ||
		filepath.IsAbs(value) || strings.Contains(value, "..") {
		return false
	}
	if strings.Contains(value, "/") {
		releaseID, ok := strings.CutPrefix(value, "releases/")
		if !ok || releaseID == "" || strings.Contains(releaseID, "/") {
			return false
		}
	}
	for _, character := range value {
		if character == '/' || character == '.' || character == '_' ||
			character == '+' || character == '-' ||
			character >= '0' && character <= '9' ||
			character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' {
			continue
		}
		return false
	}
	return true
}

func migrationRoot(configHome string) string {
	return filepath.Join(configHome, "migrations")
}

func transactionDirectory(configHome, version string) string {
	sum := sha256.Sum256([]byte(version))
	return filepath.Join(migrationRoot(configHome), "transactions", hex.EncodeToString(sum[:16]))
}

func transactionPath(configHome, version string) string {
	return filepath.Join(transactionDirectory(configHome, version), "transaction.json")
}

func validateDirectoryUnder(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || filepath.IsAbs(relative) ||
		(!safeRelativePath(relative) && relative != ".") {
		return errors.New("migration path escapes its allowlisted root")
	}
	current := filepath.Clean(root)
	for _, component := range append([]string{""}, splitPath(relative)...) {
		if component != "" {
			current = filepath.Join(current, component)
		}
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("migration path contains a symlink or non-directory")
		}
		if err := validateOwnedSafeMode(current, info); err != nil {
			return err
		}
	}
	return nil
}

func splitPath(path string) []string {
	if path == "." {
		return nil
	}
	var components []string
	for path != "." {
		directory, base := filepath.Split(path)
		components = append([]string{base}, components...)
		path = filepath.Clean(directory)
	}
	return components
}

func currentRuntimeTarget(runtimeRoot string) string {
	return runtimeLinkTarget(runtimeRoot, "current")
}

func runtimeLinkTarget(runtimeRoot, name string) string {
	target, err := os.Readlink(filepath.Join(runtimeRoot, name))
	if err != nil {
		return ""
	}
	return target
}
