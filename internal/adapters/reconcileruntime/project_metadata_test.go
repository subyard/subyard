package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/projectruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

type metadataFileExecutor struct{ root string }

func (fixture metadataFileExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := exec.CommandContext(ctx, request.Command[0], request.Command[1:]...)
	command.Args[3] = strings.ReplaceAll(command.Args[3], "/srv/workspaces", fixture.root)
	command.Stdin = strings.NewReader(strings.ReplaceAll(string(request.Stdin), "/srv/workspaces", fixture.root))
	output, err := command.CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if err != nil {
		result.ExitCode = 1
	}
	return result, err
}

type metadataCopyExecutor struct{ metadataFileExecutor }

func (fixture metadataCopyExecutor) Execute(ctx context.Context, _ domain.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return fixture.Stream(ctx, domain.Context{}, request, bytes.NewReader(request.Stdin))
}

func (fixture metadataCopyExecutor) Stream(ctx context.Context, _ domain.Context, request ports.InstanceExecRequest, input io.Reader) (ports.InstanceExecResult, error) {
	arguments := []string{"-c", `umask 0002; exec "$@"`, "project-copy-fixture"}
	replace := strings.NewReplacer("/srv/workspaces", fixture.root, "/usr/local/libexec/subyard", fixture.root+"/absent-libexec", "/etc/subyard", fixture.root+"/absent-etc")
	for _, argument := range request.Command {
		arguments = append(arguments, replace.Replace(argument))
	}
	command := exec.CommandContext(ctx, "sh", arguments...)
	command.Stdin = input
	command.Env = os.Environ()
	for key, value := range request.Environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if failure := new(exec.ExitError); errors.As(err, &failure) {
		result.ExitCode = failure.ExitCode()
	}
	return result, err
}

func TestRemoteProjectCopyCreationPassesNativeMetadataGuardWithSharedParent(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(testkit.TempDir(t), "workspaces")
	if err := os.Mkdir(root, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, os.ModeSetgid|0o775); err != nil {
		t.Fatal(err)
	}
	source := testkit.TempDir(t)
	content := []byte("retain user source bytes and permissions")
	testkit.WriteFile(t, filepath.Join(source, "payload"), content, 0o664)
	record := domain.ProjectRecord{Schema: 1, IdentityVersion: 2, ProjectID: "copy", Name: "copy", HostPath: source, YardPath: "/srv/workspaces/copy/src", Mode: domain.ProjectSync, SSHHost: "yard", Target: "yard"}
	fixture := metadataCopyExecutor{metadataFileExecutor{root}}
	runner := application.ProjectActionRunner{Data: fixture, PreparedArchive: projectruntime.TarArchiver{}, Yard: domain.Context{AccessKind: domain.AccessRemote, YardName: "selected", DevUID: os.Getuid()}, Project: record}
	action := domain.AdapterRequest{Schema: 1, OperationID: "copy-fixture", Adapter: "project", Action: "sync"}
	if _, _, err := runner.Run(ctx, action, nil); err != nil {
		t.Fatal(err)
	}
	metadata, err := application.ProjectMetadata(record, "selected")
	if err != nil {
		t.Fatal(err)
	}
	plan := &projectMetadataPlan{Targets: []projectMetadataTarget{{Root: filepath.Dir(record.YardPath), Payload: string(metadata)}}}
	runtime := Runtime{Executor: fixture, Yard: domain.Context{YardName: "selected", DevUID: os.Getuid()}}
	if _, err := runtime.metadataRequest(ctx, "inspect", plan); err != nil {
		t.Fatalf("new remote copy violates native metadata admission: %v", err)
	}
	workspace := filepath.Join(root, record.ProjectID)
	payload := filepath.Join(workspace, "src", "payload")
	if info, err := os.Stat(payload); err != nil || info.Mode().Perm() != 0o664 {
		t.Fatalf("copy changed archive payload permissions: %v %v", info, err)
	}
	// A later unsafe existing root remains a collision, never permission repair.
	if err := os.Chmod(workspace, os.ModeSetgid|0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.metadataRequest(ctx, "inspect", plan); err == nil {
		t.Fatal("native guard accepted the unsafe existing root")
	} else {
		var failure *ProjectMetadataError
		if !errors.As(err, &failure) || failure.Reason != "unsafe_root" {
			t.Fatalf("unexpected native refusal: %v", err)
		}
	}
	if _, _, err := runner.Run(ctx, action, nil); err == nil {
		t.Fatal("copy accepted the existing unsafe root")
	}
	if info, err := os.Stat(workspace); err != nil || info.Mode().Perm() != 0o775 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("copy normalized an existing root: %v %v", info, err)
	}
	if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("copy changed retained user payload: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, ".subyard-meta.json")); err != nil || !bytes.Equal(got, metadata) {
		t.Fatalf("copy changed retained metadata: %q %v", got, err)
	}
}

