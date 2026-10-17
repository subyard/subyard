package cli

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
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// This is the real CLI reader invoked by the synthetic native handler below.
func TestResourceAuditedReaderProcess(t *testing.T) {
	role := os.Getenv("SUBYARD_RESOURCE_AUDIT_CHILD")
	if role == "" {
		return
	}
	uninstall.UseArtifactGuard(repositoryRoot(t))
	if role == "waiter" {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, release, err := uninstall.OwnerLock(ctx, os.Getenv("SUBYARD_HOME"), nil)
		if err == nil {
			release()
			t.Fatal("nested reader released its parent's owner barrier")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		return
	}
	program, err := New(Options{RepositoryRoot: os.Getenv("SUBYARD_REPOSITORY_ROOT"),
		Program: "yard", Arguments: []string{"keys", "check-exclusive", "fixture-zone"},
		Environment: os.Environ(), WorkingDir: os.Getenv("SUBYARD_REPOSITORY_ROOT"),
		Stdout: io.Discard, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("nested key reader returned %d", code)
	}
	if os.Getenv("SUBYARD_OWNER_LOCK_FD") != "" {
		waiter := exec.Command(os.Args[0], "-test.run=^TestResourceAuditedReaderProcess$")
		values := environmentMap(os.Environ())
		delete(values, "SUBYARD_OWNER_LOCK_FD")
		values["SUBYARD_RESOURCE_AUDIT_CHILD"] = "waiter"
		waiter.Env = environmentList(values, nil)
		if output, err := waiter.CombinedOutput(); err != nil {
			t.Fatalf("independent barrier waiter: %v: %s", err, output)
		}
	}
}

func TestResourceExactNestedReaderRetainsAuditAndOwnerBarrier(t *testing.T) {
	root, environment, _ := resourceExactFixture(t)
	nativeInstallationFixture(t, environment)
	manifestPath := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte("keys||@keys||forward|mutate|dynamic|public|lifecycle|keys|keys [command]|fixture credential reader|--yes --help|\n")...)
	testkit.WriteFile(t, manifestPath, manifest, 0600)
	handlerPath := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh")
	handler, err := os.ReadFile(handlerPath)
	if err != nil {
		t.Fatal(err)
	}
	reader := `
import subprocess
with (root / 'reader-stages').open('a') as output:
    output.write(mode + ' begin\n')
child_env = dict(os.environ, SUBYARD_RESOURCE_AUDIT_CHILD='reader')
descriptor = os.environ.get('SUBYARD_OWNER_LOCK_FD')
subprocess.run([os.environ['SUBYARD_DISPATCH_PATH'], '-test.run=^TestResourceAuditedReaderProcess$'],
    env=child_env, pass_fds=(() if not descriptor else (int(descriptor),)), stdout=subprocess.DEVNULL, check=True)
with (root / 'reader-stages').open('a') as output:
    output.write(mode + ' finished\n')
`
	handler = []byte(strings.Replace(string(handler), "args = sys.argv[1:]", reader+"\nargs = sys.argv[1:]", 1))
	testkit.WriteFile(t, handlerPath, handler, 0700)
	home := environmentValue(environment, "SUBYARD_HOME")
	ledger := filepath.Join(home, "retained-ledger")
	testkit.WriteFile(t, ledger, []byte("unchanged protected data"), 0600)
	before, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, DispatcherPath: executable, Program: "yard",
		Arguments: []string{"demo", "run", "--yes"}, Environment: environment, WorkingDir: root,
		Stdout: io.Discard, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	// Race-instrumented helper processes also run their normal exit checks.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := program.Run(ctx)
	stages, readErr := os.ReadFile(filepath.Join(root, "reader-stages"))
	if readErr != nil || !strings.Contains(string(stages), "prepare finished\nprepare begin\n") {
		t.Fatalf("real initial reader and exact refresh were not reached: %s %v; code=%d stderr=%s", stages, readErr, code, stderr.String())
	}
	if code != 0 {
		t.Fatalf("nested audited reader blocked resource: code=%d stages=%s stderr=%s", code, stages, stderr.String())
	}
	for _, stage := range []string{"apply finished\n", "verify finished\n"} {
		if !strings.Contains(string(stages), stage) {
			t.Fatalf("missing nested reader stage %q: %s", stage, stages)
		}
	}
	audit, err := os.ReadFile(filepath.Join(home, "logs", "yard.log"))
	if err != nil {
		t.Fatal(err)
	}
	completed := strings.Count(string(stages), " finished\n")
	for _, record := range []string{" -- keys check-exclusive fixture-zone\n", " -- operation.started\n", " -- operation.finished\n"} {
		if strings.Count(string(audit), record) < completed {
			t.Fatalf("nested reader omitted actual audit %q: %s", record, audit)
		}
	}
	after, err := os.Stat(home)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("nested audit replaced installation root", err)
	}
	if body, err := os.ReadFile(ledger); err != nil || string(body) != "unchanged protected data" {
		t.Fatal("nested reader changed protected data", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := uninstall.OwnerLock(ctx, home, nil)
	if err != nil {
		t.Fatal("completed resource retained owner barrier", err)
	}
	release()
}

func TestResourceOwnerBarrierUsesOnlyCurrentAdmission(t *testing.T) {
	for _, scenario := range []string{"held", "absent", "other-cli", "wire", "transport", "foreign-home", "retired", "session-prepare", "session-apply", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			root, environment, _ := resourceExactFixture(t)
			nativeInstallationFixture(t, environment)
			// This caller value must never become descriptor authority.
			environment = append(environment, "SUBYARD_OWNER_LOCK_FD=malformed-caller-value")
			program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root,
				Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.resources.Lookup("demo")
			home := loaded.Context.Paths.DataHome
			if scenario == "other-cli" {
				_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			} else if scenario != "absent" {
				admission, err := program.originalOwnerWrite(loaded)
				if err != nil {
					t.Fatal(err)
				}
				release, err := program.acquireOwnerMutation(context.Background(), admission, false, false)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, release, err := uninstall.OwnerLock(ctx, home, nil)
				if err != nil {
					t.Error("completed resource child retained the owner barrier", err)
					return
				}
				release()
			})
			switch scenario {
			case "wire":
				program.resourceWire = "view"
			case "transport":
				program.resourceSessionTransport = "fixture transport"
			case "foreign-home":
				loaded.Context.Paths.DataHome += "-foreign"
			case "retired":
				if err := uninstall.AdvanceOwnerEpoch(home, 0, 1); err != nil {
					t.Fatal(err)
				}
			case "session-prepare", "session-apply":
				// Exercise the actual session launch sites, not just the helper.
				testkit.WriteFile(t, definition.HandlerPath(), []byte("#!/bin/sh\n[ -z \"${SUBYARD_OWNER_LOCK_FD:-}\" ] || exit 71\nprintf '{}\\n'\n"), 0700)
				if scenario == "session-prepare" {
					_, err = program.prepareResourceModeTimeout(context.Background(), loaded, definition, []string{"view"}, "prepare", time.Second)
				} else {
					runner := &resourceApplyRunner{cli: program, loaded: loaded, definition: definition,
						arguments: []string{"view"}, localAction: "view", effect: domain.ActionSession}
					_, _, err = runner.Run(context.Background(), domain.AdapterRequest{Adapter: "resource", Action: "view"}, nil)
				}
				if err != nil {
					t.Fatal("session inherited owner authority", err)
				}
				return
			}
			command := exec.Command("true")
			var output io.ReadCloser
			var cancel context.CancelFunc
			if scenario == "cancelled" {
				var ctx context.Context
				ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				command = exec.CommandContext(ctx, "sh", "-c", "printf 'ready\\n'; exec sleep 60")
				configureResourceProcess(command)
				output, err = command.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
			}
			command.Env = program.resourceEnvironment(loaded, definition, "prepare", "", "")
			release, err := program.resourceOwnerBarrier(context.Background(), loaded, command)
			if scenario == "foreign-home" || scenario == "retired" {
				if err == nil || len(command.ExtraFiles) != 0 {
					if release != nil {
						release()
					}
					t.Fatal("stale or foreign owner authority delegated", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if scenario != "held" && scenario != "cancelled" {
				if len(command.ExtraFiles) != 0 || environmentValue(command.Env, "SUBYARD_OWNER_LOCK_FD") != "" {
					t.Fatal("resource acquired or forwarded unrelated descriptor authority")
				}
				if scenario == "absent" {
					if _, err := os.Stat(uninstall.OwnerLockPath(home)); !os.IsNotExist(err) {
						t.Fatal("unadmitted resource created an owner barrier", err)
					}
				}
				return
			}
			if len(command.ExtraFiles) != 1 || environmentValue(command.Env, "SUBYARD_OWNER_LOCK_FD") != "3" {
				t.Fatal("current barrier not delegated at the actual descriptor index")
			}
			actual, err := command.ExtraFiles[0].Stat()
			if err != nil {
				t.Fatal(err)
			}
			expected, err := os.Stat(uninstall.OwnerLockPath(home))
			if err != nil || !os.SameFile(actual, expected) {
				t.Fatal("delegated descriptor is not the current exact-home inode", err)
			}
			if scenario == "cancelled" {
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				ready := make([]byte, len("ready\n"))
				_, readErr := io.ReadFull(output, ready)
				cancel()
				waitErr := command.Wait()
				if readErr != nil || string(ready) != "ready\n" || waitErr == nil {
					t.Fatalf("bounded child cancellation failed: ready=%q read=%v wait=%v", ready, readErr, waitErr)
				}
			}
			release()
			waiter := exec.Command(os.Args[0], "-test.run=^TestResourceAuditedReaderProcess$")
			values := environmentMap(environment)
			delete(values, "SUBYARD_OWNER_LOCK_FD")
			values["SUBYARD_RESOURCE_AUDIT_CHILD"] = "waiter"
			waiter.Env = environmentList(values, nil)
			if output, err := waiter.CombinedOutput(); err != nil {
				t.Fatalf("closing borrowed descriptor unlocked parent: %v: %s", err, output)
			}
		})
	}
}

func resourceExactFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, environment, log := resourceCommandFixture(t)
	handler := `#!/usr/bin/env python3
import hashlib, json, os, pathlib, sys
root = pathlib.Path(os.environ['SUBYARD_REPOSITORY_ROOT'])
mode = os.environ.get('SUBYARD_RESOURCE_MODE')
args = sys.argv[1:]
if not args or args[0] not in ('run', 'down', 'purge'):
    sys.exit(2)
verb = args[0]
if mode == 'apply':
    if verb != 'run':
        for unit in (('first',) if verb == 'down' else ('first', 'second')):
            (root / ('unit-' + unit)).unlink(missing_ok=True)
        sys.exit(0)
    if '--no-effect' not in args:
        for unit in ('first', 'second'):
            path = root / ('unit-' + unit)
            if not path.exists():
                path.write_text('running')
                with (root / 'resource-apply.log').open('a') as output:
                    output.write(unit + '\n')
    print('applied fixture runtime')
    sys.exit(0)
if mode not in ('prepare', 'verify'):
    sys.exit(2)
count = 0
if mode == 'prepare':
    log = root / 'resource-prepare.log'
    with log.open('a') as output:
        output.write('prepare\n')
    count = len(log.read_text().splitlines())
if '--v1' in args or mode == 'verify' and '--bad-verify' in args:
    print(json.dumps(dict(schema='yard.resource-action-assessment.v1', action='run', changed=True, consequences=['start fixture runtime'])))
    sys.exit(0)
binding = str(root) + ':fixture-target'
if '--binding-drift' in args and (root / 'resource-drift').exists():
    binding += ':changed'
if '--binding-on-second' in args and count >= 2:
    binding += ':changed'
first_ready = (root / 'unit-first').exists()
steps = []
for unit in ('first', 'second'):
    exists = (root / ('unit-' + unit)).exists()
    desired = 'running' if verb == 'run' or verb == 'down' and unit == 'second' else 'inactive'
    ready = exists == (desired == 'running')
    if '--noop-expands' in args and count == 1:
        ready = True
    if '--verify-not-converged' in args and mode == 'verify' and unit == 'second':
        ready = False
    conditional = verb == 'run' and unit == 'second' and not first_ready and not ready
    step = dict(id=unit, target='fixture unit ' + unit,
                observed='unknown' if conditional else ('running' if exists else 'inactive'),
                desired=desired, decision='skip' if ready else ('conditional' if conditional else 'apply'),
                verify='read fixture unit ' + unit, consequence='start fixture unit ' + unit)
    if verb == 'run' and unit == 'second':
        step['preconditions'] = ['the first fixture unit is running']
        step['dependsOn'] = ['first']
    if '--target-drift' in args and (root / 'resource-drift').exists() and unit == 'second':
        step['target'] = 'another fixture unit'
    if '--verify-mismatch' in args and mode == 'verify' and unit == 'second':
        step['target'] = 'another fixture unit'
    steps.append(step)
changed = any(step['decision'] != 'skip' for step in steps)
print(json.dumps(dict(schema='yard.resource-action-assessment.v2', action=verb, changed=changed,
    consequences=[step['consequence'] for step in steps if step['decision'] != 'skip'],
    steps=steps, binding=hashlib.sha256(binding.encode()).hexdigest())))
`
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh"), handler, 0o700)
	return root, environment, log
}

func TestResourceExactDirectAllowsPartialConvergence(t *testing.T) {
	root, environment, applyLog := resourceExactFixture(t)
	prompt := &callbackPrompt{callback: func() {
		writeCLIFile(t, filepath.Join(root, "unit-first"), "already-running", 0o600)
	}}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run"},
		Environment: environment, WorkingDir: root, Stdout: io.Discard, Stderr: &stderr, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("partial convergence failed: code=%d stderr=%s", code, stderr.String())
	}
	if log := readResourceApplyLog(t, applyLog); log != "second\n" {
		t.Fatalf("resource repeated a converged step: %q", log)
	}
	first, err := os.ReadFile(filepath.Join(root, "unit-first"))
	if err != nil || string(first) != "already-running" {
		t.Fatalf("resource overwrote converged state: %q, %v", first, err)
	}
}

