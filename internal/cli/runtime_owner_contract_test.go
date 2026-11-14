package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// runtimeOwnerCall is one native invocation of a declared runtime handler.
type runtimeOwnerCall struct {
	path, digest, action string
	arguments            []string
	environment          []string
}

// Shell bookkeeping of the synthetic handler itself.
var runtimeOwnerVolatileEnvironment = []string{"PWD=", "SHLVL=", "_=", "OLDPWD="}

// Allowed environment differences between activation and init, by name. Every
// other entry must match exactly.
var runtimeOwnerAllowedDifferences = map[string]string{
	"SUBYARD_OPERATION_ID": "each command has its own operation",
	// Activation applies the installed view of an active release repair; init
	// applies desired configuration.
	"ENVIRONMENT_PROFILES": "installed versus desired selection",
}

// Activation drops command-scope overrides and reloads persisted configuration
// (freshMigrationEnvironment), so these command inputs reach only init.
var runtimeOwnerInitOnlyEnvironment = []string{"SUBYARD_POWER_RECONCILER_PATH", "SUBYARD_POWER_UNIT_PATH", "SUBYARD_CONFIG_SECRETS_DIR"}

func runtimeOwnerContractEnvironment(environment []string) []string {
	return slices.DeleteFunc(slices.Clone(environment), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		_, allowed := runtimeOwnerAllowedDifferences[name]
		return allowed || slices.Contains(runtimeOwnerInitOnlyEnvironment, name)
	})
}

// runtimeOwnerDesired is the synthetic handler's desired digest for the
// selection one call observed.
func runtimeOwnerDesired(call runtimeOwnerCall, name string) string {
	for _, entry := range call.environment {
		if value, found := strings.CutPrefix(entry, "ENVIRONMENT_PROFILES="); found && slices.Contains(strings.Fields(value), name) {
			return strings.Repeat("b", 64)
		}
	}
	return strings.Repeat("c", 64)
}

// The synthetic handler keeps its native state in a file: observe reports the
// recorded actual against a selection-dependent desired digest, apply records the
// exact call, and a failing apply leaves a partial actual like a real hook.
func syntheticRuntimeOwnerHandler(state, calls, fail, name string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
{
  printf 'call\t%%s\t%%s\t%%s\n' "$0" "$(sha256sum "$0" | cut -d' ' -f1)" "$*"
  env | LC_ALL=C sort | sed 's/^/env\t/'
  printf 'end\n'
} >> '%s'
desired=%s
case " ${ENVIRONMENT_PROFILES:-} " in *" %s "*) desired=%s ;; esac
actual=$(cat '%s')
case "$1" in
observe)
  state=stale; [ "$actual" = "$desired" ] && state=current
  printf '{"state":"%%s","actual":"%%s","desired":"%%s"}\n' "$state" "$actual" "$desired" ;;
apply)
  [ "$#" -eq 4 ] && [ "$3" = "$actual" ] && [ "$4" = "$desired" ] || exit 17
  if [ -e '%s' ]; then printf '%%s' %s > '%s'; exit 99; fi
  printf '%%s' "$4" > '%s'
  printf '{"state":"current","actual":"%%s","desired":"%%s"}\n' "$4" "$4" ;;
