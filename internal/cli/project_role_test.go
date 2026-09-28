package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/state"
)

func TestProjectRoleRejectsAdmissionBeforeStateMutation(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	writeDisabledProjectRole(t, root)
	source := filepath.Join(root, "sample-source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := program.prepareProjectExecution(context.Background(), loaded, command.Definition{Name: "sync"},
		[]string{source}, true, false); err == nil || !strings.Contains(err.Error(), "does not accept work projects") {
		t.Fatalf("project sync reached disabled yard: %v", err)
	}
	for _, test := range []struct {
		owner bool
		args  []string
	}{
		{true, []string{"preview", source, "sync", "sample", "0"}},
		{true, []string{"reserve", "op-test", source, "sync", "sample", "0"}},
		{true, []string{"upsert", "sample-id", "sample", "sync", "yard"}},
		{false, []string{"write", "sample-id", "sample", source, "/srv/workspaces/sample-id/src", "sync", "yard"}},
		{false, []string{"set", "sample-id", "name", "sample"}},
		{false, []string{"upsert-yard", "sample-id", "sample", "sync", "yard", "yard"}},
	} {
		var output bytes.Buffer
		program.options.Stderr = &output
		if code := program.runProjectState(context.Background(), loaded, test.args, test.owner); code != 1 ||
			!strings.Contains(output.String(), "does not accept work projects") {
			t.Fatalf("internal state %v owner=%t code=%d error=%q", test.args, test.owner, code, output.String())
		}
	}
	if _, err := os.Lstat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("denied project admission created state: %v", err)
	}
}

func TestProjectRoleDirectInternalEndpointRejectsAdmission(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	writeDisabledProjectRole(t, root)
	manifest := filepath.Join(root, "config", "commands.registry")
	current, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, manifest, string(current)+"_project-state||@project-state||local|mutate|dynamic|hidden|internal|none|_project-state|owner project state||\n", 0o600)
	for _, arguments := range [][]string{
		{"_project-state", "preview", "/synthetic/source", "sync", "sample", "0"},
		{"_state", "upsert-yard", "sample-id", "sample", "sync", "yard", "yard"},
	} {
		var stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
			Arguments: arguments, Stderr: &stderr})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "does not accept work projects") {
			t.Errorf("direct state endpoint %v code=%d error=%q", arguments, code, stderr.String())
		}
	}
	if _, err := os.Lstat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("direct owner endpoint created project state: %v", err)
	}
}

func TestProjectRoleRejectsExistingProjectCommands(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	writeDisabledProjectRole(t, root)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: "/srv/workspaces/demo-12345678/src", Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "yard",
	}); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shell", "up", "code"} {
		if _, err := program.prepareProjectExecution(context.Background(), loaded, command.Definition{Name: name},
			[]string{"demo-12345678"}, true, false); err == nil || !strings.Contains(err.Error(), "does not accept work projects") {
			t.Errorf("existing project %s reached disabled yard: %v", name, err)
		}
	}
}

func TestProjectRoleRecheckedBeforeMutation(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "config", "yards", "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state", "yards", "default"), 0o700); err != nil {
		t.Fatal(err)
	}
	preset := filepath.Join(root, "config", "yards", "profiles", "projects.env")
	writeCLIFile(t, preset, "ALLOWS_PROJECTS=true\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "state", "yards", "default", "config.env"), "YARD_TEMPLATE=projects\n", 0o600)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, preset, "ALLOWS_PROJECTS=false\n", 0o600)
	if _, err := program.beginProjectMutation(context.Background(), &projectExecution{Loaded: loaded, RequiresProjects: true}); err == nil || !strings.Contains(err.Error(), "does not accept work projects") {
		t.Fatalf("project role change was not rechecked: %v", err)
	}
	if _, err := os.Lstat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("denied mutation created state: %v", err)
	}
}

func writeDisabledProjectRole(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "state", "yards", "default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "config", "yards", "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "yards", "profiles", "no-projects.env"), "ALLOWS_PROJECTS=false\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "state", "yards", "default", "config.env"), "YARD_TEMPLATE=no-projects\n", 0o600)
}
