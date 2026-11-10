package migration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/contracttest"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLegacyGitConfigReadinessRefusesWithoutChangingOperatorFile(t *testing.T) {
	for _, content := range []string{"", "[user]\nname = Yard Developer\nemail = yard@example.test\n[core]\neditor = private-editor-value\n[include]\npath = absent-file\n"} {
		yard := domain.Context{YardName: "sample", Paths: domain.RuntimePaths{DataHome: testkit.TempDir(t)}}
		path := filepath.Join(yard.Paths.DataHome, "gitconfig")
		if err := CheckLegacyGitConfig(context.Background(), yard); err != nil {
			t.Fatalf("absent drop-in: %v", err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckLegacyGitConfig(context.Background(), yard)
		if err == nil || !strings.Contains(err.Error(), "move or delete") || !strings.Contains(err.Error(), path) {
			t.Fatalf("legacy file did not refuse with a preservation route: %v", err)
		}
		if content != "" {
			for _, expected := range []string{
				"yard -Y sample config set GIT_USER_NAME 'Yard Developer'", "yard -Y sample config set GIT_USER_EMAIL 'yard@example.test'",
				"--scope yard", "core.editor", "include.path", "no longer managed by Subyard",
			} {
				if !strings.Contains(err.Error(), expected) {
					t.Errorf("missing %q in %v", expected, err)
				}
			}
			if strings.Contains(err.Error(), "private-editor-value") {
				t.Fatal("diagnostic exposed an unrelated Git value")
			}
			var refusals *domain.Refusals
			if !errors.As(err, &refusals) || len(refusals.Unwrap()) != 5 {
				t.Fatalf("independent legacy findings lost: %v", err)
			}
			for _, finding := range refusals.Unwrap() {
				var diagnostic interface{ ActivationDiagnostic() (string, string) }
				if !errors.As(finding, &diagnostic) {
					t.Fatalf("missing public diagnostic: %v", finding)
				}
				message, next := diagnostic.ActivationDiagnostic()
				if !strings.Contains(message, "yard sample") || next == "" || len(message) > 512 || strings.ContainsAny(message, "\n\r") {
					t.Fatalf("invalid activation diagnostic: %q / %q", message, next)
				}
			}
		}
		again := CheckLegacyGitConfig(context.Background(), yard)
		if again == nil || again.Error() != err.Error() {
			t.Fatal("legacy findings are unstable")
		}
		after, readErr := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if readErr != nil || statErr != nil || string(after) != content || !os.SameFile(before, info) || before.Mode() != info.Mode() || before.ModTime() != info.ModTime() {
			t.Fatal("readiness modified the legacy file")
		}
	}
}

func TestLegacyGitConfigReadinessRefusesUnreadableConfiguration(t *testing.T) {
	for _, kind := range []string{"malformed", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			yard := domain.Context{YardName: "sample", Paths: domain.RuntimePaths{DataHome: testkit.TempDir(t)}}
			path := filepath.Join(yard.Paths.DataHome, "gitconfig")
			switch kind {
			case "malformed":
				testkit.WriteFile(t, path, []byte("[invalid\n"), 0600)
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("absent", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := CheckLegacyGitConfig(context.Background(), yard); err == nil || !strings.Contains(err.Error(), "move or delete") {
				t.Fatalf("unsafe legacy drop-in was accepted: %v", err)
			}
		})
	}
}

func TestCheckValidatesExistingStoresWithoutCreatingMissingOnes(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "projects")
	store, err := state.NewFileStore(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), contracttest.ProjectRecord("migration-a")); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	report, err := Check(
		context.Background(), []string{missing, existing}, &testkit.CredentialMetadataReader{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.ProjectStoresValidated != 1 || report.Changed || report.ProjectStateSchema != 1 {
		t.Fatalf("unexpected migration report: %#v", report)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("migration check created a missing store")
	}
}

func TestApplyRepairsLegacyProjectPermissions(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "projects")
	store, err := state.NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), contracttest.ProjectRecord("legacy-a")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "legacy-a.json")
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(context.Background(), []string{directory}, nil); err == nil {
		t.Fatal("check accepted legacy broad permissions without applying migration")
	}
	report, err := Apply(context.Background(), []string{directory}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.ProjectStoresValidated != 1 {
		t.Fatalf("unexpected apply report: %#v", report)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("applied state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCheckAndApplyLegacyProjectNameMigration(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	records := []domain.ProjectRecord{
		{
			Schema: 1, ProjectID: "legacy-a", Name: "Demo",
			HostPath: "/workspace/one/Demo", YardPath: state.YardPath("legacy-a"),
			Mode: domain.ProjectSync, SSHHost: "yard",
			ImportedAt: "2026-01-01T00:00:00Z",
		},
		{
			Schema: 1, ProjectID: "legacy-b", Name: "demo",
			HostPath: "/workspace/two/Demo", YardPath: state.YardPath("legacy-b"),
			Mode: domain.ProjectSync, SSHHost: "yard",
			ImportedAt: "2026-02-01T00:00:00Z",
		},
	}
	for _, record := range records {
		payload, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		payload = append(payload, '\n')
		if err := os.WriteFile(
			filepath.Join(directory, record.ProjectID+".json"), payload, 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}

	report, err := Check(context.Background(), []string{directory}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pending ||
		!slices.Equal(report.RequiredMigrations, []string{projectNameMigrationID}) ||
		!slices.Equal(report.AffectedResources, []string{directory}) {
		t.Fatalf("legacy name migration was not reported: %#v", report)
	}
	firstBefore, err := os.ReadFile(filepath.Join(directory, "legacy-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBefore) != string(appendMustMarshal(t, records[0], '\n')) {
		t.Fatal("migration check changed project state")
	}

	report, err = Apply(context.Background(), []string{directory}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed {
		t.Fatalf("legacy name migration was not applied: %#v", report)
	}
	store, err := state.NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(context.Background(), "legacy-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Get(context.Background(), "legacy-b")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "Demo" || second.Name != "Demo-2" {
		t.Fatalf("migrated project names = %q, %q", first.Name, second.Name)
	}
	report, err = Check(context.Background(), []string{directory}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pending || len(report.RequiredMigrations) != 0 {
		t.Fatalf("applied name migration remained pending: %#v", report)
	}
}

func appendMustMarshal(t *testing.T, value any, suffix byte) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return append(payload, suffix)
}
