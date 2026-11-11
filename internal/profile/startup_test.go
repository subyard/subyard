package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

const startingFixture = `printf '%s\n' '{"state":"starting","reason":"transport is starting","hint":"inspect the service, then retry yard start"}'`

func startupFixture(t *testing.T, root, name, body string) {
	t.Helper()
	fixture(t, root, name, "#!/bin/bash\n[ \"$1\" = --startup-check ] || exit 2\n"+body+"\n", Definition{DefaultYards: []string{"default"}, StartupReadiness: "owner.sh"})
}

func startupEnvironment() map[string]string {
	return map[string]string{"SUBYARD_ENGINE_CONTEXT": "1", "SUBYARD_ENGINE_CONTEXT_SCHEMA": "1"}
}

func TestStartupReadinessReadyAndStableDriftDoNotWait(t *testing.T) {
	for _, state := range []string{"ready", "drift"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			startupFixture(t, root, "synthetic", fmt.Sprintf("printf '%%s\\n' '{\"state\":\"%s\"}'", state))
			var output bytes.Buffer
			if err := RunServices(context.Background(), root, []string{"--wait-ready"}, startupEnvironment(), &output, &output); err != nil || output.Len() != 0 {
				t.Fatalf("err=%v output=%q", err, output.String())
			}
		})
	}
}

func TestStartupReadinessWaitsForDelayedProfile(t *testing.T) {
	root := testkit.TempDir(t)
	startupFixture(t, root, "synthetic", fmt.Sprintf(`
printf '%%s\n' "$SUBYARD_STARTUP_REMAINING_MS" >> %[1]q/budgets
if [ -e %[1]q/checked ]; then
  printf '%%s\n' '{"state":"ready"}'
else
  : > %[1]q/checked
  %s
fi`, root, startingFixture))
	definitions, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	environment := startupEnvironment()
	environment[StartupRemainingEnvironment] = "1000"
	if err := waitStartupReadiness(ctx, root, definitions, environment, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	budgets, err := os.ReadFile(filepath.Join(root, "budgets"))
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Fields(string(budgets))
	if len(values) != 2 {
		t.Fatalf("unexpected probe budgets: %q", budgets)
	}
	first, firstErr := strconv.Atoi(values[0])
	second, secondErr := strconv.Atoi(values[1])
	if firstErr != nil || secondErr != nil || first > 1000 || second > first || second < 0 {
		t.Fatalf("startup budget was reset between probes: %q", budgets)
	}
}

func TestStartupReadinessDeadlineRefusesTransientProfile(t *testing.T) {
	root := testkit.TempDir(t)
	startupFixture(t, root, "synthetic", startingFixture)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	err := RunServices(ctx, root, []string{"--wait-ready"}, startupEnvironment(), &output, &output)
	var refusal *StartupRefusal
	if !errors.As(err, &refusal) || !errors.Is(err, context.DeadlineExceeded) ||
		refusal.Profile != "synthetic" || refusal.Reason != "transport is starting" ||
		refusal.Hint != "inspect the service, then retry yard start" || output.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, output.String())
	}
}

func TestStartupExpiredAnchorInspectsOnceWithoutAnotherPollingWindow(t *testing.T) {
	for _, state := range []string{"ready", "drift", "starting"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			startupFixture(t, root, "synthetic", fmt.Sprintf(`
[ "$SUBYARD_STARTUP_REMAINING_MS" = 0 ] || exit 19
printf x >> %q/calls
printf '%%s\n' '{"state":"%s","reason":"service is reconnecting","hint":"run yard migrate to repair"}'
`, root, state))
			environment := startupEnvironment()
			environment[StartupRemainingEnvironment] = "0"
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := RunServices(ctx, root, []string{"--wait-ready"}, environment, &bytes.Buffer{}, &bytes.Buffer{})
			if state == "starting" {
				var refusal *StartupRefusal
				if !errors.As(err, &refusal) || !errors.Is(err, context.DeadlineExceeded) || refusal.Reason != "service is reconnecting" || !strings.Contains(refusal.Hint, "yard migrate") {
					t.Fatalf("expired startup lost its refusal: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil || string(calls) != "x" {
				t.Fatalf("expired budget polled again: %q %v", calls, err)
			}
		})
	}
}

