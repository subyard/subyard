package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

// runStage is the observable point after admission at which Run stops for an invocation whose
// release gate cannot be inspected and whose selected yard does not exist.
type runStage string

const (
	// stageCompatibility: registered-init migration admission precedes the strict gate.
	stageCompatibility runStage = "compatibility"
	// stageGate: the release mutation gate was consulted before dispatch.
	stageGate runStage = "gate"
	// stageLoad: passed admission without consulting the gate and reached context loading.
	stageLoad runStage = "load"
	// stageDispatch: passed admission without consulting the gate and reached its handler.
	stageDispatch runStage = "dispatch"
	// stageUnknown: no stage signature matched; always a failure.
	stageUnknown runStage = "unknown"
)

// TestRunAdmissionStages characterizes command admission through Run with the shipped
// command manifest and a synthetic profile resource. Release-gate inspection fails with a
// distinctive configuration error, so consulting it is observable without side effects.
func TestRunAdmissionStages(t *testing.T) {
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
	cases := []struct {
		arguments []string
		want      runStage
		marker    string // handler diagnostic that identifies stageDispatch
	}{
		// Manifest read effect.
		{[]string{"check"}, stageLoad, ""},
		{[]string{"security"}, stageLoad, ""},
		{[]string{"status"}, stageLoad, ""},
		{[]string{"space"}, stageLoad, ""},
		{[]string{"logs"}, stageLoad, ""},
		{[]string{"usage"}, stageLoad, ""},
		{[]string{"list"}, stageLoad, ""},
		{[]string{"yards"}, stageLoad, ""},
		{[]string{"info"}, stageLoad, ""},
		{[]string{"_info"}, stageLoad, ""},
		// Help requests are readers for every core command.
		{[]string{"start", "--help"}, stageLoad, ""},
		{[]string{"init", "-h"}, stageLoad, ""},
		{[]string{"keys", "--help"}, stageLoad, ""},
		// Mutating commands consult the gate.
		{[]string{"init"}, stageCompatibility, ""},
		{[]string{"setup"}, stageCompatibility, ""},
		{[]string{"start"}, stageGate, ""},
		{[]string{"stop"}, stageGate, ""},
		{[]string{"shell"}, stageGate, ""},
		{[]string{"provision"}, stageGate, ""},
		{[]string{"sync"}, stageGate, ""},
		{[]string{"bind"}, stageGate, ""},
		{[]string{"clone", "https://example.invalid/r.git"}, stageGate, ""},
		{[]string{"code"}, stageGate, ""},
		{[]string{"export"}, stageGate, ""},
		{[]string{"remove"}, stageGate, ""},
		{[]string{"up"}, stageGate, ""},
		{[]string{"down"}, stageGate, ""},
		{[]string{"remote", "list"}, stageGate, ""},
		{[]string{"_authorize"}, stageGate, ""},
		{[]string{"_state"}, stageGate, ""},
		{[]string{"_keys-exchange"}, stageGate, ""},
		{[]string{"_keys-auto-sync"}, stageGate, ""},
		{[]string{"_keys-init"}, stageGate, ""},
		{[]string{"svc", "demo", "status"}, stageGate, ""},
		// Verb-level readers of mixed commands.
		{[]string{"network", "status"}, stageLoad, ""},
		{[]string{"network", "link"}, stageGate, ""},
		{[]string{"keys", "check-exclusive", "zone"}, stageLoad, ""},
		{[]string{"keys", "--yes", "check-exclusive", "zone"}, stageLoad, ""},
		{[]string{"keys", "check-exclusive"}, stageGate, ""},
		{[]string{"keys", "list"}, stageGate, ""},
		{[]string{"ssh-agent", "status"}, stageLoad, ""},
		{[]string{"ssh-agent", "lock"}, stageLoad, ""},
		{[]string{"ssh-agent", "unlock"}, stageLoad, ""},
		{[]string{"ssh-agent", "unlock", "--key", "key", "--ttl", "1h"}, stageGate, ""},
		{[]string{"host"}, stageLoad, ""},
		{[]string{"host", "list"}, stageLoad, ""},
		{[]string{"host", "add", "owner"}, stageGate, ""},
		{[]string{"host", "remove", "owner"}, stageGate, ""},
		{[]string{"codex", "--check"}, stageDispatch, "prepare codex: choose an action"},
		{[]string{"update", "--check"}, stageLoad, ""},
		{[]string{"integration", "status"}, stageLoad, ""},
		{[]string{"integration", "cleanup", "tool", "--check"}, stageLoad, ""},
		{[]string{"integration", "enable", "tool"}, stageGate, ""},
		{[]string{"integration", "cleanup", "tool"}, stageGate, ""},
		{[]string{"profile", "list"}, stageLoad, ""},
		{[]string{"profile", "status", "fixture"}, stageLoad, ""},
		{[]string{"profile", "enable", "fixture"}, stageGate, ""},
		{[]string{"_project-state", "check-role"}, stageLoad, ""},
		{[]string{"_project-state", "preview"}, stageLoad, ""},
		{[]string{"_project-state", "apply"}, stageGate, ""},
		{[]string{"test-vms", "logs"}, stageLoad, ""},
		{[]string{"test-vms", "--help"}, stageLoad, ""},
		{[]string{"config"}, stageLoad, ""},
		{[]string{"config", "show"}, stageLoad, ""},
		{[]string{"config", "--yes", "status"}, stageLoad, ""},
		{[]string{"config", "fields"}, stageLoad, ""},
		{[]string{"config", "paths"}, stageLoad, ""},
		{[]string{"config", "sync", "path"}, stageLoad, ""},
		{[]string{"config", "sync", "--check"}, stageLoad, ""},
		{[]string{"config", "sync", "status"}, stageLoad, ""},
		{[]string{"config", "sync"}, stageGate, ""},
		{[]string{"config", "set", "SSH_PORT", "2223"}, stageGate, ""},
		{[]string{"config", "apply"}, stageGate, ""},
		{[]string{"config", "apply", "--all-local"}, stageGate, ""},
		{[]string{"demo", "status"}, stageLoad, ""},
		{[]string{"demo", "--help"}, stageLoad, ""},
		{[]string{"demo", "run"}, stageGate, ""},
		{[]string{"demo", "unknown"}, stageGate, ""},
		// Gate owned by the handler or bypassed for registration repair.
		{[]string{"update"}, stageLoad, ""},
		{[]string{"teardown"}, stageLoad, ""},
		{[]string{"uninstall", "--keep-data"}, stageLoad, ""},
		{[]string{"config", "repair-registration", "default"}, stageLoad, ""},
	}
	for _, test := range cases {
		t.Run(strings.Join(test.arguments, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root,
				Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
				Incus: lifecycleIncus(), AdapterRunner: &testkit.ScriptedAdapter{},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			code := program.Run(ctx)
			output := stderr.String()
			got := stageUnknown
			switch {
			case strings.Contains(output, "init compatibility migrations: mutation gate runtime root"):
				got = stageCompatibility
			case strings.Contains(output, "inspect release transition: mutation gate runtime root"):
				got = stageGate
			case strings.Contains(output, "yard admission-missing, config inspection"):
				got = stageLoad
			case test.marker != "" && strings.Contains(output, test.marker):
				got = stageDispatch
			}
			if got != test.want {
				t.Fatalf("stage=%s want %s exit=%d stdout=%q stderr=%q", got, test.want, code, stdout.String(), output)
			}
		})
	}
}

