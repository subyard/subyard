package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