func TestResourceExactRejectsDriftBeforeApply(t *testing.T) {
	for _, argument := range []string{"--binding-drift", "--target-drift", "--binding-on-second", "--noop-expands"} {
		t.Run(argument, func(t *testing.T) {
			root, environment, applyLog := resourceExactFixture(t)
			prompt := &callbackPrompt{callback: func() {
				writeCLIFile(t, filepath.Join(root, "resource-drift"), "changed", 0o600)
			}}
			var stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run", argument},
				Environment: environment, WorkingDir: root, Stdout: io.Discard, Stderr: &stderr, Prompt: prompt})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
				t.Fatalf("drift accepted: code=%d stderr=%s", code, stderr.String())
			}
			if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
				t.Fatalf("drift reached resource apply: %v", err)
			}
		})
	}
}

func TestResourceExactRequiresNativePostconditionVerification(t *testing.T) {
	for _, argument := range []string{"--no-effect", "--bad-verify", "--verify-not-converged", "--verify-mismatch"} {
		t.Run(argument, func(t *testing.T) {
			root, environment, _ := resourceExactFixture(t)
			var stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run", argument, "--yes"},
				Environment: environment, WorkingDir: root, Stdout: io.Discard, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "verif") {
				t.Fatalf("unverified apply reported success: code=%d stderr=%s", code, stderr.String())
			}
		})
	}
}

