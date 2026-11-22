package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// blockedGateRefusal is the public refusal of the completed-history gate fixture
// that newConfigApplyRepairFixture installs.
const blockedGateRefusal = `{"status":"migration-required","code":"transition-required","active":"1.2.3-aaaaaaaaaaaa","previous":"release-a","target":"1.2.3-aaaaaaaaaaaa","transaction":null,"message":"activation drift","action":"run yard migrate"}`

// blockedGateTextRefusal is the direct CLI's text rendering of the same gate.
const blockedGateTextRefusal = "yard: release gate: release migration-required (transition-required): active 1.2.3-aaaaaaaaaaaa, target 1.2.3-aaaaaaaaaaaa; this command is not run\n" +
	"yard: cause: activation drift\nyard: next: run yard migrate\n"

const sealedSourceRefusal = "release source admission: sealed original assets are unavailable or inconsistent; restore the exact original release without changing protected history\n"

// blockedGateAdmissionFixture is a blocked release gate with the shipped config,
// integration, profile and extra commands added to the fixture manifest.
func blockedGateAdmissionFixture(t *testing.T, extra ...string) configApplyRepairFixture {
	t.Helper()
	fixture := newConfigApplyRepairFixture(t, false)
	path := filepath.Join(fixture.cli.options.RepositoryRoot, "config", "commands.registry")
	commands := mustReadConfigFixture(t, path)
	for _, line := range strings.Split(string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry"))), "\n") {
		for _, name := range append([]string{"config|", "integration|", "profile|"}, extra...) {
			if strings.HasPrefix(line, name) {
				commands = append(commands, []byte(line+"\n")...)
			}
		}
	}
	testkit.WriteFile(t, path, commands, 0o600)
	return fixture
}

// operationSession is an RPC session of the default yard of fixture.
func operationSession(t *testing.T, fixture configApplyRepairFixture) *rpcHandler {
	t.Helper()
	program := newFixture(t, withWorkingDir("")).New(fixture.cli.options)
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	t.Cleanup(handler.closePlans)
	return handler
}

// TestRunAdmissionBlockedGateRecovery characterizes, through Run, which bounded recovery
// replaces a blocked release gate, the fallback repairs tried before the refusal, the
// deferred teardown gate and the gate skip of a release-transition child.
func TestRunAdmissionBlockedGateRecovery(t *testing.T) {
	refusal := blockedGateTextRefusal
	planned := "yard: plan config: testkit prompt answers exhausted\n"
	sealedSource := "yard: config set: " + sealedSourceRefusal
	cases := []struct {
		arguments []string
		child     bool
		code      int
		stderr    string // ROOT stands for the fixture repository root
	}{
		// No recovery and no fallback applies.
		{[]string{"start"}, false, 1, refusal},
		// An unattributed gate admits no repair: init and config apply wait for migrate.
		{[]string{"init"}, false, 1, refusal},
		{[]string{"config", "apply"}, false, 1, refusal},
		{[]string{"config", "apply", "--all-local"}, false, 1, refusal},
		// Fallbacks: completed scalar authoring handles local scalars; activation
		// selection repair handles selection reductions, which scalar authoring excludes.
		{[]string{"config", "set", "--scope", "yard", "--local", "SSH_PORT", "2224"}, false, 1, sealedSource},
		{[]string{"config", "set", "--scope", "yard", "--local", "CODING_TOOL_INTEGRATIONS", ""}, false, 1, sealedSource},
		{[]string{"integration", "disable", "codex"}, false, 1, sealedSource},
		{[]string{"profile", "disable", "sample"}, false, 1, sealedSource},
		// Scalar authoring requires explicit local storage; otherwise the gate refuses.
		{[]string{"config", "set", "--scope", "yard", "SSH_PORT", "2224"}, false, 1, refusal},
		{[]string{"integration", "enable", "codex"}, false, 1, refusal},
		// The deferred teardown gate after scope selection.
		{[]string{"teardown", "--yes"}, false, 1, refusal},
		// A release-transition child skips the gate, init compatibility and the teardown gate.
		{[]string{"start"}, true, 1, "yard: start: Incus network policy adapter is unavailable\n"},
		{[]string{"init"}, true, 1, "yard: prepare init: open ROOT/config/projects-changed.sh: no such file or directory\n"},
		{[]string{"config", "apply", "--all-local"}, true, 1, planned},
		{[]string{"integration", "enable", "codex"}, true, 1, "yard: prepare integration: integration reconcile requires an existing yard\n"},
		{[]string{"teardown", "--yes"}, true, 1, "yard: prepare teardown action: Incus network policy adapter is required for teardown\n"},
	}
	for _, test := range cases {
		name := strings.Join(test.arguments, "_")
		if test.child {
			name = "child_" + name
		}
		t.Run(name, func(t *testing.T) {
			fixture := blockedGateAdmissionFixture(t)
			options := fixture.cli.options
			var stdout, stderr bytes.Buffer
			options.Arguments, options.Stdout, options.Stderr = test.arguments, &stdout, &stderr
			options.Prompt = &testkit.Prompt{}
			program := newFixture(t, withWorkingDir("")).New(options)
			program.releaseTransitionChild = test.child
			code := program.Run(context.Background())
			want := strings.ReplaceAll(test.stderr, "ROOT", options.RepositoryRoot)
			if code != test.code || stderr.String() != want || (test.stderr == refusal && stdout.Len() != 0) {
				t.Fatalf("exit=%d want %d\nstderr=%q\n  want %q\nstdout=%q", code, test.code, stderr.String(), want, stdout.String())
			}
		})
	}
}

