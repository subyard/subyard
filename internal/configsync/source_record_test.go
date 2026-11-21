package configsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigurationSourceRecordIsProtectedAndIdempotent(t *testing.T) {
	configHome := filepath.Join(testkit.TempDir(t), "config")
	checkout := filepath.Join(testkit.TempDir(t), "checkout")
	if err := RegisterSource(configHome, checkout); err != nil {
		t.Fatal(err)
	}
	record, exists, err := ReadSourceRecord(configHome)
	if err != nil || !exists {
		t.Fatalf("read source record: %#v %v %v", record, exists, err)
	}
	if record.SchemaVersion != 1 || record.Checkout != checkout {
		t.Fatalf("unexpected source record: %#v", record)
	}
	info, err := os.Lstat(SourceRecordPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("unsafe source record mode: %v", info.Mode())
	}
	if err := RegisterSource(configHome, checkout); err != nil {
		t.Fatalf("idempotent registration failed: %v", err)
	}
	if err := RegisterSource(configHome, filepath.Join(testkit.TempDir(t), "other")); err == nil ||
		!strings.Contains(err.Error(), "already registered") {
		t.Fatalf("source replacement was accepted: %v", err)
	}
}

func TestConfigurationSourceRecordPinsGitOrigin(t *testing.T) {
	configHome := filepath.Join(testkit.TempDir(t), "config")
	checkout := filepath.Join(testkit.TempDir(t), "checkout")
	origin := "git@example.invalid:private/config.git"
	if err := RegisterSourceOrigin(configHome, checkout, origin); err != nil {
		t.Fatal(err)
	}
	record, exists, err := ReadSourceRecord(configHome)
	if err != nil || !exists || record.Origin != origin {
		t.Fatalf("source origin record: %#v exists=%v err=%v", record, exists, err)
	}
	if err := RegisterSourceOrigin(
		configHome, checkout, "git@example.invalid:other/config.git",
	); err == nil {
		t.Fatal("changed source origin was accepted")
	}
}

func TestConfigurationSourceRecordRejectsUnsafeState(t *testing.T) {
	t.Run("relative registration", func(t *testing.T) {
		if err := RegisterSource(filepath.Join(testkit.TempDir(t), "config"), "relative"); err == nil {
			t.Fatal("relative checkout was accepted")
		}
	})
	t.Run("symlink record", func(t *testing.T) {
		configHome := filepath.Join(testkit.TempDir(t), "config")
		if err := os.MkdirAll(filepath.Join(configHome, ".sync"), 0o700); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(testkit.TempDir(t), "source.json")
		testkit.WriteFile(t, external, []byte("{}\n"), 0o600)
		if err := os.Symlink(external, SourceRecordPath(configHome)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadSourceRecord(configHome); err == nil ||
			!strings.Contains(err.Error(), "regular 0600") {
			t.Fatalf("symlink record was accepted: %v", err)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		configHome := filepath.Join(testkit.TempDir(t), "config")
		path := SourceRecordPath(configHome)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path,
			[]byte(`{"schemaVersion":1,"checkout":"/safe","extra":true}`+"\n"),
			0o600)
		if _, _, err := ReadSourceRecord(configHome); err == nil {
			t.Fatal("unknown source record field was accepted")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, exists, err := ReadSourceRecord(filepath.Join(testkit.TempDir(t), "missing")); err != nil ||
			exists {
			t.Fatalf("missing source record: exists=%v err=%v", exists, err)
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		configHome := filepath.Join(testkit.TempDir(t), "config")
		path := SourceRecordPath(configHome)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(testkit.TempDir(t), "source.json")
		testkit.WriteFile(t, external,
			[]byte(`{"schemaVersion":1,"checkout":"/safe"}`+"\n"),
			0o600)
		if err := os.Link(external, path); err != nil {
			if errors.Is(err, os.ErrPermission) {
				t.Skip(err)
			}
			t.Fatal(err)
		}
		if _, _, err := ReadSourceRecord(configHome); err == nil ||
			!strings.Contains(err.Error(), "hard links") {
			t.Fatalf("hard-linked source record was accepted: %v", err)
		}
	})
}
