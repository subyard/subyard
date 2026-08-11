package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePersistentAssignmentPreservesUnrelatedRecords(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "config.env")
	initial := "# keep this comment\nSSH_PORT=2200\nDEV_USER='dev'\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	value := "2299"
	if err := WritePersistentAssignment(
		configHome, path, "SSH_PORT", &value,
	); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "# keep this comment\n") ||
		!strings.Contains(string(content), "DEV_USER='dev'\n") ||
		!strings.Contains(string(content), "SSH_PORT='2299'\n") {
		t.Fatalf("unexpected edited config:\n%s", content)
	}
	if err := WritePersistentAssignment(
		configHome, path, "SSH_PORT", nil,
	); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "SSH_PORT") ||
		!strings.Contains(string(content), "DEV_USER='dev'") {
		t.Fatalf("unexpected unset config:\n%s", content)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("persistent config mode = %o", info.Mode().Perm())
	}
}

func TestCreatePersistentFileRefusesExistingTarget(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "yards", "hermes", "config.env")
	content := []byte("ENVIRONMENT_PROFILES=hermes\n")
	if err := CreatePersistentFile(configHome, path, content); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || string(stored) != string(content) {
		t.Fatalf("unexpected persistent file: mode=%v content=%q", info.Mode(), stored)
	}
	if err := CreatePersistentFile(configHome, path, []byte("CODING_TOOL_INTEGRATIONS=claude\n")); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing target error=%v", err)
	}
	stored, err = os.ReadFile(path)
	if err != nil || string(stored) != string(content) {
		t.Fatalf("existing target was changed: content=%q err=%v", stored, err)
	}
}

func TestCreatePersistentFileCreatesMissingConfigurationRoot(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(configHome, "yards", "hermes", "config.env")
	content := []byte("ENVIRONMENT_PROFILES=hermes\n")
	if err := CreatePersistentFile(configHome, path, content); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != string(content) {
		t.Fatalf("persistent file content=%q err=%v", stored, err)
	}
	info, err := os.Lstat(configHome)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("configuration root mode=%v err=%v", info.Mode(), err)
	}
}