// TestRPCOperationGatePolicy characterizes the RPC release gate of operation.plan and
// operation.execute: the typed refusal, inline init recovery that leaves the gate blocked,
// the release recovery command that skips the gate, and reinspection at execution.
func TestRPCOperationGatePolicy(t *testing.T) {
	refused := func(t *testing.T, err error) {
		t.Helper()
		fault, ok := err.(*rpc.Error)
		if !ok || fault.Code != "transition-required" || fault.Message != blockedGateRefusal {
			t.Fatalf("gate refusal=%#v", err)
		}
	}
	for _, command := range []string{"start", "init", "teardown"} {
		t.Run("plan_"+command, func(t *testing.T) {
			handler := operationSession(t, blockedGateAdmissionFixture(t))
			_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "gate-" + command,
				Params: json.RawMessage(`{"command":"` + command + `","arguments":[]}`)}, nil)
			refused(t, err)
		})
	}
	t.Run("plan_migrate", func(t *testing.T) {
		handler := operationSession(t, blockedGateAdmissionFixture(t))
		_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "gate-migrate",
			Params: json.RawMessage(`{"command":"migrate","arguments":[]}`)}, nil)
		// migrate plans its own recovery past the gate.
		if fault, ok := err.(*rpc.Error); err != nil && (!ok || fault.Code == "transition-required") {
			t.Fatalf("release recovery command consulted the gate: %#v", err)
		}
	})
	t.Run("execute_reinspects", func(t *testing.T) {
		fixture := blockedGateAdmissionFixture(t)
		writeCLIFile(t, fixture.readyMarker, "ready\n", 0o600)
		handler := operationSession(t, fixture)
		if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "gate-execute",
			Params: json.RawMessage(`{"command":"start","arguments":[]}`)}, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fixture.readyMarker); err != nil {
			t.Fatal(err)
		}
		_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "gate-execute",
			Params: json.RawMessage(`{"confirmed":true}`)}, func(string, any) (uint64, error) { return 0, nil })
		refused(t, err)
	})
}

