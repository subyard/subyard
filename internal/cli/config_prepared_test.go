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
	authorizePreparedFixture(prepared)
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

func TestConfigExactScalarRejectsInvalidProspectiveScopeBeforeConsent(t *testing.T) {
	for _, action := range []string{"set", "unset"} {
		t.Run(action, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			target := filepath.Join(configHome, "config.env")
			initial := "HOST_BASE=/srv/retained\nRESTRICTED_DISK_PATHS=/srv/retained\n"
			writeConfigCommandFile(t, target, initial)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			arguments := []string{action, "HOST_BASE"}
			if action == "set" {
				arguments = append(arguments, "/srv/invalid")
			}
			arguments = append(arguments, "--scope", "host")
			program := configExactProgram(t, root, environment, nil, nil)
			definition, _ := program.manifest.Lookup("config")
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments})
			if prepared != nil {
				_ = prepared.Close()
			}
			var diagnostic configProspectiveError
			if !errors.As(err, &diagnostic) || !strings.Contains(diagnostic.Unwrap().Error(), "HOST_BASE must equal") {
				t.Fatalf("invalid prospective scope accepted: %v", err)
			}
			content, err := os.ReadFile(target)
			if err != nil || string(content) != initial {
				t.Fatalf("invalid draft persisted: %s, %v", content, err)
			}
		})
	}
}

func TestConfigExactScalarRejectsChangedDependencyBeforeWrite(t *testing.T) {
	for _, change := range []string{"bytes", "same bytes inode", "mode", "new registration", "invalid neighbor"} {
		t.Run(change, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			dependency := filepath.Join(configHome, "overrides/shared/config.env")
			writeConfigCommandFile(t, dependency, "E2E_VM_SLOT_COUNT=2\n")
			loaded := loadConfigCommandContext(t, root, environment, "default")
			prepared := configExactPlan(t, configExactProgram(t, root, environment, nil, nil), loaded, []string{"set", "SSH_PORT", "2390", "--scope", "host"})
			switch change {
			case "bytes":
				writeConfigCommandFile(t, dependency, "E2E_VM_SLOT_COUNT=3\n")
			case "same bytes inode":
				replacement := filepath.Join(configHome, "replacement.env")
				writeConfigCommandFile(t, replacement, "E2E_VM_SLOT_COUNT=2\n")
				if err := os.Rename(replacement, dependency); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(dependency, 0o640); err != nil {
					t.Fatal(err)
				}
			case "new registration":
				writeConfigCommandFile(t, filepath.Join(configHome, "yards/new/config.env"), "SSH_PORT=2391\n")
			case "invalid neighbor":
				writeConfigCommandFile(t, filepath.Join(configHome, "yards/new/config.env"), "YARD_PROFILES=one\nENVIRONMENT_PROFILES=two\n")
			}
			if _, err := prepared.Execute(context.Background(), nil, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("dependency drift accepted: %v", err)
			}
			content, err := os.ReadFile(filepath.Join(configHome, "config.env"))
			if err != nil || len(content) != 0 {
				t.Fatalf("stale plan changed target: %s, %v", content, err)
			}
		})
	}
}

func TestConfigExactSharedScalarValidatesRegisteredNeighbor(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, filepath.Join(configHome, "yards/neighbor/config.env"), "HOST_BASE=/srv/a\nRESTRICTED_DISK_PATHS=/srv/b\n")
	loaded := loadConfigCommandContext(t, root, environment, "default")
	program := configExactProgram(t, root, environment, nil, nil)
	definition, _ := program.manifest.Lookup("config")
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{"set", "E2E_VM_SLOT_COUNT", "3", "--scope", "shared"}})
	if prepared != nil {
		_ = prepared.Close()
	}
	var diagnostic configProspectiveError
	if !errors.As(err, &diagnostic) || !strings.Contains(diagnostic.Unwrap().Error(), "neighbor") {
		t.Fatalf("invalid registered neighbor accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configHome, "overrides/shared/config.env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid draft persisted: %v", err)
	}
}