// TestRunAdmissionEarlyPaths characterizes dispatch that stops before the release gate and
// context loading. Each case requires the exit code and a diagnostic or output owned by the
// expected path, so an unrelated early exit cannot satisfy it.
func TestRunAdmissionEarlyPaths(t *testing.T) {
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
	cases := []struct {
		arguments []string
		stdin     string
		code      int
		marker    string // substring of stdout or stderr owned by the expected path
	}{
		{[]string{"network", "--help"}, "", 0, "Usage: yard network status [--json]"},
		{[]string{"list", "--complete-profiles"}, "", 0, "fixture\n"},
		{[]string{"help"}, "", 0, "Usage: yard [option] <command> [args]"},
		// A malformed frame proves that the RPC session, not another early path, read stdin.
		{[]string{"rpc", "--stdio"}, "malformed\n", 1, "yard: RPC session: RPC frame size"},
		{[]string{"migrate", "--check"}, "", 1, "yard: migrate: mutation gate runtime root"},
		{[]string{"_migrate"}, "", 2, "yard: internal: invalid _migrate action"},
		{[]string{"_release-transition"}, "", 2, "yard: release transition request is invalid"},
		{[]string{"config", "validate"}, "", 1, "yard admission-missing: prospective configuration:"},
		{[]string{"config", "repair-alias"}, "", 2, "yard: config repair-alias: repair-alias requires --local"},
		{[]string{"config", "reduce-selection"}, "", 2, "yard: config reduce-selection requires one optional selection"},
		{[]string{"logs", "--updates", "-n", "bogus"}, "", 2, "yard: logs: -n needs a number"},
		{[]string{"teardown", "--bogus"}, "", 2, `yard: unknown teardown argument "--bogus"`},
		{[]string{"service", "start", "missing"}, "", 1, `infrastructure service "missing" is not declared`},
		{[]string{"test-vms", "status"}, "", 1, `infrastructure service "test-vms" is not declared`},
		{[]string{"unknown-command"}, "", 2, `yard: unknown command "unknown-command"`},
	}
	for _, test := range cases {
		t.Run(strings.Join(test.arguments, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root,
				Stdin: strings.NewReader(test.stdin), Stdout: &stdout, Stderr: &stderr,
				Incus: lifecycleIncus(), AdapterRunner: &testkit.ScriptedAdapter{},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			code := program.Run(ctx)
			output := stdout.String() + stderr.String()
			if code != test.code || !strings.Contains(output, test.marker) {
				t.Fatalf("exit=%d want %d, marker %q missing: stdout=%q stderr=%q",
					code, test.code, test.marker, stdout.String(), stderr.String())
			}
			for _, later := range []string{
				"inspect release transition: mutation gate runtime root",
				"yard admission-missing, config inspection",
			} {
				if strings.Contains(output, later) {
					t.Fatalf("early path reached a later stage (%q): %q", later, output)
				}
			}
		})
	}
}