// TestRPCOperationGateRecoverySkip characterizes the RPC release gate skip of operations
// admitted by hard-cleanup or developer-identity recovery, at operation.plan and at
// operation.execute; the same operation without that recovery is refused by the gate.
// Only init passes through init recovery: a retained permit bound to the blocked gate
// admits init at execution but not start, and plan never parses teardown as init.
func TestRPCOperationGateRecoverySkip(t *testing.T) {
	// A pending developer identity of the default yard admits only its plain init.
	pendingIdentity := func(t *testing.T, handler *rpcHandler) {
		t.Helper()
		program, loaded := handler.cli, handler.loaded
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
	}
	fault := func(err error) (string, string) {
		if fault, ok := err.(*rpc.Error); ok {
			return string(fault.Code), fault.Message
		}
		return "", ""
	}
	// Neither recovery command plans in this fixture, so execution reuses a start plan
	// prepared while the gate was ready and binds it to the recovered invocation. Its
	// lifecycle is detached, so only confirmation after the gate can report it.
	const confirmed = "prepared command is not executable"
	cases := []struct {
		name                        string
		command                     string
		arguments                   []string
		setup                       string // identity: pending developer identity
		planCode, planMessage       string
		executeCode, executeMessage string
	}{
		{"cleanup-hard", "cleanup", []string{"--hard"}, "", "plan_failed", "native independent yard backup is unavailable", "confirmation_failed", confirmed},
		{"cleanup-soft", "cleanup", nil, "", "transition-required", blockedGateRefusal, "transition-required", blockedGateRefusal},
		{"identity-init", "init", nil, "identity", "plan_failed", "EOF", "confirmation_failed", confirmed},
		{"init", "init", nil, "", "transition-required", blockedGateRefusal, "transition-required", blockedGateRefusal},
		{"teardown-keep-data", "teardown", []string{"--keep-data"}, "", "transition-required", blockedGateRefusal, "transition-required", blockedGateRefusal},
	}
	for _, test := range cases {
		t.Run("plan_"+test.name, func(t *testing.T) {
			handler := operationSession(t, blockedGateAdmissionFixture(t, "cleanup|"))
			if test.setup == "identity" {
				pendingIdentity(t, handler)
			}
			arguments, _ := json.Marshal(append([]string{}, test.arguments...))
			_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "recovery-" + test.name,
				Params: json.RawMessage(`{"command":"` + test.command + `","arguments":` + string(arguments) + `}`)}, nil)
			if code, message := fault(err); code != test.planCode || message != test.planMessage {
				t.Fatalf("plan=%#v want code %q message %q", err, test.planCode, test.planMessage)
			}
		})
	}
	for _, test := range cases {
		t.Run("execute_"+test.name, func(t *testing.T) {
			fixture := blockedGateAdmissionFixture(t, "cleanup|")
			writeCLIFile(t, fixture.readyMarker, "ready\n", 0o600)
			handler := operationSession(t, fixture)
			id := "recovery-execute-" + test.name
			if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: id,
				Params: json.RawMessage(`{"command":"start","arguments":[]}`)}, nil); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(fixture.readyMarker); err != nil {
				t.Fatal(err)
			}
			planned := handler.plans[id]
			definition, ok := handler.cli.manifest.Lookup(test.command)
			if planned == nil || !ok {
				t.Fatal("recovery execution fixture", planned, ok)
			}
			planned.Definition, planned.Arguments = definition, test.arguments
			lifecycle := planned.lifecycle
			planned.lifecycle = nil
			t.Cleanup(func() { planned.lifecycle = lifecycle; _ = planned.Close() })
			if test.setup == "identity" {
				pendingIdentity(t, handler)
			}
			_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: id,
				Params: json.RawMessage(`{"confirmed":true}`)}, func(string, any) (uint64, error) { return 0, nil })
			if code, message := fault(err); code != test.executeCode || message != test.executeMessage {
				t.Fatalf("execute=%#v want code %q message %q", err, test.executeCode, test.executeMessage)
			}
		})
	}
}

