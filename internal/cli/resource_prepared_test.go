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

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func resourceExactFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, environment, log := resourceCommandFixture(t)
	handler := `#!/usr/bin/env python3
import hashlib, json, os, pathlib, sys
root = pathlib.Path(os.environ['SUBYARD_REPOSITORY_ROOT'])
mode = os.environ.get('SUBYARD_RESOURCE_MODE')
args = sys.argv[1:]
if not args or args[0] != 'run':
    sys.exit(2)
if mode == 'apply':
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
    ready = (root / ('unit-' + unit)).exists()
    if '--noop-expands' in args and count == 1:
        ready = True
    if '--verify-not-converged' in args and mode == 'verify' and unit == 'second':
        ready = False
    conditional = unit == 'second' and not first_ready and not ready
    step = dict(id=unit, target='fixture unit ' + unit,
                observed='running' if ready else ('unknown' if conditional else 'inactive'),
                desired='running', decision='skip' if ready else ('conditional' if conditional else 'apply'),
                verify='read fixture unit ' + unit, consequence='start fixture unit ' + unit)
    if unit == 'second':
        step['preconditions'] = ['the first fixture unit is running']
        step['dependsOn'] = ['first']
    if '--target-drift' in args and (root / 'resource-drift').exists() and unit == 'second':
        step['target'] = 'another fixture unit'
    if '--verify-mismatch' in args and mode == 'verify' and unit == 'second':
        step['target'] = 'another fixture unit'
    steps.append(step)
changed = any(step['decision'] != 'skip' for step in steps)
print(json.dumps(dict(schema='yard.resource-action-assessment.v2', action='run', changed=changed,
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