func TestConfigExactUnsetPreservesLegacyRegistrationFallback(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	legacy := filepath.Join(filepath.Dir(loaded.Context.Paths.ConfigDir), "private/yards/named.env")
	writeConfigCommandFile(t, legacy, "SSH_PORT=2290\n")
	target := filepath.Join(configHome, "yards/named/config.env")
	writeConfigCommandFile(t, target, "SSH_PORT=2390\n")
	loaded = loadConfigCommandContext(t, root, environment, "named")
	prepared := configExactPlan(t, configExactProgram(t, root, environment, nil, nil), loaded, []string{"unset", "SSH_PORT", "--scope", "yard"})
	if _, err := prepared.Execute(context.Background(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unset target retained: %v", err)
	}
	if actual := loadConfigCommandContext(t, root, environment, "named").Context.SSHPort; actual != 2290 {
		t.Fatalf("fallback port = %d", actual)
	}
}

func TestConfigExactScalarRewritesLegacyProfileAliasWithoutConflict(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "yards/named/config.env")
	writeConfigCommandFile(t, target, "# retained\nYARD_PROFILES=alpha\nSSH_PORT=2290\n", 0o640)
	loaded := loadConfigCommandContext(t, root, environment, "named")
	prepared := configExactPlan(t, configExactProgram(t, root, environment, nil, nil), loaded, []string{"set", "ENVIRONMENT_PROFILES", "beta", "--scope", "yard"})
	if _, err := prepared.Execute(context.Background(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "# retained\nENVIRONMENT_PROFILES='beta'\nSSH_PORT=2290\n" {
		t.Fatalf("canonical rewrite: %s, %v", content, err)
	}
	if got := loadConfigCommandContext(t, root, environment, "named").Environment["ENVIRONMENT_PROFILES"]; got != "beta" {
		t.Fatalf("loaded profiles = %s", got)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode: %v, %v", info, err)
	}
}

func TestConfigExactScalarValidatesProspectiveProfileCompatibility(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	writeConfigCommandFile(t, filepath.Join(root, "config/subyard.env"), "DEV_UID=1000\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nSHIFT_MODE=shift\nSTORAGE_PATH=$SUBYARD_HOME/storage\nHOST_BASE=/srv/subyard\nRESTRICTED_DISK_PATHS=/srv/subyard\n")
	writeConfigCommandFile(t, filepath.Join(root, "config/yards/profiles/isolated.env"), "YARD_KIND=vm\nREQUIRED_YARD_KIND=vm\nEXCLUSIVE_ENVIRONMENT_PROFILE=service\nENVIRONMENT_PROFILES=service\nALLOWS_CODING_TOOLS=false\n")
	writeConfigCommandFile(t, filepath.Join(root, "config/profiles/service/profile.conf"), "PROVISION_SCOPE=dedicated\n")
	target := filepath.Join(configHome, "yards/named/config.env")
	writeConfigCommandFile(t, target, "YARD_TEMPLATE=isolated\n")
	loaded := loadConfigCommandContext(t, root, environment, "named")
	program := configExactProgram(t, root, environment, nil, nil)
	valid := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: program, Loaded: loaded}
	err := valid.prepareConfigScalar(context.Background(), "set", []string{"SSH_PORT", "2390", "--scope", "yard"})
	_ = valid.Close()
	if err != nil {
		t.Fatalf("valid profile-compatible draft rejected: %v", err)
	}
	for _, arguments := range [][]string{{"ENVIRONMENT_PROFILES", "another"}, {"YARD_KIND", "container"}} {
		prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: program, Loaded: loaded}
		err := prepared.prepareConfigScalar(context.Background(), "set", append(arguments, "--scope", "yard"))
		_ = prepared.Close()
		if err == nil {
			t.Fatalf("incompatible draft accepted: %v", arguments)
		}
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "YARD_TEMPLATE=isolated\n" {
		t.Fatalf("invalid draft persisted: %s, %v", content, err)
	}
}

func TestConfigExactScalarCreatesAbsentConfigurationRoot(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	if err := os.Remove(filepath.Join(configHome, "config.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configHome); err != nil {
		t.Fatal(err)
	}
	prepared := configExactPlan(t, configExactProgram(t, root, environment, nil, nil), loaded, []string{"set", "SSH_PORT", "2390", "--scope", "host"})
	if _, err := prepared.Execute(context.Background(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if actual := loadConfigCommandContext(t, root, environment, "default").Context.SSHPort; actual != 2390 {
		t.Fatalf("port = %d", actual)
	}
}

func TestConfigProspectiveExternalReadInputsDoNotRequireWriterPermissions(t *testing.T) {
	for _, change := range []string{"unchanged", "large unchanged", "bytes", "mode", "same bytes inode"} {
		t.Run(change, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			asset := filepath.Join(t.TempDir(), "codex.toml")
			content := "# external configuration\n"
			if change == "large unchanged" {
				content = "# " + strings.Repeat("x", 9<<20) + "\n"
			}
			writeConfigCommandFile(t, asset, content, 0o666)
			target := filepath.Join(configHome, "config.env")
			initial := "AGENT_codex_CONFIG='" + asset + "'\n"
			writeConfigCommandFile(t, target, initial)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			prepared := configExactPlan(t, configExactProgram(t, root, environment, nil, nil), loaded, []string{"set", "SSH_PORT", "2390", "--scope", "host"})
			switch change {
			case "bytes":
				writeConfigCommandFile(t, asset, "# changed external configuration\n", 0o666)
			case "mode":
				if err := os.Chmod(asset, 0o644); err != nil {
					t.Fatal(err)
				}
			case "same bytes inode":
				replacement := asset + ".replacement"
				writeConfigCommandFile(t, replacement, "# external configuration\n", 0o666)
				if err := os.Rename(replacement, asset); err != nil {
					t.Fatal(err)
				}
			}
			_, err := prepared.Execute(context.Background(), nil, io.Discard)
			if strings.HasSuffix(change, "unchanged") {
				if err != nil || loadConfigCommandContext(t, root, environment, "default").Context.SSHPort != 2390 {
					t.Fatalf("valid external reader input rejected: %v", err)
				}
			} else {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("external dependency drift accepted: %v", err)
				}
				if content, err := os.ReadFile(target); err != nil || string(content) != initial {
					t.Fatalf("stale plan changed target: %v", err)
				}
			}
		})
	}
}

func TestConfigProspectiveDiagnosticsDoNotDiscloseInvalidNeighborValue(t *testing.T) {
	for _, surface := range []string{"text host", "text shared", "RPC host", "RPC shared"} {
		t.Run(surface, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			const marker = "SECRET_MARKER"
			writeConfigCommandFile(t, filepath.Join(configHome, "yards/neighbor/config.env"), "YARD_TEMPLATE="+marker+"\n")
			loaded := loadConfigCommandContext(t, root, environment, "default")
			arguments := []string{"set", "SSH_PORT", "2390", "--scope", "host", "--yes"}
			if strings.HasSuffix(surface, "shared") {
				arguments = []string{"set", "E2E_VM_SLOT_COUNT", "3", "--scope", "shared", "--yes"}
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: append([]string{"config"}, arguments...), Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(surface, "RPC") {
				handler := &rpcHandler{cli: program, loaded: loaded}
				defer handler.closePlans()
				params, _ := json.Marshal(map[string]any{"command": "config", "arguments": arguments, "exact": true, "stepSchema": 1})
				_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "safe-config", Params: params}, nil)
				if err == nil {
					t.Fatal("invalid neighbor accepted")
				}
				if !strings.Contains(err.Error(), "prospective configuration:") {
					t.Fatalf("RPC rejection did not validate the prospective scope: %v", err)
				}
				wire, marshalErr := json.Marshal(err)
				if marshalErr != nil || strings.Contains(string(wire), marker) || strings.Contains(err.Error(), marker) {
					t.Fatal("RPC diagnostic disclosed invalid source value")
				}
			} else {
				if code := program.Run(context.Background()); code == 0 {
					t.Fatal("invalid neighbor accepted")
				}
				if !strings.Contains(stderr.String(), "prospective configuration:") {
					t.Fatalf("text rejection did not validate the prospective scope: %s", stderr.String())
				}
			}
			if strings.Contains(stdout.String()+stderr.String(), marker) {
				t.Fatal("text diagnostic disclosed invalid source value")
			}
			if content, err := os.ReadFile(filepath.Join(configHome, "config.env")); err != nil || len(content) != 0 {
				t.Fatalf("invalid draft changed host target: %v", err)
			}
			if _, err := os.Stat(filepath.Join(configHome, "overrides/shared/config.env")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid draft changed shared target: %v", err)
			}
		})
	}
}

