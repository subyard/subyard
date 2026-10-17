package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/state"
)

func TestProjectTargetAdmissionRequiresActiveProfileAndRetainsShutdown(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	writeProvisionProfile(t, root, "sample")
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bind", []string{source, "--target", "sample"}},
		{"sync", []string{source, "--target", "sample"}},
		{"clone", []string{"https://example.invalid/sample.git", "--target", "sample"}},
	} {
		_, err := program.prepareProjectExecution(context.Background(), loaded, command.Definition{Name: tc.name}, tc.args, true, false)
		if err == nil || !strings.Contains(err.Error(), "not active") {
			t.Fatalf("%s inactive admission=%v", tc.name, err)
		}
		if _, err := os.Lstat(stateDirectory); !os.IsNotExist(err) {
			t.Fatalf("inactive admission created state: %v", err)
		}
	}
	active, err := config.WithEnvironmentProfiles(loaded, []string{"sample"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProjectTarget(root, active, "sample"); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	record := domain.ProjectRecord{Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo", HostPath: source, SourceKey: state.SourceKey(source), YardPath: state.YardPath("Demo"), Mode: domain.ProjectSync, SSHHost: "yard", Target: "sample", Profile: "sample"}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"up", "down", "remove"} {
		_, err := program.prepareProjectExecution(context.Background(), loaded, command.Definition{Name: name}, []string{"Demo"}, true, false)
		if name == "up" {
			if err == nil || !strings.Contains(err.Error(), "not active") {
				t.Fatalf("inactive up=%v", err)
			}
		} else if err != nil {
			t.Fatalf("retained %s=%v", name, err)
		}
	}
	// Changing selection after consent must fail before a project mutation.
	execution := &projectExecution{Loaded: active, Record: record, RequiresProjects: true, RequiresActiveTarget: true}
	if _, err := program.beginProjectMutation(context.Background(), execution); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("deselection after consent=%v", err)
	}
}

func TestProjectBindReimportCapturesMigratedIdentityBeforeConsent(t *testing.T) {
	for _, scenario := range []string{"reimport", "record changed after preparation", "inactive retained target"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			root, environment, stateDirectory := nativeFixture(t)
			source := filepath.Join(root, "Demo")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			legacy := domain.ProjectRecord{Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: source, YardPath: "/srv/workspaces/demo-12345678/src", Mode: domain.ProjectBind, SSHHost: "yard", Target: "yard", ImportedAt: "2026-01-01T00:00:00Z"}
			if scenario == "inactive retained target" {
				writeProvisionProfile(t, root, "sample")
				legacy.Target = "sample"
			}
			store, err := state.NewFileStore(stateDirectory)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Put(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			execution, err := program.prepareProjectImport(ctx, loaded, "bind", []string{source})
			if scenario == "inactive retained target" {
				if err == nil || !strings.Contains(err.Error(), "not active") {
					t.Fatalf("inactive retained bind admitted: %v", err)
				}
				retained, readErr := store.GetReadOnly(ctx, legacy.ProjectID)
				if readErr != nil || retained != legacy {
					t.Fatalf("inactive admission migrated retained record: %#v %v", retained, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			migrated, err := store.GetReadOnly(ctx, legacy.ProjectID)
			if err != nil || migrated.SourceKey != state.SourceKey(source) {
				t.Fatalf("legacy migration=%#v %v", migrated, err)
			}
			if execution.Record.ProjectID != legacy.ProjectID || execution.PreviewExisting == nil || *execution.PreviewExisting != migrated {
				t.Fatalf("prepared stale identity after own migration: %#v", execution.PreviewExisting)
			}
			if scenario == "record changed after preparation" {
				migrated.ImportedAt = "2026-02-01T00:00:00Z"
				if err := store.Put(ctx, migrated); err != nil {
					t.Fatal(err)
				}
				if err := program.reserveProjectExecution(ctx, execution); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("external drift admitted: %v", err)
				}
				return
			}
			if err := program.reserveProjectExecution(ctx, execution); err != nil {
				t.Fatalf("retained bind reservation failed after migration: %v", err)
			}
			if err := program.commitProjectExecution(ctx, execution); err != nil {
				t.Fatal(err)
			}
			records, err := store.ListReadOnly(ctx)
			if err != nil || len(records) != 1 || records[0].ProjectID != legacy.ProjectID || records[0].SourceKey != state.SourceKey(source) {
				t.Fatalf("reimport identity changed: %#v %v", records, err)
			}
		})
	}
}