// TestRunAdmissionTeardownFull characterizes full-teardown pre-admission through Run: the
// remote selector refusal, resume of a pending .uninstall store and its inventory failure.
func TestRunAdmissionTeardownFull(t *testing.T) {
	root, environment, _ := resourceCommandFixture(t)
	shipped, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "commands.registry"), string(shipped), 0o600)
	environment = append(environment,
		"YARD_RUNTIME_ROOT=relative-admission-probe",
		"SUBYARD_YARD=admission-missing",
	)
	home := filepath.Join(root, "full-owner")
	pending := home + ".uninstall"
	if err := os.Rename(testkit.TempDir(t), pending); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(pending, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(root, "not-a-directory")
	testkit.WriteFile(t, blocker, []byte("file\n"), 0o600)
	resume := "yard: full uninstall recovery at " + pending + ": "
	var prompt *testkit.Prompt
	run := func(t *testing.T, arguments []string, extra ...string) (int, string, string) {
		t.Helper()
		prompt = &testkit.Prompt{Answers: []bool{false}}
		var stdout, stderr bytes.Buffer
		program := newFixture(t, withWorkingDir("")).New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: arguments,
			Environment: append(append([]string(nil), environment...), extra...), WorkingDir: root,
			Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
			Incus: lifecycleIncus(), AdapterRunner: &testkit.ScriptedAdapter{}, Prompt: prompt,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		code := program.Run(ctx)
		return code, stdout.String(), stderr.String()
	}
	// The resumed store reports exactly what the internal resume entrypoint reports.
	_, _, direct := run(t, []string{"_uninstall-resume", pending}, "SUBYARD_HOME="+home)
	if !strings.HasPrefix(direct, resume) {
		t.Fatalf("resume diagnostic=%q", direct)
	}
	// The direct resume entrypoint is exempt like teardown --full: its prepared
	// command must not reach execution without an admission.
	var discard bytes.Buffer
	resumed := newFixture(t, withWorkingDir("")).New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"_uninstall-resume", pending},
		Environment: append(append([]string(nil), environment...), "SUBYARD_HOME="+home), WorkingDir: root,
		Stdin: strings.NewReader(""), Stdout: &discard, Stderr: &discard,
		Incus: lifecycleIncus(), AdapterRunner: &testkit.ScriptedAdapter{}, Prompt: &testkit.Prompt{},
	})
	resumed.Run(context.Background())
	if resumed.gateAdmitted == nil || resumed.gateAdmitted.kind != admissionExempt {
		t.Fatalf("resume admission=%+v, want exempt", resumed.gateAdmitted)
	}
	cases := []struct {
		name      string
		arguments []string
		extra     []string
		code      int
		stderr    string
	}{
		{"remote", []string{"-Y", "owner/yard", "teardown", "--full"}, []string{"SUBYARD_HOME=" + home}, 2,
			"yard: --full must run locally on the owner host; remote selectors do not authorize host uninstall\n"},
		{"remote-invalid", []string{"-Y", "owner/yard", "teardown", "--full", "--keep-data"}, nil, 2,
			"yard: --full cannot be combined with --keep-data\n"},
		{"pending", []string{"teardown", "--full"}, []string{"SUBYARD_HOME=" + home}, 1, direct},
		// Consent reaches the resume as --yes, from the normalized global -y or ASSUME_YES.
		{"pending-global-yes", []string{"-y", "teardown", "--full"}, []string{"SUBYARD_HOME=" + home}, 1, direct},
		{"pending-assume-yes", []string{"teardown", "--full"}, []string{"SUBYARD_HOME=" + home, "ASSUME_YES=1"}, 1, direct},
		{"inventory", []string{"teardown", "--full"}, []string{"SUBYARD_HOME=" + filepath.Join(blocker, "home")}, 1,
			"yard: full uninstall recovery inventory: lstat " + filepath.Join(blocker, "home") + ".uninstall: not a directory\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := run(t, test.arguments, test.extra...)
			if code != test.code || stderr != test.stderr || stdout != "" {
				t.Fatalf("exit=%d want %d\nstderr=%q\n  want %q\nstdout=%q", code, test.code, stderr, test.stderr, stdout)
			}
		})
	}
	// A valid store must reach confirmation. An overlapping removal root stops
	// confirmed execution before removal, so lost consent cannot hide behind an
	// unreadable store and this test never needs physical teardown.
	original := uninstall.RecoveryPlan{
		Schema: 1, Stage: uninstall.RecoveryPrepared,
		Installation: uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()),
			DataHome: home, ConfigHome: filepath.Join(root, "state"),
			RuntimeRoot: filepath.Join(root, "runtime"), CacheRoot: filepath.Join(root, "cache")},
		Trees: []uninstall.Tree{{Path: pending}},
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pending, "plan.json")
	testkit.WriteFile(t, path, body, 0600)
	if _, _, err := (uninstall.RecoveryStore{Directory: pending, UID: uint32(os.Getuid())}).Read(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		arguments []string
		extra     []string
		consent   bool
	}{
		{"no-consent", []string{"teardown", "--full"}, nil, false},
		{"local-short", []string{"teardown", "--full", "-y"}, nil, true},
		{"local-long", []string{"teardown", "--full", "--yes"}, nil, true},
		{"global-short", []string{"-y", "teardown", "--full"}, nil, true},
		{"environment", []string{"teardown", "--full"}, []string{"ASSUME_YES=1"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := run(t, test.arguments, append(test.extra, "SUBYARD_HOME="+home)...)
			want, prompts := "yard: operation declined\n", 1
			if test.consent {
				want, prompts = "recovery root overlaps an approved removal root", 0
			}
			if code != 1 || len(prompt.Requests) != prompts || !strings.Contains(stderr, want) {
				t.Fatalf("exit=%d prompts=%d want %d stderr=%q", code, len(prompt.Requests), prompts, stderr)
			}
			if after := mustReadConfigFixture(t, path); !bytes.Equal(after, body) {
				t.Fatal("resume changed the protected plan")
			}
		})
	}
}

