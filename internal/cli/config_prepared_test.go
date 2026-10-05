package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func configExactProgram(t *testing.T, root string, environment []string, incus *testkit.Incus, applier *recordingConfigApplier) *CLI {
	t.Helper()
	options := Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root, Stdout: io.Discard, Stderr: io.Discard, Config: applier}
	if incus != nil {
		options.Incus = incus
		options.Executor = incus
	}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return program
}
func configExactPlan(t *testing.T, program *CLI, loaded config.Loaded, arguments []string) *preparedCommand {
	t.Helper()
	definition, ok := program.manifest.Lookup("config")
	if !ok {
		t.Fatal("config metadata missing")
	}
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close() })
	prepared.Plan.Confirmed = true
	return prepared
}

func TestConfigExactScalarRetainsNativeCASAndRejectsNoOpExpansion(t *testing.T) {
	for _, kind := range []string{"converged", "changed baseline", "no-op expansion"} {
		t.Run(kind, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			target := filepath.Join(configHome, "config.env")
			baseline := "SSH_PORT='2290'\n"
			if kind == "no-op expansion" {
				baseline = "SSH_PORT='2390'\n"
			}
			writeConfigCommandFile(t, target, baseline)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			prepared := configExactPlan(t, configExactProgram(t, root, environment, nil, nil), loaded, []string{"set", "SSH_PORT", "2390", "--scope", "host"})
			if len(prepared.Plan.Steps) != 1 || prepared.Plan.Steps[0].Desired == "2390" {
				t.Fatal("scalar plan lacks safe native fingerprint")
			}
			if kind == "converged" {
				writeConfigCommandFile(t, target, "SSH_PORT='2390'\n")
			} else {
				writeConfigCommandFile(t, target, "SSH_PORT='2490'\n")
			}
			var output bytes.Buffer
			_, err := prepared.Execute(context.Background(), nil, &output)
			if kind == "converged" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("native scalar guard did not reject drift: %v", err)
			}
			current, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "converged" && string(current) != "SSH_PORT='2490'\n" {
				t.Fatal("stale scalar plan overwrote concurrent input")
			}
		})
	}
}

func TestConfigExactApplyRechecksOriginalNoOpBeforeAnyWrite(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running"}}}
	appendHashSteps(t, fake, loaded)
	appendMismatchedHashSteps(t, fake, loaded, "0")
	applier := &recordingConfigApplier{}
	prepared := configExactPlan(t, configExactProgram(t, root, environment, fake, applier), loaded, []string{"apply"})
	if prepared.Plan.Assessment.Changed {
		t.Fatal("expected original native no-op")
	}
	if _, err := prepared.Execute(context.Background(), nil, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("new materialized work received old no-op authority: %v", err)
	}
	if len(applier.yards) != 0 {
		t.Fatal("original no-op wrote materialized configuration")
	}
}

func TestConfigExactSyncRejectsUnmanagedDriftBeforePublishingIdentity(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	environment = append(environment, "SUBYARD_HOST_ID=owner-a")
	source := filepath.Join(testkit.TempDir(t), "source")
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"), "{\"schemaVersion\":1}\n")
	writeConfigCommandFile(t, filepath.Join(source, "hosts", "owner-a", "config.env"), "SSH_PORT=2390\n")
	runConfigSyncGit(t, source, "init", "-q")
	commitConfigSource(t, source, "candidate")
	loaded := loadConfigCommandContext(t, root, environment, "default")
	prepared := configExactPlan(t, configExactProgram(t, root, environment, nil, nil), loaded, []string{"sync", source, "--adopt"})
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"), "SSH_PORT=2490\n")
	if _, err := prepared.Execute(context.Background(), nil, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("unmanaged drift did not invalidate publication: %v", err)
	}
	if _, err := os.Stat(configsync.HostIDPath(configHome)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale sync published owner identity: %v", err)
	}
	if _, err := os.Stat(configsync.ManifestPath(configHome)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale sync published manifest: %v", err)
	}
}

func TestConfigExactEligibilityExcludesProtectedDraftsAndSensitiveSettings(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	for _, arguments := range [][]string{{"import", "AGENT_codex_CONFIG", "secret-input", "--scope", "host"}, {"edit", "AGENT_codex_CONFIG", "--scope", "host"}, {"set", "UNKNOWN_SECRET", "secret-input", "--scope", "host"}, {"sync", "status"}, {"sync", "--check"}} {
		if configExactInvocation(loaded, arguments) {
			t.Fatalf("protected/read-only invocation entered exact mutation boundary: %s", strings.Join(arguments[:1], " "))
		}
	}
	if !configExactInvocation(loaded, []string{"set", "SSH_PORT", "2390", "--scope", "host"}) || !configExactInvocation(loaded, []string{"sync", "source", "--apply"}) {
		t.Fatal("safe native mutation missing exact transport")
	}
}