*) exit 18 ;;
esac
`, calls, strings.Repeat("c", 64), name, strings.Repeat("b", 64), state, fail, strings.Repeat("d", 64), state, state)
}

func readRuntimeOwnerCalls(t *testing.T, path string) []runtimeOwnerCall {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var result []runtimeOwnerCall
	var current *runtimeOwnerCall
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.SplitN(line, "\t", 4)
		switch {
		case fields[0] == "call" && len(fields) == 4:
			words := strings.Fields(fields[3])
			result = append(result, runtimeOwnerCall{path: fields[1], digest: fields[2], action: words[0], arguments: words[1:]})
			current = &result[len(result)-1]
		case fields[0] == "env" && current != nil && len(fields) >= 2:
			entry := strings.Join(fields[1:], "\t")
			if !slices.ContainsFunc(runtimeOwnerVolatileEnvironment, func(prefix string) bool { return strings.HasPrefix(entry, prefix) }) {
				current.environment = append(current.environment, entry)
			}
		case fields[0] == "end":
			current = nil
		}
	}
	return result
}

func runtimeOwnerApplies(calls []runtimeOwnerCall) []runtimeOwnerCall {
	return slices.DeleteFunc(slices.Clone(calls), func(call runtimeOwnerCall) bool { return call.action != "apply" })
}

// installSyntheticRuntimeOwner copies a shipped profile package into the fixture
// and replaces its declared runtime handler with the synthetic protocol handler.
func installSyntheticRuntimeOwner(t *testing.T, root string, definition profile.Definition) (state, calls, fail string) {
	t.Helper()
	destination := filepath.Join(root, "config", "profiles", definition.Name)
	if err := os.RemoveAll(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(destination, os.DirFS(definition.Root)); err != nil {
		t.Fatal(err)
	}
	state, calls, fail = filepath.Join(root, "owner-state"), filepath.Join(root, "owner-calls"), filepath.Join(root, "owner-fail")
	testkit.WriteFile(t, state, []byte(strings.Repeat("a", 64)), 0o600)
	testkit.WriteFile(t, filepath.Join(destination, definition.Runtime.Handler),
		[]byte(syntheticRuntimeOwnerHandler(state, calls, fail, definition.Name)), 0o700)
	return state, calls, fail
}

// prepareRuntimeOwnerInit builds the native init profile-runtime stage the way
// yard init does: a fresh operation, a prepared native plan, then ApplyStage.
func prepareRuntimeOwnerInit(t *testing.T, program *CLI) reconcileruntime.Runtime {
	t.Helper()
	program.env["SUBYARD_OPERATION_ID"] = newOperationID()
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := program.initPlatform(loaded, nil).(reconcileruntime.Runtime)
	if !ok {
		t.Fatal("init platform is not the native runtime")
	}
	plan, err := runtime.PrepareProfileRuntimes(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.RuntimePlan = plan
	return runtime
}

// Activation repair and yard init must route every declared profile runtime to
// the same shipped handler and native protocol, so that init can finish what a
// failed activation left behind. Owner-specific repeatability and removal are
// tested by the owning profile; this contract covers routing only.
func TestRuntimeOwnersShareActivationAndInitProtocol(t *testing.T) {
	shipped, err := profile.Load(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	owners := slices.DeleteFunc(shipped, func(definition profile.Definition) bool { return definition.Runtime == nil })
	if len(owners) == 0 {
		t.Fatal("no declared profile runtime owners")
	}
	for _, definition := range owners {
		t.Run(definition.Runtime.ActivationID, func(t *testing.T) {
			fixture := newPendingInitFixture(t, false, map[string]string{"default": "ENVIRONMENT_PROFILES=" + definition.Name + "\n"})
			root := fixture.cli.options.RepositoryRoot
			// Installed commands dispatch through the release engine and name
			// their repository root, as activation does.
			fixture.cli.options.DispatcherPath = filepath.Join(root, "bin", "yard-engine")
			fixture.cli.options.Environment = append(fixture.cli.options.Environment, "SUBYARD_REPOSITORY_ROOT="+root)
			state, callsPath, fail := installSyntheticRuntimeOwner(t, root, definition)
			executor := fixture.cli.options.Executor.(installedSettingsExecutor)
			executor.profiles = []string{definition.Name}
			executor.guest = map[string]string{"ENVIRONMENT_PROFILES": definition.Name, "DEV_USER": "dev", "DEV_UID": "1000", "ALLOWS_CODING_TOOLS": "true"}
			fixture.cli.options.Executor = executor
			testkit.WriteFile(t, fail, nil, 0o600)
			program, err := New(fixture.cli.options)
			if err != nil {
				t.Fatal(err)
			}
			request, _ := repairRequest(t, fixture)
			activation := program.profileActivationReconciler(request, definition.Runtime.ActivationID).(*profileRuntimeActivationReconciler)
			if !activation.cli.activation.repair {
				t.Fatal("active release repair did not select the installed view")
			}
			if err := activation.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err == nil {
				t.Fatal("failed activation hook was accepted")
			}
			activated := runtimeOwnerApplies(readRuntimeOwnerCalls(t, callsPath))
			if len(activated) != 1 {
				t.Fatalf("activation applies: %#v", activated)
			}
			if err := os.Remove(fail); err != nil {
				t.Fatal(err)
			}
			if partial, _ := os.ReadFile(state); string(partial) != strings.Repeat("d", 64) {
				t.Fatalf("failed apply did not leave the partial actual: %q", partial)
			}

			// A fresh init repeats the same apply from the new actual and converges.
			fresh, err := New(fixture.cli.options)
			if err != nil {
				t.Fatal(err)
			}
			initRuntime := prepareRuntimeOwnerInit(t, fresh)
			if err := initRuntime.ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); err != nil {
				t.Fatalf("init after failed activation: %v", err)
			}
			applied := runtimeOwnerApplies(readRuntimeOwnerCalls(t, callsPath))
			if len(applied) != 2 {
				t.Fatalf("init applies: %#v", applied)
			}
			before, after := applied[0], applied[1]
			// Allowed differences: operation ID and the actual left by the failure.
			if before.path != after.path || before.digest != after.digest || len(before.arguments) != 3 || len(after.arguments) != 3 {
				t.Fatalf("activation and init use different handlers or protocol:\n%#v\n%#v", before, after)
			}
			if !domain.SafeID(before.arguments[0]) || !domain.SafeID(after.arguments[0]) || before.arguments[0] == after.arguments[0] {
				t.Fatalf("operation IDs: %q %q", before.arguments[0], after.arguments[0])
			}
			if before.arguments[1] != strings.Repeat("a", 64) || after.arguments[1] != strings.Repeat("d", 64) ||
				before.arguments[2] != runtimeOwnerDesired(before, definition.Name) || after.arguments[2] != runtimeOwnerDesired(after, definition.Name) {
				t.Fatalf("assessment arguments: %v %v", before.arguments, after.arguments)
			}
			for _, entry := range before.environment {
				if name, _, _ := strings.Cut(entry, "="); slices.Contains(runtimeOwnerInitOnlyEnvironment, name) {
					t.Fatalf("activation received a command-scope input: %s", name)
				}
			}
			if left, right := runtimeOwnerContractEnvironment(before.environment), runtimeOwnerContractEnvironment(after.environment); !slices.Equal(left, right) {
				t.Fatalf("activation and init environments differ:\n%s", runtimeOwnerEnvironmentDiff(left, right))
			}
			converged, err := initRuntime.CheckStage(context.Background(), ports.ReconcileStageProfileRuntimes)
			if err != nil || !converged {
				t.Fatalf("init did not converge: %v %v", converged, err)
			}

			// Drift after the plan is a stale plan, not a new apply.
			testkit.WriteFile(t, state, []byte(strings.Repeat("e", 64)), 0o600)
			drifted := prepareRuntimeOwnerInit(t, fresh)
			testkit.WriteFile(t, state, []byte(strings.Repeat("f", 64)), 0o600)
			if err := drifted.ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("drift after plan: %v", err)
			}
			if count := len(runtimeOwnerApplies(readRuntimeOwnerCalls(t, callsPath))); count != 2 {
				t.Fatalf("stale plan applied: %d", count)
			}

			// Deselection passes the new desired contract to the same handler.
			testkit.WriteFile(t, state, []byte(after.arguments[2]), 0o600)
			path := filepath.Join(request.ConfigHome, "yards", "default", "config.env")
			content := strings.ReplaceAll(string(mustReadConfigFixture(t, path)), "ENVIRONMENT_PROFILES="+definition.Name+"\n", "ENVIRONMENT_PROFILES=\n")
			testkit.WriteFile(t, path, []byte(content), 0o600)
			deselected, err := New(fixture.cli.options)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepareRuntimeOwnerInit(t, deselected).ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); err != nil {
				t.Fatalf("init after deselection: %v", err)
			}
			applied = runtimeOwnerApplies(readRuntimeOwnerCalls(t, callsPath))
			if runtimeOwnerDesired(after, definition.Name) == strings.Repeat("c", 64) {
				// Desired configuration never selected this owner; nothing changes.
				if len(applied) != 2 {
					t.Fatalf("unselected owner applied again: %#v", applied)
				}
				return
			}
			if len(applied) != 3 || applied[2].path != before.path ||
				!slices.Equal(applied[2].arguments[1:], []string{strings.Repeat("b", 64), strings.Repeat("c", 64)}) {
				t.Fatalf("deselection apply: %#v", applied)
			}
		})
	}
}

// No shipped integration lifecycle owner exists yet. Routing coverage for one
// must be added before such an owner ships.
func TestIntegrationLifecycleOwnersHaveRoutingCoverage(t *testing.T) {
	integrations, err := profile.LoadIntegrations(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	var owners []string
	for _, integration := range integrations {
		if integration.Lifecycle != nil && !integration.Retired {
			owners = append(owners, integration.Name)
		}
	}
	if len(owners) != 0 {
		t.Fatalf("add lifecycle routing coverage for active lifecycle owners: %v", owners)
	}
}

func runtimeOwnerEnvironmentDiff(left, right []string) string {
	var lines []string
	for _, entry := range left {
		if !slices.Contains(right, entry) {
			lines = append(lines, "- "+entry)
		}
	}
	for _, entry := range right {
		if !slices.Contains(left, entry) {
			lines = append(lines, "+ "+entry)
		}
	}
	return strings.Join(lines, "\n")
}