func TestStartupSliverBudgetDoesNotKillReadyObservation(t *testing.T) {
	root := testkit.TempDir(t)
	startupFixture(t, root, "synthetic", `sleep 0.02
printf '%s\n' '{"state":"ready"}'`)
	environment := startupEnvironment()
	environment[StartupRemainingEnvironment] = "1"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := RunServices(ctx, root, []string{"--wait-ready"}, environment, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("polling deadline killed a ready observation: %v", err)
	}
}

func TestStartupReadinessDeadlineCollectsEveryProfileInStableOrder(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"zeta", "alpha"} {
		startupFixture(t, root, name, startingFixture)
	}
	// A blocked observer must not prevent independent owners from being checked.
	startupFixture(t, root, "hung", "sleep 60")
	definitions, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = waitStartupReadiness(ctx, root, definitions, startupEnvironment(), time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline refusal, got %v", err)
	}
	want := "profile alpha startup: transport is starting; hint: inspect the service, then retry yard start; " +
		"profile hung startup: readiness check unavailable; hint: run yard migrate to repair; inspect the profile service; " +
		"profile zeta startup: transport is starting; hint: inspect the service, then retry yard start"
	if err.Error() != want {
		t.Fatalf("refusals:\n%s\nwant:\n%s", err, want)
	}
}

func TestStartupReadinessSelectionAndOptIn(t *testing.T) {
	root := testkit.TempDir(t)
	startupFixture(t, root, "unselected", "exit 99")
	fixture(t, root, "legacy", "#!/bin/bash\nexit 99\n", Definition{DefaultYards: []string{"default"}})
	fixture(t, root, "disabled", "#!/bin/bash\nexit 99\n", Definition{Disabled: true, StartupReadiness: "owner.sh"})
	environment := startupEnvironment()
	environment["ENVIRONMENT_PROFILES"] = "legacy"
	if err := RunServices(context.Background(), root, []string{"--wait-ready"}, environment, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupReadinessObservesAppliedDisabledProfile(t *testing.T) {
	root := testkit.TempDir(t)
	fixture(t, root, "retained", "#!/bin/sh\nprintf '%s\\n' '{\"state\":\"starting\",\"reason\":\"service is activating\",\"hint\":\"inspect service\"}'\n", Definition{Disabled: true, StartupReadiness: "owner.sh"})
	environment := startupEnvironment()
	environment["ENVIRONMENT_PROFILES"] = "retained"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := RunServices(ctx, root, []string{"--wait-ready"}, environment, &bytes.Buffer{}, &bytes.Buffer{})
	var refusal *StartupRefusal
	if !errors.As(err, &refusal) || refusal.Profile != "retained" || refusal.Reason != "service is activating" {
		t.Fatalf("applied disabled owner was skipped: %v", err)
	}
}

func TestStartupReadinessRejectsInvalidOrUnavailableObserversWithoutNativeOutput(t *testing.T) {
	for _, body := range []string{
		`printf '%s\n' '{"state":"starting"}'`,
		`printf '%s\n' '{"state":"unknown"}'`,
		`printf '%s\n' '{"state":"ready","private":"canary"}'`,
		`printf '%s\n' '{"state":"ready"} {"state":"ready"}'`,
		`printf '%s\n' '{"state":"starting","reason":"private\ncanary","hint":"inspect service"}'`,
		`printf 'private-canary\n' >&2; exit 1`,
		`printf '%02000d' 0`,
	} {
		t.Run(body, func(t *testing.T) {
			root := testkit.TempDir(t)
			startupFixture(t, root, "synthetic", body)
			var output bytes.Buffer
			err := RunServices(context.Background(), root, []string{"--wait-ready"}, startupEnvironment(), &output, &output)
			if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "canary") {
				t.Fatalf("err=%v output=%q", err, output.String())
			}
		})
	}
}

func TestStartupReadinessDeclarationValidation(t *testing.T) {
	for _, data := range []string{
		`{"schema_version":1,"startup_readiness":"../escape"}`,
		`{"schema_version":1,"startup_readiness":"/absolute"}`,
		`{"schema_version":1,"startup_readiness":true}`,
		`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"cleanup.sh","startup_readiness":"owner.sh"}`,
	} {
		if _, err := Decode("synthetic", []byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	root := testkit.TempDir(t)
	path := fixture(t, root, "synthetic", "", Definition{StartupReadiness: "missing.sh"})
	if _, err := Load(root); err == nil {
		t.Fatal("accepted missing startup executable")
	}
	testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "missing.sh"), []byte("#!/bin/sh\n"), 0o600)
	if _, err := Load(root); err == nil {
		t.Fatal("accepted non-executable startup handler")
	}
}
