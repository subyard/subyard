package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type cleanupAssessmentFixture struct {
	cleanupDataFixture
	payload []byte
	applied []byte
}

func (f *cleanupAssessmentFixture) Execute(_ context.Context, _ domain.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if request.Command[3] == "assess" {
		return ports.InstanceExecResult{Stdout: f.payload}, nil
	}
	f.applied = append([]byte(nil), request.Stdin...)
	return ports.InstanceExecResult{Stdout: []byte(`{"entries_removed":0,"mounts_preserved":0}`)}, nil
}

func cleanupAssessmentCommand(t *testing.T, payload string) (*preparedCommand, *cleanupAssessmentFixture) {
	t.Helper()
	root := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "scripts", "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts", "lib", "yard-cleanup.py"), []byte("# synthetic cleanup protocol fixture\n"), 0644)
	directory := filepath.Join(root, "config", "profiles", "selected")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "profile.conf"), []byte("CLEANUP_CACHES=/srv/cache/test\n"), 0644)
	data := &cleanupAssessmentFixture{payload: []byte(payload)}
	program := &CLI{options: Options{RepositoryRoot: root, ProjectData: data}}
	return &preparedCommand{CLI: program, Loaded: config.Loaded{Context: domain.Context{DevUser: "dev", Paths: domain.RuntimePaths{DataHome: root}}, Environment: map[string]string{"ENVIRONMENT_PROFILES": "selected"}}}, data
}

func TestCleanupAssessmentPreservesFullWidthIdentityThroughApply(t *testing.T) {
	prepared, data := cleanupAssessmentCommand(t, `[{"path":"/srv/cache/test","identity":{"inode":9007199254740993,"device":18446744073709551615},"preserved":{}}]`)
	if err := prepared.prepareCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.execute(context.Background(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	var applied []struct {
		Identity map[string]json.Number `json:"identity"`
	}
	if err := json.Unmarshal(data.applied, &applied); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Identity["inode"].String() != "9007199254740993" || applied[0].Identity["device"].String() != "18446744073709551615" {
		t.Fatal("assessment rounded the approved native identity before deletion", string(data.applied))
	}
}

func TestCleanupAssessmentRejectsMalformedAndTrailingResponsesBeforeApply(t *testing.T) {
	for _, payload := range []string{`[{}] trailing`, `[{}]{}`, `[]`, `[1]`, `[null]`, `"not roots"`, `{"path":"/srv/cache/test"}`} {
		t.Run(payload, func(t *testing.T) {
			prepared, data := cleanupAssessmentCommand(t, payload)
			if err := prepared.prepareCleanup(context.Background(), nil); err == nil {
				t.Fatal("malformed root assessment was accepted")
			}
			if data.applied != nil {
				t.Fatal("invalid assessment reached deletion")
			}
		})
	}
}

func TestCleanupUsesOnlySelectedExplicitDisposableSubset(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"selected", "other"} {
		directory := filepath.Join(root, "config", "profiles", name)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(directory, "profile.conf"), []byte("CACHES=/srv/cache/runtime\nCLEANUP_CACHES=/srv/cache/"+name+"\n"), 0644)
	}
	program := &CLI{options: Options{RepositoryRoot: root}}
	loaded := config.Loaded{Context: domain.Context{DevUser: "dev"}, Environment: map[string]string{"ENVIRONMENT_PROFILES": "selected"}}
	paths, err := program.cleanupPaths(loaded)
	if err != nil || !slices.Equal(paths, []string{"/srv/cache/selected"}) {
		t.Fatal(paths, err)
	}
	loaded.Environment["ENVIRONMENT_PROFILES"] = ""
	paths, err = program.cleanupPaths(loaded)
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
}

func TestCleanupSoftCommandUsesRegisteredDestructiveAction(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	shipped, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config", "commands.registry"), shipped, 0600)
	if err := os.MkdirAll(filepath.Join(root, "scripts", "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts", "lib", "yard-cleanup.py"), []byte("# fixture\n"), 0644)
	directory := filepath.Join(root, "config", "profiles", "selected")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "profile.conf"), []byte("CLEANUP_CACHES=/srv/cache/test\n"), 0644)
	environment = append(environment, "ENVIRONMENT_PROFILES=selected")
	data := &cleanupAssessmentFixture{payload: []byte(`[{"path":"/srv/cache/test","identity":null}]`)}
	var out bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"cleanup", "--soft", "--yes"}, Environment: environment, WorkingDir: root, Stdout: &out, Stderr: &out, ProjectData: data, Incus: lifecycleIncus()})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("cleanup command failed %d: %s", code, out.String())
	}
	if len(data.applied) == 0 {
		t.Fatal("registered cleanup action did not apply", out.String())
	}
	// An empty declaration set is an ordinary no-op, not an invalid destructive plan.
	options := program.options
	options.Environment = append(environment, "ENVIRONMENT_PROFILES=")
	options.ProjectData = &cleanupAssessmentFixture{payload: []byte("[]")}
	out.Reset()
	empty, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if code := empty.Run(context.Background()); code != 0 {
		t.Fatalf("empty cleanup failed %d: %s", code, out.String())
	}

}
