package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Exercise the shipped activation handler through the same generic adapter used
// by update/rollback. Only the physical Incus leaf is replaced here; Docker
// identity/receipt semantics have separate profile-owned tests and a VM fixture.
func TestOpenLITActivationUpdateRollbackAndStaleAssessment(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := profile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var definition profile.Definition
	for _, candidate := range definitions {
		if candidate.Name == "openlit" {
			definition = candidate
		}
	}
	if definition.Runtime == nil || definition.Runtime.ActivationID != "openlit-runtime" {
		t.Fatal("OpenLIT must participate in runtime activation")
	}
	tmp := testkit.TempDir(t)
	actualPath := filepath.Join(tmp, "actual")
	desiredPath := filepath.Join(tmp, "desired")
	calls := filepath.Join(tmp, "calls")
	old := strings.Repeat("a", 64)
	updated := strings.Repeat("b", 64)
	testkit.WriteFile(t, actualPath, []byte(old), 0o600)
	testkit.WriteFile(t, desiredPath, []byte(updated), 0o600)
	testkit.WriteFile(t, filepath.Join(tmp, "incus"), []byte(`#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = list ]; then echo RUNNING; exit; fi
[ "$1" = exec ] || exit 1
tar -tf - >/dev/null
while [ "$1" != subyard ]; do shift; done
shift
[ "$2" = 1 ] && [ "$3" = dev ] && [ "$4" = 'claude codex' ]
actual="$(cat "$FIXTURE_ACTUAL")"
desired="$(cat "$FIXTURE_DESIRED")"
if [ "$1" = refresh ]; then
  [ "$#" = 6 ] && [ "$5" = "$actual" ] && [ "$6" = "$desired" ]
  printf '%s\n' refresh >> "$FIXTURE_CALLS"
  printf '%s' "$desired" > "$FIXTURE_ACTUAL"
  actual="$desired"
else
  [ "$1" = observe ] && [ "$#" = 4 ]
fi
state=stale
[ "$actual" != "$desired" ] || state=current
printf '{"state":"%s","actual":"%s","desired":"%s"}\n' "$state" "$actual" "$desired"
`), 0o700)
	environment := map[string]string{
		"PATH":                   tmp + ":" + os.Getenv("PATH"),
		"SUBYARD_ENGINE_CONTEXT": "1", "SUBYARD_ENGINE_CONTEXT_SCHEMA": "1",
		"SUBYARD_OPERATOR_HOME": tmp, "SUBYARD_CONFIG_DIR": filepath.Join(root, "config"),
		"SUBYARD_CONFIG_HOME": tmp, "SUBYARD_HOME": tmp, "STORAGE_PATH": tmp,
		"HOST_BASE": tmp, "RESTRICTED_DISK_PATHS": tmp, "ACCESS_KIND": "local", "YARD_KIND": "container",
		"YARD_INSTANCE_NAME": "yard-fixture", "INCUS_PROJECT": "fixture", "INCUS_BRIDGE": "fixture",
		"SSH_HOST": "127.0.0.1", "DEV_USER": "dev", "DEV_UID": "1000", "DEV_SUDO": "0",
		"FORWARD_SSH_AGENT": "0", "NESTED_E2E_VMS": "0", "ENVIRONMENT_PROFILES": "openlit",
		"CODING_TOOL_INTEGRATIONS": "claude codex", "FIXTURE_ACTUAL": actualPath, "FIXTURE_DESIRED": desiredPath,
		"AGENT_claude_CONFIG": filepath.Join(root, "config/agents/claude/settings.json"), "AGENT_claude_CONFIG_DEST": ".claude/settings.json",
		"AGENT_codex_CONFIG": filepath.Join(root, "config/agents/codex/config.toml"), "AGENT_codex_CONFIG_DEST": ".codex/config.toml",
		"FIXTURE_CALLS": calls,
	}
	runner := reconcileruntime.Runtime{RepositoryRoot: root, Profiles: []profile.Definition{definition},
		Yard:  domain.Context{IncusProject: "fixture", YardInstanceName: "yard-fixture"},
		Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard-fixture": {Status: "running"}}}}
	for key, value := range environment {
		runner.Environment = append(runner.Environment, key+"="+value)
	}
	observe := func(want ports.RuntimeState, actual, desired string) {
		t.Helper()
		observations, err := runner.ObserveProfileRuntimes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		value := observations["openlit-runtime"]
		if value.State != want || value.Actual != actual || value.Desired != desired {
			t.Fatalf("observation = %#v", value)
		}
	}
	observe(ports.RuntimeStateStale, old, updated)
	if err := runner.ApplyProfileRuntime(context.Background(), "openlit-runtime", "op-update", old, updated); err != nil {
		t.Fatal(err)
	}
	observe(ports.RuntimeStateCurrent, updated, updated)
	testkit.WriteFile(t, desiredPath, []byte(old), 0o600)
	observe(ports.RuntimeStateStale, updated, old)
	if err := runner.ApplyProfileRuntime(context.Background(), "openlit-runtime", "op-rollback", updated, old); err != nil {
		t.Fatal(err)
	}
	observe(ports.RuntimeStateCurrent, old, old)
	testkit.WriteFile(t, desiredPath, []byte(updated), 0o600)
	testkit.WriteFile(t, actualPath, []byte(strings.Repeat("c", 64)), 0o600)
	if err := runner.ApplyProfileRuntime(context.Background(), "openlit-runtime", "op-stale", old, updated); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed actual state error = %v", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "refresh\nrefresh\n" {
		t.Fatalf("refresh calls = %q, %v", data, err)
	}
	pin, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	runner.RepositoryRoot = fmt.Sprintf("/proc/self/fd/%d", pin.Fd())
	pinned, err := profile.Load(runner.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range pinned {
		if candidate.Name == "openlit" {
			runner.Profiles = []profile.Definition{candidate}
		}
	}
	runner.Environment = nil
	for key, value := range environment {
		runner.Environment = append(runner.Environment, key+"="+strings.ReplaceAll(value, root, runner.RepositoryRoot))
	}
	observe(ports.RuntimeStateStale, strings.Repeat("c", 64), updated)
	runner.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard-fixture": {Status: "stopped"}}}
	observe(ports.RuntimeStateDeferred, "", "")
	runner.Incus = &testkit.Incus{}
	observe(ports.RuntimeStateAbsent, "", "")
}
