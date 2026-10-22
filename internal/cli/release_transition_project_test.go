package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
	"github.com/lxc/incus/v6/shared/api"
)

func migrationProjectFixture(t *testing.T, yard string) (*CLI, config.Loaded, *incusclient.Client, *atomic.Int32, *testkit.IncusServer) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	helper, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts/lib/project-config.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(root, "scripts/lib/project-config.sh"), helper, 0644)
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644)
	testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644)
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	writeReleaseTransitionTestFile(t, filepath.Join(home, "yards", yard, "config.env"), []byte("SSH_PORT=2223\nSRV_POOL=default\nSRV_VOLUME=yard-"+yard+"\n"), 0600)
	fake, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	fake.SetExtensions("projects", "instances", "instance_all_projects", "storage", "network")
	t.Cleanup(func() { _ = fake.Close() })
	listener, err := net.Listen("unix", filepath.Join(testkit.TempDir(t), "project.socket"))
	if err != nil {
		t.Fatal(err)
	}
	posts := new(atomic.Int32)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/1.0/projects" {
			fake.ServeHTTP(w, r)
			return
		}
		posts.Add(1)
		var request api.ProjectsPost
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		fake.SetProject(request.Name, map[string]any{"name": request.Name, "config": request.Config})
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status": "Success", "status_code": 200, "metadata": map[string]any{}})
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	client := incusclient.New(listener.Addr().String(), "projects")
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: client, Executor: client, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.resolveReleaseTransitionContext(yard, home)
	if err != nil {
		t.Fatal(err)
	}
	return program, loaded, client, posts, fake
}

func migrationProjectGuard(t *testing.T, program *CLI, loaded config.Loaded) error {
	t.Helper()
	execution := &initExecution{loaded: loaded, platform: program.initPlatform(loaded, []domain.Context{loaded.Context})}
	runtime := execution.platform.(reconcileruntime.Runtime)
	state, err := runtime.Incus.ReconcileState(context.Background(), loaded.Context.IncusProject, loaded.Context.YardInstanceName, runtime.SRVPool, runtime.SRVVolume, loaded.Context.IncusBridge)
	if err != nil || !state.ProjectFound {
		t.Fatalf("native project state unavailable to guard: found=%t err=%v", state.ProjectFound, err)
	}
	return program.prepareLegacyContinuation(context.Background(), execution, initArguments{mode: initReconcile})
}

func TestMigrationSharedProjectCreationPassesRealInitGuard(t *testing.T) {
	for _, yard := range []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard} {
		t.Run(yard, func(t *testing.T) {
			program, loaded, client, posts, _ := migrationProjectFixture(t, yard)
			runtime := program.initPlatform(loaded, []domain.Context{loaded.Context}).(reconcileruntime.Runtime)
			sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer sources.Close()
			rendered, err := runtime.RenderOwnerProjectConfig(context.Background(), sources)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := client.PrepareImageProject(context.Background(), loaded.Context.IncusProject, true)
			if err != nil {
				t.Fatal(err)
			}
			prepared.InitialConfig = rendered
			if err := client.ApplyPreparedImageProject(context.Background(), prepared); err != nil {
				t.Fatal(err)
			}
			if posts.Load() != 1 {
				t.Fatal("missing atomic project creation")
			}
			if err := migrationProjectGuard(t, program, loaded); err != nil {
				t.Fatalf("fresh migration project refused by real init guard: %v", err)
			}
		})
	}
}

func TestMigrationSharedProjectCallbackUsesExactYardContext(t *testing.T) {
	for _, yard := range []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard} {
		t.Run(yard, func(t *testing.T) {
			program, loaded, client, posts, _ := migrationProjectFixture(t, yard)
			options, err := program.releaseTransitionTestYardOptions(releasetransition.ProcessRequest{ConfigHome: loaded.Context.Paths.ConfigHome})
			if err != nil {
				t.Fatal(err)
			}
			if options.EnsureImageProject == nil {
				t.Fatal("ordinary migration has no authenticated project creator")
			}
			if err := options.EnsureImageProject(context.Background(), yard, true); err != nil {
				t.Fatal(err)
			}
			prepared, err := client.PrepareImageProject(context.Background(), loaded.Context.IncusProject, true)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Config["features.images"] != "false" || prepared.Config["user.subyard.installation"] != "1" || prepared.Config["user.subyard.yard_name"] != yard || prepared.Config["user.subyard.data_home"] != loaded.Context.Paths.DataHome || prepared.Config["user.subyard.config_home"] != loaded.Context.Paths.ConfigHome || prepared.Config["restricted"] != "true" {
				t.Fatal("created project lost canonical ownership/policy/image scope")
			}
			if _, present := prepared.Config["user.subyard.owner_create"]; present {
				t.Fatal("ordinary creation gained native custody token")
			}
			if err := migrationProjectGuard(t, program, loaded); err != nil {
				t.Fatal(err)
			}
			// A preexisting project is an exact no-op; rendering is unnecessary.
			if err := os.Remove(filepath.Join(program.options.RepositoryRoot, "scripts/lib/project-config.sh")); err != nil {
				t.Fatal(err)
			}
			if err := options.EnsureImageProject(context.Background(), yard, true); err != nil || posts.Load() != 1 {
				t.Fatal("existing project was rendered or mutated", err)
			}
		})
	}
}

func TestMigrationSharedProjectPreservesExistingUnmarkedGuard(t *testing.T) {
	program, loaded, _, posts, fake := migrationProjectFixture(t, testyardmigration.CurrentYard)
	fake.SetProject(loaded.Context.IncusProject, map[string]any{"name": loaded.Context.IncusProject, "config": map[string]string{"features.images": "false", "foreign": "keep"}})
	options, err := program.releaseTransitionTestYardOptions(releasetransition.ProcessRequest{ConfigHome: loaded.Context.Paths.ConfigHome})
	if err != nil {
		t.Fatal(err)
	}
	if options.EnsureImageProject == nil {
		t.Fatal("missing ordinary project callback")
	}
	if err := options.EnsureImageProject(context.Background(), testyardmigration.CurrentYard, true); err != nil || posts.Load() != 0 {
		t.Fatal("existing foreign project was changed", err)
	}
	if err := migrationProjectGuard(t, program, loaded); err == nil || !strings.Contains(err.Error(), "existing unmarked project has no continuation consent") {
		t.Fatal("foreign project gained init authority", err)
	}
}