func TestConfigProspectiveBindsValidatedPackageInputs(t *testing.T) {
	for _, input := range []string{"comment-only template", "owner service", "runtime handler", "guest handler", "credential stop handler", "resource handler", "integration declaration", "integration provision", "integration cleanup", "integration lifecycle handler", "integration source"} {
		for _, change := range []string{"bytes", "mode", "same bytes inode"} {
			t.Run(input+"/"+change, func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
				writeConfigCommandFile(t, filepath.Join(root, "config/subyard.env"), "DEV_UID=1000\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nSHIFT_MODE=shift\nSTORAGE_PATH=$SUBYARD_HOME/storage\nHOST_BASE=/srv/subyard\nRESTRICTED_DISK_PATHS=/srv/subyard\n")
				target := filepath.Join(configHome, "yards/named/config.env")
				initial := "SSH_PORT=2290\n"
				dependency := filepath.Join(root, "config/profiles/sample/handler.sh")
				content := "#!/bin/sh\n"
				mode := os.FileMode(0o700)
				declaration := `{"schema_version":1}`
				switch input {
				case "comment-only template":
					dependency = filepath.Join(root, "config/yards/profiles/empty.env")
					content, mode = "# no scalar assignments\n", 0o600
					initial += "YARD_TEMPLATE=empty\n"
				case "owner service":
					declaration = `{"schema_version":1,"owner_service":"handler.sh"}`
				case "runtime handler":
					declaration = `{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"handler.sh"}}`
				case "guest handler":
					declaration = `{"schema_version":1,"guest_environment":{"handler":"handler.sh"}}`
				case "credential stop handler":
					declaration = `{"schema_version":1,"consumers":[{"id":"sample","zone":"global","path":"sample/key","format":"file","stop_handler":"handler.sh"}]}`
				case "resource handler":
					initial += "ENVIRONMENT_PROFILES=sample\n"
					writeConfigCommandFile(t, filepath.Join(root, "config/profiles/sample/resources/relay.res"), "COMMAND=relay\nHANDLER=handler.sh\nTITLE='Sample relay'\nACTION='up up public-ingress-change reversible'\nACTION='down down public-ingress-change reversible'\nBRINGUP=up\nSHUTDOWN=down\n")
				case "integration declaration", "integration provision", "integration cleanup", "integration lifecycle handler", "integration source":
					packageRoot := filepath.Join(root, "config/agents/sample")
					dependency = filepath.Join(packageRoot, "handler.sh")
					integration := `{"schema_version":1}`
					switch input {
					case "integration declaration":
						dependency = filepath.Join(packageRoot, "integration.json")
						content, mode = integration, 0o600
					case "integration provision":
						integration = `{"schema_version":1,"provision":"handler.sh"}`
					case "integration cleanup":
						integration = `{"schema_version":1,"cleanup":"handler.sh"}`
					case "integration lifecycle handler":
						integration = `{"schema_version":1,"lifecycle":{"handler":"handler.sh"}}`
					case "integration source":
						integration = `{"schema_version":1,"lifecycle":{"handler":"handler.sh","source_files":["source.txt"]}}`
						writeConfigCommandFile(t, dependency, content, mode)
						dependency = filepath.Join(packageRoot, "source.txt")
						content, mode = "source\n", 0o600
					}
					writeConfigCommandFile(t, filepath.Join(packageRoot, "integration.json"), integration)
				}
				writeConfigCommandFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), declaration)
				writeConfigCommandFile(t, dependency, content, mode)
				writeConfigCommandFile(t, target, initial)
				loaded := loadConfigCommandContext(t, root, environment, "named")
				prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: configExactProgram(t, root, environment, nil, nil), Loaded: loaded}
				t.Cleanup(func() { _ = prepared.Close() })
				if err := prepared.prepareConfigScalar(context.Background(), "set", []string{"SSH_PORT", "2390", "--scope", "yard"}); err != nil {
					t.Fatal(errors.Unwrap(err))
				}
				if change == "bytes" {
					writeConfigCommandFile(t, dependency, content+"\n", mode)
				} else if change == "mode" {
					if err := os.Chmod(dependency, mode|0o040); err != nil {
						t.Fatal(err)
					}
				} else {
					replacement := dependency + ".replacement"
					writeConfigCommandFile(t, replacement, content, mode)
					if err := os.Rename(replacement, dependency); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := prepared.execute(context.Background(), nil, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("validated package input drift accepted: %v", err)
				}
				if content, err := os.ReadFile(target); err != nil || string(content) != initial {
					t.Fatalf("stale package input changed target: %v", err)
				}
			})
		}
	}
}

