package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityPrerequisitePlanPassesOperationPolicy(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution := &initExecution{loaded: loaded, mode: initReconcile}
	execution.hostID, execution.hostIDPending, err = configsync.ResolveHostID(loaded.Context.Paths.ConfigHome, loaded.Environment)
	if err != nil {
		t.Fatal(err)
	}
	runtime := reconcileruntime.Runtime{RepositoryRoot: "../..", Yard: loaded.Context, Incus: &testkit.Incus{}, Integrations: []profile.IntegrationDefinition{}, Profiles: []profile.Definition{{Name: "synthetic", Runtime: &profile.RuntimeHook{ActivationID: "synthetic-runtime", ProjectsChangedHooks: []string{"/usr/local/libexec/subyard/synthetic"}}}}}
	execution.runtimePlan, err = runtime.DeferredIdentityRuntimes()
	if err != nil {
		t.Fatal(err)
	}
	execution.hookPlan, err = runtime.PrepareProjectHooks(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	execution.hookPlan.CaptureOwnerProjects([]domain.ProjectRecord{{Schema: 1, ProjectID: "project", Name: "project", YardPath: "/srv/workspaces/project/src", SSHHost: "yard", Mode: domain.ProjectSync}})
	runtime.HookPlan = execution.hookPlan
	if err := runtime.PrepareProjectMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, stage := range application.InitStages(loaded.Context) {
		execution.plan.Steps = append(execution.plan.Steps, application.ReconcileStep{Stage: stage, Conditional: true})
	}
	steps := identityPrerequisiteSteps(execution.operationSteps(), "project/yard", "rename developer and canonical home; preserve UID, GID and retained data")
	if err := domain.ValidateOperationSteps(steps); err != nil {
		t.Fatal(err)
	}
	if len(steps) < 5 || steps[0].ID != "init.developer-identity" {
		t.Fatal("identity plan lost its prerequisite or ordinary stages")
	}
	for _, expected := range []string{"init.host-id", "init.runtime.synthetic-runtime", "init.project-hooks", "init.project-metadata"} {
		found := false
		for _, step := range steps {
			found = found || step.ID == expected
		}
		if !found {
			t.Fatalf("composed identity plan omitted %s", expected)
		}
	}
}

func TestDeveloperIdentityMarkerCreatesPrivateAuthorityWithoutReplacingConcurrentFile(t *testing.T) {
	root := testkit.TempDir(t)
	path := config.DeveloperIdentityMarkerPath(root, "default")
	content := []byte(`{"schema":1,"proof":"captured-private-completion-proof"}`)
	snapshot, err := createDeveloperIdentityMarker(root, path, config.PersistentFileSnapshot{}, content)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Exists || snapshot.Identity.Mode&0777 != 0600 || string(snapshot.Content) != string(content) {
		t.Fatal("marker publication lost exact private content or mode")
	}
	if _, err := createDeveloperIdentityMarker(root, path, config.PersistentFileSnapshot{}, []byte("foreign")); !errors.Is(err, config.ErrPersistentTargetStale) {
		t.Fatalf("concurrent marker was not protected: %v", err)
	}
	current, err := config.ReadPersistentFileSnapshot(root, path)
	if err != nil || string(current.Content) != string(content) || current.Identity != snapshot.Identity {
		t.Fatal("concurrent creation changed the published authority", err)
	}
	if err := config.RemovePersistentFileSnapshot(root, path, snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityContinuationScope(t *testing.T) {
	approved := []domain.OperationStep{{ID: "init.stage.provision", Target: "project/yard", Decision: domain.StepConditional}}
	if err := identityContinuationScope(approved, approved); err != nil {
		t.Fatal(err)
	}
	if err := identityContinuationScope(approved, nil); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []domain.OperationStep{{ID: "init.reset", Target: "project/yard"}, {ID: "init.stage.provision", Target: "foreign/yard"}} {
		if err := identityContinuationScope(approved, []domain.OperationStep{changed}); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("changed continuation admitted: %v", err)
		}
	}
}

func TestDeveloperIdentityAdmissionBindsSourceAndDesiredConfiguration(t *testing.T) {
	for _, change := range []string{"none", "source", "desired", "release"} {
		t.Run(change, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(loaded.Context.Paths.ConfigHome, 0o700); err != nil {
				t.Fatal(err)
			}
			peers, err := program.powerYardContexts(loaded)
			if err != nil {
				t.Fatal(err)
			}
			inputs, err := program.captureOwnerInputs(loaded, "", peers)
			if err != nil {
				t.Fatal(err)
			}
			release, err := identityReleaseBinding(loaded.Context.Paths.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			marker := config.DeveloperIdentityMarker{Schema: 1, Yard: "default", Inputs: inputs.binding(), Identity: operationStateDigest("guest"), Release: release}
			content, _ := json.Marshal(marker)
			writeCLIFile(t, config.DeveloperIdentityMarkerPath(loaded.Context.Paths.ConfigHome, "default"), string(content), 0o600)
			switch change {
			case "source":
				writeCLIFile(t, filepath.Join(root, "scripts", "identity-source.sh"), "# changed source\n", 0o644)
			case "desired":
				writeCLIFile(t, filepath.Join(loaded.Context.Paths.ConfigHome, "config.env"), "DEV_USER=changed\n", 0o600)
			case "release":
				if err := os.MkdirAll(filepath.Join(loaded.Context.Paths.ConfigHome, "release-transition", "v2"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, filepath.Join(loaded.Context.Paths.ConfigHome, "release-transition", "v2", "journal.json"), "{}\n", 0o600)
			}
			allowed, err := program.admitDeveloperIdentity(context.Background(), "default", "@init", nil)
			if change == "none" && (err != nil || !allowed) {
				t.Fatalf("exact recovery refused: %v", err)
			}
			if change != "none" && err == nil {
				t.Fatal("changed pending identity admitted")
			}
			if recovery, err := program.admitDeveloperIdentity(context.Background(), "default", "@teardown", []string{"--full"}); err != nil || recovery {
				t.Fatalf("independent full removal refused or treated as identity recovery: %v", err)
			}
			for _, arguments := range [][]string{{"--reset-config"}, {"--keep-data"}, {"--full", "--keep-data"}} {
				if _, err := program.admitDeveloperIdentity(context.Background(), "default", "@teardown", arguments); err == nil {
					t.Fatal("ordinary or invalid teardown bypassed pending identity")
				}
			}
			if _, err := program.admitDeveloperIdentity(context.Background(), "remote/default", "@teardown", []string{"--full"}); err == nil {
				t.Fatal("remote full removal bypassed local owner admission")
			}
			for _, handler := range []string{"@update", "@migrate", "@config", "@lifecycle", "@teardown"} {
				if _, err := program.admitDeveloperIdentity(context.Background(), "default", handler, nil); err == nil {
					t.Fatalf("%s admitted pending identity", handler)
				}
			}
			if _, err := program.admitDeveloperIdentity(context.Background(), "default", "@init", []string{"--reset"}); err == nil {
				t.Fatal("reset admitted pending identity")
			}
		})
	}
}