func TestConfigExactRPCUsesRetainedNativeCaptureAndSafeDiagnostics(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	defer handler.closePlans()
	value, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "config-native", Params: json.RawMessage(`{"command":"config","arguments":["set","SSH_PORT","2390","--scope","host"],"exact":true,"stepSchema":1}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := value.(exactOperationPlan)
	if len(plan.Plan.Steps) != 1 || plan.Plan.Steps[0].Desired == "2390" || len(plan.Digest) != 64 {
		t.Fatal("RPC config plan lacks bounded native capture")
	}
	before, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatal("planning published configuration before consent")
	}
	params, _ := json.Marshal(map[string]any{"confirmed": true, "digest": plan.Digest})
	if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "config-native", Params: params}, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || string(after) != "SSH_PORT='2390'\n" {
		t.Fatalf("native config execution did not publish approved assignment: %v", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "config set: updated") {
		t.Fatal("configuration diagnostics corrupted framed stdout")
	}
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "config-native", Params: params}, nil)
	var fault *rpc.Error
	if !errors.As(err, &fault) || fault.Code != "plan_not_found" {
		t.Fatalf("configuration plan remained reusable: %v", err)
	}
}

// The owner contract is mocked here; its native execution is covered above.
// The controller must send only one framed exact plan/execute session.
func writeConfigExactSSHMock(t *testing.T, directory, logPath string) {
	t.Helper()
	script := filepath.Join(directory, "config-rpc.py")
	writeConfigCommandFile(t, script, `import datetime,json,os,struct,sys
log=os.environ['SUBYARD_TEST_SSH_LOG']
def send(req,result):
 data=json.dumps(dict(version=1,type='response',id=req['id'],operationId=req.get('operationId',''),result=result)).encode()
 sys.stdout.buffer.write(struct.pack('>I',len(data))+data);sys.stdout.buffer.flush()
while True:
 header=sys.stdin.buffer.read(4)
 if not header:break
 req=json.loads(sys.stdin.buffer.read(struct.unpack('>I',header)[0]))
 with open(log,'a') as out:out.write(json.dumps(req)+'\n')
 if req['method']=='rpc.negotiate':send(req,dict(capabilities=['operation-exact-plan-v1','operation-steps-v1']))
 elif req['method']=='operation.plan':
  plan=dict(operationId=req['operationId'],command='config',effect='mutate',target='local-owner',confirmation='never',confirmed=True,steps=[dict(id='config.verify',target='owner native configuration',observed='converged',desired='converged',decision='skip',verify='native configuration recheck')])
  send(req,dict(schema=1,stepSchema=1,digest='a'*64,expiresAt=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=2)).isoformat(),plan=plan))
 elif req['method']=='operation.execute':send(req,dict(plan=plan,result=dict(schema=1,operationId=req['operationId'],status='ok')))
 else:sys.exit(6)
`, 0o600)
	writeConfigCommandFile(t, filepath.Join(directory, "ssh"), "#!/bin/sh\n"+trustedSSHMock(t)+"\nprintf '%s\\n' \"$@\" >\"$SUBYARD_TEST_SSH_LOG\"\ncase \"$*\" in *rpc*) exec python3 '"+script+"';; esac\n", 0o700)
}

func TestConfigExactSyncApplyCapturesCandidateConsumersBeforePublication(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	environment = append(environment, "SUBYARD_HOST_ID=owner-a")
	source := filepath.Join(testkit.TempDir(t), "source")
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"), "{\"schemaVersion\":1}\n")
	writeConfigCommandFile(t, filepath.Join(source, "hosts", "owner-a", "yards", "demo", "config.env"), "SSH_PORT=2391\n")
	writeConfigCommandFile(t, filepath.Join(source, "hosts", "owner-a", "overrides", "agents", "codex", "config.toml"), "model = \"approved-candidate\"\n")
	runConfigSyncGit(t, source, "init", "-q")
	commitConfigSource(t, source, "candidate")
	loaded := loadConfigCommandContext(t, root, environment, "default")
	native, err := configsync.BuildPlan(configsync.Options{SourceRoot: source, ConfigHome: configHome, RepositoryRoot: root, OperatorHome: home, Environment: environmentMap(environment), FileSettings: config.SyncableFileMappings(loaded), Adopt: true})
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := native.CandidateConfigs()
	if err != nil {
		t.Fatal(err)
	}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running"}}}
	appendMismatchedHashSteps(t, fake, contexts[0], "0")
	applier := &recordingConfigApplier{}
	prepared := configExactPlan(t, configExactProgram(t, root, environment, fake, applier), loaded, []string{"sync", source, "--adopt", "--apply"})
	consumer, named := false, false
	for _, step := range prepared.Plan.Steps {
		if step.ID == "config.apply.default" {
			consumer = true
			if step.Decision != domain.StepConditional || len(step.DependsOn) != 1 || step.DependsOn[0] != "config.sync.manifest" {
				t.Fatal("consumer missing bounded conditional source dependency")
			}
		}
		if step.ID == "config.apply.demo" {
			named = true
			if step.Decision != domain.StepSkip {
				t.Fatal("absent named candidate unexpectedly requires materialization")
			}
		}
	}
	if !consumer || !named {
		t.Fatal("candidate consumer target set missing from initial exact plan")
	}
	if _, err := os.Stat(filepath.Join(configHome, config.GitSettingsRelativePath, "yards", "demo", "config.env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planning published candidate registration")
	}
	writeConfigCommandFile(t, filepath.Join(source, "hosts", "owner-a", "yards", "another", "config.env"), "SSH_PORT=2392\n")
	commitConfigSource(t, source, "unapproved target")
	if _, err := prepared.Execute(context.Background(), nil, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("candidate target expansion was not stale: %v", err)
	}
	if len(applier.yards) != 0 {
		t.Fatal("unapproved candidate target expansion materialized settings")
	}
	if _, err := os.Stat(configsync.HostIDPath(configHome)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale candidate published owner identity")
	}
}