// TestRunAdmissionSelectorPaths characterizes paths whose admission depends on the yard
// selector: explicit -Y disables host log dispatch, and a registered configuration source
// lets host rename handle the invocation before the gate.
func TestRunAdmissionSelectorPaths(t *testing.T) {
	t.Run("logs", func(t *testing.T) {
		for _, test := range []struct {
			arguments []string
			code      int
			stderr    string
		}{
			{[]string{"logs", "--updates", "-n", "bogus"}, 2, "yard: logs: -n needs a number from 1 to 1000\n"},
			{[]string{"-Y", "admission-missing", "logs", "--updates", "-n", "bogus"}, 2,
				"yard: yard admission-missing, config inspection: unknown yard\n"},
			{[]string{"--yard", "admission-missing", "logs", "--audit"}, 2,
				"yard: yard admission-missing, config inspection: unknown yard\n"},
			// Without a registered configuration source, host rename is gated.
			{[]string{"host", "rename", "next"}, 1, "yard: inspect release transition: mutation gate runtime root"},
		} {
			t.Run(strings.Join(test.arguments, "_"), func(t *testing.T) {
				cliFixture, _ := newResourceCommandFixture(t)
				root, environment := cliFixture.Root, cliFixture.Environment
				shipped, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
				if err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, filepath.Join(root, "config", "commands.registry"), string(shipped), 0o600)
				environment = append(environment,
					"YARD_RUNTIME_ROOT=relative-admission-probe",
					"SUBYARD_YARD=admission-missing",
				)
				var stdout, stderr bytes.Buffer
				program := cliFixture.New(Options{
					RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
					Environment: environment, WorkingDir: root,
					Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
					Incus: lifecycleIncus(), AdapterRunner: &testkit.ScriptedAdapter{},
				})
				code := program.Run(context.Background())
				got := stderr.String()
				if strings.HasSuffix(test.stderr, "\n") && got != test.stderr || !strings.HasPrefix(got, test.stderr) || code != test.code || stdout.Len() != 0 {
					t.Fatalf("exit=%d want %d\nstderr=%q\n  want %q\nstdout=%q", code, test.code, got, test.stderr, stdout.String())
				}
			})
		}
	})
	t.Run("host-rename", func(t *testing.T) {
		for _, test := range []struct {
			arguments      []string
			code           int
			stdout, stderr string
		}{
			{[]string{"-Y", "named", "host", "rename", "owner-b"}, 2, "",
				"yard: host rename must run on the local owner without a yard selector\n"},
			{[]string{"host", "rename"}, 2,
				"Usage: yard host add <owner-endpoint> | list | rename <new-host-id> [--source-host-id <old-host-id>] | remove <host-id> | repair <host-id> [--yes]\n", ""},
		} {
			t.Run(strings.Join(test.arguments, "_"), func(t *testing.T) {
				program, _, _, _, environment := pendingSourceRenameFixture(t, false)
				var stdout, stderr bytes.Buffer
				retry := newFixture(t, withWorkingDir("")).New(Options{RepositoryRoot: program.options.RepositoryRoot, Program: "yard", Arguments: test.arguments,
					Environment: environment, WorkingDir: program.options.RepositoryRoot, Stdout: &stdout, Stderr: &stderr, Prompt: &testkit.Prompt{}})
				if code := retry.Run(context.Background()); code != test.code || stdout.String() != test.stdout || stderr.String() != test.stderr {
					t.Fatalf("exit=%d want %d stdout=%q stderr=%q", code, test.code, stdout.String(), stderr.String())
				}
			})
		}
	})
}