// TestAdmissionPolicy pins the pure classification behind Run's stages, including the
// reader/exempt distinction and the recovery that may replace a blocked gate.
func TestAdmissionPolicy(t *testing.T) {
	root, environment, _ := resourceCommandFixture(t)
	shipped, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "commands.registry"), string(shipped), 0o600)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		readOnly      bool
		gate          gatePolicy
		recovery      gateRecovery
		early         earlyPath
		ownerRecovery bool
		readOnlyRoute bool
	}
	reader := want{readOnly: true, gate: gateSkipRead, readOnlyRoute: true}
	writer := want{gate: gateRequired, ownerRecovery: true}
	with := func(base want, change func(*want)) want { change(&base); return base }
	cases := []struct {
		arguments []string
		explicit  bool
		want      want
	}{
		{[]string{"status"}, false, reader},
		{[]string{"start", "--help"}, false, reader},
		{[]string{"start"}, false, writer},
		{[]string{"code"}, false, with(writer, func(w *want) { w.readOnlyRoute = true })},
		{[]string{"svc", "demo", "status"}, false, writer},
		{[]string{"demo", "status"}, false, reader},
		{[]string{"demo", "purge"}, false, with(writer, func(w *want) { w.recovery = recoveryResourceShutdown })},
		{[]string{"keys", "-y", "check-exclusive", "zone"}, false, reader},
		{[]string{"_keys-init", "check-exclusive", "zone"}, false, writer},
		{[]string{"ssh-agent", "unlock", "--key", "k", "--ttl", "1h"}, false, writer},
		{[]string{"ssh-agent", "lock"}, false, reader},
		{[]string{"init"}, false, with(writer, func(w *want) { w.recovery = recoveryProfileInit })},
		{[]string{"config", "apply", "--all-local"}, false, with(writer, func(w *want) { w.recovery = recoveryConfigApply })},
		{[]string{"config", "set", "SSH_PORT", "2223"}, false, writer},
		{[]string{"config", "sync", "--check"}, false, reader},
		{[]string{"config", "repair-registration", "default"}, false, want{gate: gateSkipRegistrationRepair, readOnlyRoute: true}},
		{[]string{"config", "validate"}, false, with(writer, func(w *want) { w.early = earlyConfigValidate })},
		{[]string{"integration", "enable", "tool"}, false, want{gate: gateRequired, recovery: recoveryIntegrationCleanup, readOnlyRoute: true}},
		{[]string{"integration", "status"}, false, reader},
		{[]string{"profile", "enable", "fixture"}, false, want{gate: gateRequired, readOnlyRoute: true}},
		{[]string{"update"}, false, want{gate: gateHandlerOwned, ownerRecovery: true}},
		{[]string{"update", "--check"}, false, reader},
		{[]string{"teardown"}, false, want{gate: gateAfterScope, readOnlyRoute: true}},
		{[]string{"teardown", "--help"}, false, reader},
		{[]string{"network", "--help"}, false, with(reader, func(w *want) { w.early = earlyNetworkHelp })},
		{[]string{"list", "--complete-profiles"}, false, with(reader, func(w *want) { w.early = earlyProfileCompletion })},
		{[]string{"migrate"}, false, with(writer, func(w *want) { w.early = earlyCurrentMigration })},
		{[]string{"host", "rename", "next"}, false, with(writer, func(w *want) { w.early = earlyHostRename })},
		{[]string{"help"}, false, with(reader, func(w *want) { w.early = earlyHelp })},
		{[]string{"rpc", "--stdio"}, false, with(writer, func(w *want) { w.early = earlyRPC })},
		{[]string{"logs", "--audit"}, false, with(reader, func(w *want) { w.early = earlyHostLogs })},
		{[]string{"logs", "--audit"}, true, reader},
	}
	for _, test := range cases {
		t.Run(strings.Join(test.arguments, "_"), func(t *testing.T) {
			definition, core := program.manifest.Lookup(test.arguments[0])
			resourceDefinition, profileResource := program.resources.Lookup(test.arguments[0])
			admission := program.admission(definition, core, resourceDefinition, profileResource, test.arguments[1:], test.explicit)
			got := want{admission.readOnly, admission.gate, admission.recovery, admission.early, admission.ownerRecovery, admission.readOnlyRoute}
			if got != test.want {
				t.Fatalf("admission=%+v want %+v", got, test.want)
			}
		})
	}
}