func TestResourceExactInitialNoOpRechecksAndVerifiesWithoutPrompt(t *testing.T) {
	root, environment, applyLog := resourceExactFixture(t)
	for _, unit := range []string{"first", "second"} {
		writeCLIFile(t, filepath.Join(root, "unit-"+unit), "running", 0o600)
	}
	prompt := &testkit.Prompt{}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run"},
		Environment: environment, WorkingDir: root, Stdout: io.Discard, Stderr: io.Discard, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || len(prompt.Requests) != 0 {
		t.Fatalf("converged resource prompted or failed: code=%d requests=%#v", code, prompt.Requests)
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("converged resource reached apply: %v", err)
	}
	if log := readResourceApplyLog(t, filepath.Join(root, "resource-prepare.log")); log != "prepare\nprepare\n" {
		t.Fatalf("initial no-op was not rechecked: %q", log)
	}
}

func TestResourceExactRPCUsesNativePlanAndSeparateDiagnosticStream(t *testing.T) {
	root, environment, applyLog := resourceExactFixture(t)
	var stdout, diagnostics bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	defer handler.closePlans()
	value, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "resource-exact",
		Params: json.RawMessage(`{"command":"demo","arguments":["run"],"exact":true}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := value.(exactOperationPlan)
	if len(plan.Plan.Steps) != 2 || plan.Plan.Steps[1].Decision != domain.StepConditional {
		t.Fatalf("native dependency authorization omitted: %#v", plan.Plan.Steps)
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("RPC preparation applied resource: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"confirmed": true, "digest": plan.Digest})
	if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "resource-exact", Params: params}, nil); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || !strings.Contains(diagnostics.String(), "applied fixture runtime") {
		t.Fatalf("native output reached protocol stdout: stdout=%q diagnostics=%q", stdout.String(), diagnostics.String())
	}
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "resource-exact", Params: params}, nil)
	var fault *rpc.Error
	if !errors.As(err, &fault) || fault.Code != "plan_not_found" {
		t.Fatalf("resource exact plan replay accepted: %v", err)
	}
}

func TestResourceExactRPCRefusesV1Mutation(t *testing.T) {
	root, environment, applyLog := resourceExactFixture(t)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		WorkingDir: root, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	defer handler.closePlans()
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "resource-v1",
		Params: json.RawMessage(`{"command":"demo","arguments":["run","--v1"],"exact":true}`)}, nil)
	if err == nil {
		t.Fatal("v1 resource mutation was admitted to exact RPC")
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("unsupported plan applied resource: %v", err)
	}
}