func TestPreparedProjectMetadataRepair(t *testing.T) {
	for _, scenario := range []string{"missing", "existing", "legacy", "foreign", "foreign yard", "malformed", "symlink", "root symlink", "source symlink", "hardlink", "unsafe mode", "missing root", "missing src", "stale", "unrelated drift", "scope drift"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			workspace := filepath.Join(root, "sample")
			if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
				t.Fatal(err)
			}
			record := domain.ProjectRecord{Schema: 1, ProjectID: "sample", Name: "sample", YardPath: "/srv/workspaces/sample/src", Mode: domain.ProjectGit, Target: "yard", IdentityVersion: 2, SSHHost: "yard"}
			payload, err := application.ProjectMetadata(record, "selected")
			if err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(workspace, ".subyard-meta.json")
			sentinel := filepath.Join(workspace, "src", "source")
			testkit.WriteFile(t, sentinel, []byte("preserve source and sessions"), 0o600)
			switch scenario {
			case "existing":
				testkit.WriteFile(t, metadata, payload, 0o600)
			case "legacy":
				payload = []byte("{ \"schema\": 1, \"projectId\": \"sample\", \"name\": \"sample\", \"mode\": \"git\" }\n")
				testkit.WriteFile(t, metadata, payload, 0o600)
			case "foreign":
				testkit.WriteFile(t, metadata, []byte(`{"schema":1,"yard":"other","projectId":"sample"}`), 0o600)
			case "foreign yard":
				testkit.WriteFile(t, metadata, []byte(`{"schema":1,"yard":"other","projectId":"sample","name":"sample","mode":"git"}`), 0o600)
			case "hardlink":
				testkit.WriteFile(t, metadata, payload, 0o600)
				if err := os.Link(metadata, filepath.Join(workspace, "linked")); err != nil {
					t.Fatal(err)
				}
			case "unsafe mode":
				testkit.WriteFile(t, metadata, payload, 0o666)
			case "root symlink":
				if err := os.Rename(workspace, workspace+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(workspace+"-old", workspace); err != nil {
					t.Fatal(err)
				}
			case "source symlink":
				if err := os.Rename(filepath.Join(workspace, "src"), filepath.Join(workspace, "old")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("old", filepath.Join(workspace, "src")); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				testkit.WriteFile(t, metadata, []byte("private-payload-must-not-appear"), 0o600)
			case "symlink":
				if err := os.Symlink(sentinel, metadata); err != nil {
					t.Fatal(err)
				}
			case "missing src":
				if err := os.Rename(filepath.Join(workspace, "src"), filepath.Join(workspace, "oldsrc")); err != nil {
					t.Fatal(err)
				}
			case "missing root":
				if err := os.Rename(workspace, workspace+"-old"); err != nil {
					t.Fatal(err)
				}
			}
			runtime := Runtime{Executor: metadataFileExecutor{root}, Yard: domain.Context{YardName: "selected", DevUID: os.Getuid()}, HookPlan: &ProjectHookPlan{ownerProjects: []domain.ProjectRecord{record}, approved: hookObservation{Projects: "fixture"}}}
			observation, err := runtime.metadataRequest(context.Background(), "inspect", &projectMetadataPlan{Targets: []projectMetadataTarget{}})
			if err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan.approved.Projects = observation.Projects
			err = runtime.PrepareProjectMetadata(context.Background())
			if scenario != "missing" && scenario != "existing" && scenario != "legacy" && scenario != "stale" && scenario != "unrelated drift" && scenario != "scope drift" {
				if err == nil {
					t.Fatal("unsafe repair accepted")
				}
				var failure *ProjectMetadataError
				if !errors.As(err, &failure) || failure.ProjectID != "sample" || failure.Root != "/srv/workspaces/sample" || errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("missing bounded target diagnostic: %v", err)
				}
				reasons := map[string]string{"foreign": "conflicting_metadata", "foreign yard": "conflicting_metadata", "malformed": "invalid_metadata", "symlink": "unsafe_path", "root symlink": "unsafe_path", "source symlink": "unsafe_path", "hardlink": "unsafe_metadata", "unsafe mode": "unsafe_metadata", "missing root": "missing_root", "missing src": "missing_source"}
				if failure.Reason != reasons[scenario] || strings.Contains(err.Error(), "private-payload") || strings.Contains(err.Error(), "Traceback") {
					t.Fatalf("incorrect or unsafe refusal reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "stale" {
				testkit.WriteFile(t, metadata, payload, 0o600)
			}
			if scenario == "scope drift" {
				testkit.WriteFile(t, filepath.Join(root, "unregistered"), []byte("changed"), 0o600)
			}
			if scenario == "unrelated drift" {
				testkit.WriteFile(t, filepath.Join(workspace, "session"), []byte("changed"), 0o600)
			}
			err = runtime.repairProjectMetadata(context.Background())
			if scenario == "stale" || scenario == "unrelated drift" || scenario == "scope drift" {
				if err == nil {
					t.Fatal("stale metadata accepted")
				}
				var failure *ProjectMetadataError
				if !errors.Is(err, domain.ErrPlanStale) || !errors.As(err, &failure) {
					t.Fatalf("lost stale classification: %v", err)
				}
				if scenario != "scope drift" && (failure.Reason != "stale_inputs" || failure.ProjectID != "sample" || failure.Root != "/srv/workspaces/sample") {
					t.Fatalf("lost stale project diagnostic: %v", err)
				}
				if scenario == "scope drift" && (failure.Reason != "scope_changed" || failure.ProjectID != "" || failure.Root != "/srv/workspaces") {
					t.Fatalf("scope drift attributed to wrong project: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(metadata)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(metadata)
			if err != nil || string(got) != string(payload) {
				t.Fatalf("metadata=%s error=%v", got, err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("metadata mode=%o", info.Mode().Perm())
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve source and sessions" {
				t.Fatal("source changed")
			}
			if err := runtime.PrepareProjectMetadata(context.Background()); err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan.metadataCompleted = false
			if err := runtime.repairProjectMetadata(context.Background()); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(metadata)
			if !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) {
				t.Fatal("repeat init rewrote metadata")
			}
			var value map[string]any
			if err := json.Unmarshal(got, &value); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Execute the actual native observer and publisher; the installed hook checks
// registration at its invocation boundary, rather than assuming it was repaired.
type metadataHookExecutor struct {
	metadataFileExecutor
	calls           int
	expectedBinding string
}

func (fixture *metadataHookExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if request.Command[0] == "python3" {
		return fixture.metadataFileExecutor.Exec(ctx, project, instance, request)
	}
	if request.Command[0] == "bash" {
		script := strings.NewReplacer("/srv/workspaces", fixture.root, "/usr/local/libexec/subyard", fixture.root+"/absent-libexec", "/etc/subyard", fixture.root+"/absent-etc").Replace(string(request.Stdin))
		command := exec.CommandContext(ctx, "bash", "-s", "--", "--observe")
		command.Stdin = strings.NewReader(script)
		output, err := command.CombinedOutput()
		return ports.InstanceExecResult{Stdout: output}, err
	}
	if request.Command[1] == "-eu" {
		return ports.InstanceExecResult{}, nil
	}
	fixture.calls++
	if fixture.expectedBinding != "" {
		var scope struct {
			Bindings map[string]string `json:"bindings"`
		}
		if err := json.Unmarshal([]byte(request.Environment["SUBYARD_PROJECT_HOOK_SCOPE"]), &scope); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if scope.Bindings["/usr/local/libexec/subyard/projects-changed.d/synthetic"] != fixture.expectedBinding {
			return ports.InstanceExecResult{}, errors.New("native hook catalog binding is stale")
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "sample", ".subyard-meta.json")); err != nil {
		return ports.InstanceExecResult{}, err
	}
	return ports.InstanceExecResult{}, nil
}
func TestInitRepairsOwnerMetadataBeforeHooksAndPreservesUnregisteredDirectories(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"sample", "unregistered"} {
		if err := os.MkdirAll(filepath.Join(root, name, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executor := &metadataHookExecutor{metadataFileExecutor: metadataFileExecutor{root}}
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "running"}}}
	runtime := Runtime{RepositoryRoot: "../../..", Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{}, Incus: incus, Executor: executor, Yard: domain.Context{YardName: "selected", DevUID: os.Getuid()}}
	var err error
	runtime.HookPlan, err = runtime.PrepareProjectHooks(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.HookPlan.CaptureOwnerProjects([]domain.ProjectRecord{{Schema: 1, ProjectID: "sample", Name: "Sample", YardPath: "/srv/workspaces/sample/src", Mode: domain.ProjectSync, SSHHost: "yard"}})
	if err := runtime.PrepareProjectMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	if step := runtime.HookPlan.MetadataOperationStep("yard"); step == nil || step.Decision != domain.StepApply {
		t.Fatalf("metadata approval=%+v", step)
	}
	if err := runtime.RunProjectHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 {
		t.Fatalf("hook calls=%d", executor.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "unregistered", ".subyard-meta.json")); !os.IsNotExist(err) {
		t.Fatal("unregistered project adopted")
	}
	if err := runtime.RunProjectHooks(context.Background()); err != nil || executor.calls != 1 {
		t.Fatalf("repeat calls=%d error=%v", executor.calls, err)
	}
}

func TestMetadataRepairBindsProjectedNativeCatalogBeforeFirstHook(t *testing.T) {
	for _, scenario := range []string{"repair", "native drift", "unsupported projection"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			workspace := filepath.Join(root, "workspaces")
			if err := os.MkdirAll(filepath.Join(workspace, "sample", "src"), 0o700); err != nil {
				t.Fatal(err)
			}
			nativeCatalog := filepath.Join(root, "catalog")
			testkit.WriteFile(t, nativeCatalog, []byte("captured"), 0o600)
			// EOF denotes the ordinary observer, while the optional projection supplies
			// prospective metadata without changing the independently observed catalog.
			handler := `#!/usr/bin/python3
import json,os,sys
raw=sys.stdin.read()
projection=json.loads(raw) if raw else None
present=os.path.exists(os.environ['META']) or bool(projection and projection['missingMetadata'] and os.environ['SUPPORTED']=='1')
binding=os.environ['NEW'] if present else os.environ['OLD']
if open(os.environ['CATALOG']).read()!='captured': binding=os.environ['DRIFT']
print(json.dumps(dict(state='current',actual=os.environ['NEW'],desired=os.environ['NEW'],hook_binding=binding)))
`
			runtime := profileRuntimeFixtureAt(t, root, handler)
			supported := "1"
			if scenario == "unsupported projection" {
				supported = "0"
			}
			runtime.Environment = []string{"META=" + filepath.Join(workspace, "sample", ".subyard-meta.json"), "CATALOG=" + nativeCatalog, "SUPPORTED=" + supported, "OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest, "DRIFT=" + strings.Repeat("a", 64)}
			runtime.Integrations = []profile.IntegrationDefinition{}
			runtime.Yard.DevUID = os.Getuid()
			runtime.Yard.YardName = "selected"
			hook := "/usr/local/libexec/subyard/projects-changed.d/synthetic"
			testkit.WriteFile(t, filepath.Join(root, "config", "profiles", "synthetic", "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":"runtime.sh","projects_changed_hooks":["`+hook+`"]}}`), 0o600)
			source, err := os.ReadFile("../../../config/projects-changed.sh")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "config", "projects-changed.sh"), source, 0o600)
			incus := runtime.Incus.(*testkit.Incus)
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "running"}}
			executor := &metadataHookExecutor{metadataFileExecutor: metadataFileExecutor{workspace}}
			executor.expectedBinding = testRuntimeNewDigest
			runtime.Executor = executor
			runtime.RuntimePlan, err = runtime.PrepareProfileRuntimes(context.Background(), false, false)
			if err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan, err = runtime.PrepareProjectHooks(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan.CaptureOwnerProjects([]domain.ProjectRecord{{Schema: 1, ProjectID: "sample", Name: "Sample", YardPath: "/srv/workspaces/sample/src", Mode: domain.ProjectSync, SSHHost: "yard"}})
			if err := runtime.PrepareProjectMetadata(context.Background()); err != nil {
				t.Fatal(err)
			}
			if scenario == "native drift" {
				testkit.WriteFile(t, nativeCatalog, []byte("drift"), 0o600)
			}
			err = runtime.RunProjectHooks(context.Background())
			if scenario != "repair" {
				if !errors.Is(err, domain.ErrPlanStale) || executor.calls != 0 {
					t.Fatalf("unapproved catalog reached hook: calls=%d err=%v", executor.calls, err)
				}
				return
			}
			if err != nil || executor.calls != 1 {
				t.Fatalf("first init failed: calls=%d err=%v", executor.calls, err)
			}
			if runtime.HookPlan.hookBindings[hook] != testRuntimeNewDigest {
				t.Fatal("native hook received pre-publication catalog binding")
			}
			if err := runtime.CheckProfileRuntimePlan(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type partialMetadataExecutor struct {
	metadataFileExecutor
	interrupt bool
}

func (fixture *partialMetadataExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	var mode struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(request.Stdin, &mode); err != nil {
		return ports.InstanceExecResult{}, err
	}
	if mode.Mode == "apply" && fixture.interrupt {
		fixture.interrupt = false
		prefix := "import os\n_original_link=os.link\ndef interrupt(*args,**kwargs):\n _original_link(*args,**kwargs)\n os.rename('/srv/workspaces/second','/srv/workspaces/second-old')\nos.link=interrupt\n"
		request.Command = append([]string(nil), request.Command...)
		request.Command[3] = prefix + request.Command[3]
	}
	return fixture.metadataFileExecutor.Exec(ctx, project, instance, request)
}
func TestProjectMetadataPartialPublicationRecoversWithFreshPlan(t *testing.T) {
	root := testkit.TempDir(t)
	records := []domain.ProjectRecord{}
	for _, name := range []string{"first", "second"} {
		if err := os.MkdirAll(filepath.Join(root, name, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, name, "src", "session"), []byte("preserved"), 0o600)
		records = append(records, domain.ProjectRecord{Schema: 1, ProjectID: name, Name: name, YardPath: "/srv/workspaces/" + name + "/src", Mode: domain.ProjectSync, SSHHost: "yard"})
	}
	executor := &partialMetadataExecutor{metadataFileExecutor: metadataFileExecutor{root}, interrupt: true}
	runtime := Runtime{Executor: executor, Yard: domain.Context{YardName: "selected", DevUID: os.Getuid()}, HookPlan: &ProjectHookPlan{ownerProjects: records}}
	prepare := func() {
		t.Helper()
		observed, err := runtime.metadataRequest(context.Background(), "inspect", &projectMetadataPlan{Targets: []projectMetadataTarget{}})
		if err != nil {
			t.Fatal(err)
		}
		runtime.HookPlan.approved.Projects = observed.Projects
		if err := runtime.PrepareProjectMetadata(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	prepare()
	if err := runtime.repairProjectMetadata(context.Background()); err == nil || runtime.HookPlan.metadataCompleted {
		t.Fatal("partial repair reported complete")
	} else {
		var failure *ProjectMetadataError
		if !errors.As(err, &failure) || !errors.Is(err, domain.ErrPlanStale) || failure.ProjectID != "second" || failure.Root != "/srv/workspaces/second" || failure.Reason != "stale_inputs" {
			t.Fatalf("partial repair lost failing project and stale classification: %v", err)
		}
	}
	firstMeta := filepath.Join(root, "first", ".subyard-meta.json")
	first, err := os.Stat(firstMeta)
	if err != nil {
		t.Fatal("verified earlier publication was lost")
	}
	if _, err := os.Stat(filepath.Join(root, "second-old", ".subyard-meta.json")); !os.IsNotExist(err) {
		t.Fatal("changed later root was written")
	}
	if err := os.Rename(filepath.Join(root, "second-old"), filepath.Join(root, "second")); err != nil {
		t.Fatal(err)
	}
	prepare()
	if err := runtime.repairProjectMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(firstMeta)
	if !os.SameFile(first, after) || !first.ModTime().Equal(after.ModTime()) {
		t.Fatal("retry overwrote earlier publication")
	}
	for _, name := range []string{"first", "second"} {
		if payload, err := os.ReadFile(filepath.Join(root, name, "src", "session")); err != nil || string(payload) != "preserved" {
			t.Fatal("partial repair changed session/source data")
		}
	}
}

func TestProjectMetadataFailureDoesNotExposeUntrustedOutput(t *testing.T) {
	runtime := Runtime{Executor: metadataFileExecutor{filepath.Join(testkit.TempDir(t), "absent")}}
	_, err := runtime.metadataRequest(context.Background(), "inspect", &projectMetadataPlan{})
	var ioFailure *ProjectMetadataError
	if !errors.As(err, &ioFailure) || ioFailure.Reason != "io_failure" || errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("lost native IO classification: %v", err)
	}
	plan := &projectMetadataPlan{Targets: []projectMetadataTarget{{Root: "/srv/workspaces/sample"}}}
	for _, output := range []string{
		"Traceback private transport and payload details",
		`{"error":{"reason":"private-payload","target":0}}`,
		`{"error":{"reason":"missing_root","target":99,"root":"private-path"}}`,
		strings.Repeat("private-output", 500),
	} {
		err := metadataFailure(plan, []byte(output))
		var failure *ProjectMetadataError
		if !errors.As(err, &failure) || failure.Reason != "io_failure" || errors.Is(err, domain.ErrPlanStale) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "Traceback") {
			t.Fatalf("unsafe transport diagnostic: %v", err)
		}
	}
}

func TestProjectMetadataNativeExecutorFailure(t *testing.T) {
	const private = "private transport details"
	for _, scenario := range []struct {
		name   string
		result ports.InstanceExecResult
		want   string
		stale  bool
	}{
		{"native refusal", ports.InstanceExecResult{ExitCode: 1, Stdout: []byte(`{"error":{"reason":"unsafe_root","target":0,"stale":false}}`)}, "unsafe_root", false},
		{"native stale", ports.InstanceExecResult{ExitCode: 1, Stdout: []byte(`{"error":{"reason":"stale_inputs","target":0,"stale":true}}`)}, "stale_inputs", true},
		{"transport", ports.InstanceExecResult{}, "io_failure", false},
		{"partial transport output", ports.InstanceExecResult{Stdout: []byte(`{"error":{"reason":"unsafe_root","target":0}}`)}, "io_failure", false},
		{"unstructured exit", ports.InstanceExecResult{ExitCode: 1, Stdout: []byte(private)}, "io_failure", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			scenario.result.Stderr = []byte(private)
			executor := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: scenario.result, Err: errors.New(private)}}}
			runtime := Runtime{Executor: executor}
			_, err := runtime.metadataRequest(context.Background(), "inspect", &projectMetadataPlan{Targets: []projectMetadataTarget{{Root: "/srv/workspaces/sample"}}})
			var failure *ProjectMetadataError
			if !errors.As(err, &failure) || failure.Reason != scenario.want || errors.Is(err, domain.ErrPlanStale) != scenario.stale || strings.Contains(err.Error(), private) {
				t.Fatalf("incorrect native failure: %v", err)
			}
			if scenario.want != "io_failure" && failure.ProjectID != "sample" {
				t.Fatalf("native target lost: %v", err)
			}
		})
	}
}

func TestProjectMetadataPublicationUsesCapturedOwnerFacts(t *testing.T) {
	for _, scenario := range []struct {
		name, mode, projectMode, metadata string
		owner, group, permissions         int
		wantUser                          uint32
	}{
		{"inspect", "inspect", "bind", "null", 0, 1000, 0o2755, 1000},
		{"legacy publication", "apply", "bind", "null", 0, 1000, 0o2755, 0},
		{"group writable", "apply", "bind", "null", 0, 1000, 0o2775, 1000},
		{"developer owned", "apply", "bind", "null", 1000, 1000, 0o755, 1000},
		{"non bind", "apply", "sync", "null", 0, 1000, 0o2755, 1000},
		{"foreign group", "apply", "bind", "null", 0, 1001, 0o2755, 1000},
		{"world writable", "apply", "bind", "null", 0, 1000, 0o2757, 1000},
		{"existing metadata", "apply", "bind", "[]", 0, 1000, 0o2755, 1000},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			expected, err := json.Marshal([]any{map[string]any{"root": []int{0o40000 | scenario.permissions, scenario.owner, scenario.group}, "metadata": json.RawMessage(scenario.metadata)}})
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]string{"mode": scenario.projectMode})
			executor := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{Stdout: []byte(`{"expected":[],"projects":"` + strings.Repeat("a", 64) + `"}`)}}}}
			runtime := Runtime{Executor: executor, Yard: domain.Context{DevUID: 1000}}
			_, err = runtime.metadataRequest(context.Background(), scenario.mode, &projectMetadataPlan{Targets: []projectMetadataTarget{{Root: "/srv/workspaces/sample", Payload: string(payload)}}, projectMetadataObservation: projectMetadataObservation{Expected: expected}})
			if err != nil {
				t.Fatal(err)
			}
			call := executor.ExecCalls[0].Request
			var request struct {
				UID int `json:"uid"`
			}
			if err := json.Unmarshal(call.Stdin, &request); err != nil || call.User != scenario.wantUser || call.Group != 1000 || request.UID != 1000 {
				t.Fatalf("publication identity=%d/%d developer=%d err=%v", call.User, call.Group, request.UID, err)
			}
		})
	}
}

func TestProjectMetadataFailureBoundsLegacyIdentity(t *testing.T) {
	plan := &projectMetadataPlan{Targets: []projectMetadataTarget{{Root: "/srv/workspaces/" + strings.Repeat("a", 10000)}}}
	err := metadataFailure(plan, []byte(`{"error":{"reason":"io_failure","target":0}}`))
	var failure *ProjectMetadataError
	if !errors.As(err, &failure) || len(failure.ProjectID) > 255 || len(failure.Root) > 271 || len(err.Error()) > 600 {
		t.Fatal("legacy identity diagnostic is unbounded")
	}
}

func TestProjectMetadataDispatcherParityWithDirectorySymlinks(t *testing.T) {
	for _, scenario := range []string{"ready", "missing", "target symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root, outside := testkit.TempDir(t), testkit.TempDir(t)
			workspace := filepath.Join(root, "sample")
			if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(outside, "retained"), []byte("independent"), 0o600)
			link := filepath.Join(root, "unregistered")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			record := domain.ProjectRecord{Schema: 1, ProjectID: "sample", Name: "Sample", YardPath: "/srv/workspaces/sample/src", Mode: domain.ProjectSync, SSHHost: "yard"}
			payload, err := application.ProjectMetadata(record, "selected")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "ready" {
				testkit.WriteFile(t, filepath.Join(workspace, ".subyard-meta.json"), payload, 0o600)
			}
			if scenario == "target symlink" {
				if err := os.Rename(workspace, workspace+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(workspace+"-original", workspace); err != nil {
					t.Fatal(err)
				}
			}
			executor := &metadataHookExecutor{metadataFileExecutor: metadataFileExecutor{root}}
			runtime := Runtime{RepositoryRoot: "../../..", Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{}, Executor: executor, Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "running"}}}, Yard: domain.Context{YardName: "selected", DevUID: os.Getuid()}}
			runtime.HookPlan, err = runtime.PrepareProjectHooks(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan.CaptureOwnerProjects([]domain.ProjectRecord{record})
			err = runtime.PrepareProjectMetadata(context.Background())
			if scenario == "target symlink" {
				var failure *ProjectMetadataError
				if !errors.As(err, &failure) || failure.Reason != "unsafe_path" || executor.calls != 0 {
					t.Fatalf("symlinked owner target accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			decision := domain.StepApply
			if scenario == "ready" {
				decision = domain.StepSkip
			}
			if step := runtime.HookPlan.MetadataOperationStep("yard"); step == nil || step.Decision != decision {
				t.Fatalf("metadata decision=%+v", step)
			}
			// Unregistered symlink destinations are outside both captured scopes.
			testkit.WriteFile(t, filepath.Join(outside, "retained"), []byte("independent change"), 0o600)
			if err := runtime.RunProjectHooks(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(workspace, ".subyard-meta.json")); err != nil || string(got) != string(payload) {
				t.Fatalf("metadata repair failed: %v", err)
			}
			if got, err := os.Readlink(link); err != nil || got != outside {
				t.Fatal("unregistered symlink changed")
			}
			if _, err := os.Stat(filepath.Join(outside, ".subyard-meta.json")); !os.IsNotExist(err) {
				t.Fatal("unregistered symlink destination adopted")
			}
		})
	}
}