func TestConfigProspectiveOverridesCannotMaskPersistentProfileConstraints(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	home := filepath.Join(root, "state")
	for _, directory := range []string{home, filepath.Join(root, "config", "profiles", "fixture")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "fixture", "profile.conf"), "PROVISION_SCOPE=dedicated\n", 0o600)
	path := filepath.Join(home, "config.env")
	before := "ENVIRONMENT_PROFILES='fixture'\nSSH_PORT='2398'\n"
	testkit.WriteFile(t, path, []byte(before), 0o600)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: append(environment, "ENVIRONMENT_PROFILES="), Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatalf("ordinary supplied context should load: %v", err)
	}
	if code := program.runConfigAuthoring(context.Background(), loaded, "set", []string{"SSH_PORT", "2444", "--scope", "host", "--local"}, true); code == 0 {
		t.Fatal("command override masked invalid persistent profile selection")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != before {
		t.Fatal("invalid persistent draft was written")
	}
	if strings.Contains(stderr.String(), "profile \"fixture\"") {
		t.Fatal("schema diagnostic exposed a setting value")
	}
}

func TestConfigProspectiveContextDefaultsCannotMaskPersistentPaths(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	path := filepath.Join(root, "state", "config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "HOST_BASE='/'\nRESTRICTED_DISK_PATHS='/'\nSSH_PORT='2398'\n"
	writeCLIFile(t, path, before, 0o600)
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatalf("ordinary overridden context: %v", err)
	}
	if code := program.runConfigAuthoring(context.Background(), loaded, "set", []string{"SSH_PORT", "2444", "--scope", "host", "--local"}, true); code == 0 {
		t.Fatal("execution context defaults masked unsafe persistent host paths")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != before {
		t.Fatal("invalid scoped configuration was written")
	}
}
