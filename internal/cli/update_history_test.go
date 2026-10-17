package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type protectedOwnerInputCase struct {
	name, override string
	mounts, agents *string
	blocked        bool
}

func protectedOwnerInputCases() []protectedOwnerInputCase {
	explicitMounts, emptyMounts, agents := "explicit:/mnt/explicit:ro:0700", "", "codex"
	derivedMounts := "derived:/mnt/derived:ro:0700"
	return []protectedOwnerInputCase{
		{name: "derived settings"},
		{name: "canonical override", override: "HOST_MOUNTS=" + explicitMounts, mounts: &explicitMounts},
		{name: "empty override", override: "HOST_MOUNTS=", mounts: &emptyMounts},
		{name: "legacy override", override: "AGENTS=" + agents, agents: &agents},
		{name: "unsafe explicit override", override: "HOST_MOUNTS=derived:/mnt/derived:ro:0700", mounts: &derivedMounts, blocked: true},
	}
}

func TestReadinessAndMigrateProtectedOwnerReceiveOnlyExplicitSettingOverrides(t *testing.T) {
	for _, entrypoint := range []string{"readiness", "migrate"} {
		t.Run(entrypoint, func(t *testing.T) {
			for _, test := range protectedOwnerInputCases() {
				t.Run(test.name, func(t *testing.T) {
					root, environment, runtimeRoot := updateReleaseFixture(t)
					writeCLIFile(t, filepath.Join(root, "config", "host.env"), "HOST_MOUNTS=derived:/mnt/derived:ro:0700\n", 0o600)
					capture := filepath.Join(root, "protected-owner-request.json")
					environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "UPDATE_OWNER_CAPTURE="+capture, "SUBYARD_NO_AUDIT=1")
					if test.override != "" {
						environment = append(environment, test.override)
					}
					journalPath := installCompletedUpdateOwnerFixture(t, environment, runtimeRoot)
					before, err := os.ReadFile(journalPath)
					if err != nil {
						t.Fatal(err)
					}
					var stdout, stderr bytes.Buffer
					program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdout: &stdout, Stderr: &stderr})
					if err != nil {
						t.Fatal(err)
					}
					loaded, err := program.loadContext("default")
					if err != nil {
						t.Fatal(err)
					}
					// Readiness can follow loading a yard. Its effective settings must never
					// replace the original command inputs of any protected operation.
					program.env = loaded.Environment
					if entrypoint == "readiness" {
						outcome, err := program.inspectMutationGate(context.Background(), "default")
						if err != nil || (outcome != nil) != test.blocked {
							t.Fatalf("gate outcome=%#v err=%v", outcome, err)
						}
					} else {
						prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: program, Loaded: loaded, Arguments: []string{"--check", "--json"}, Plan: domain.OperationPlan{Confirmed: true}}
						if err := prepared.prepareCurrentMigration(context.Background(), nil); err != nil {
							t.Fatal(err)
						}
						defer prepared.Close()
						orchestrator := &application.Orchestrator{Clock: wallClock{}}
						prepared.Plan = confirmedAdapterFixture(t, orchestrator, prepared.Plan)
						result, err := prepared.execute(context.Background(), orchestrator, nil)
						if err != nil {
							t.Fatalf("current migration: %v (%#v)", err, result)
						}
						var report struct{ Outcome releasetransition.Outcome }
						if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || (report.Outcome.Status == releasetransition.StatusOperatorActionRequired) != test.blocked || report.Outcome.ReachedGoal == test.blocked {
							t.Fatalf("migrate report=%s err=%v", stdout.String(), err)
						}
					}
					payload, err := os.ReadFile(capture)
					if err != nil {
						t.Fatal(err)
					}
					var observed struct {
						Mounts, Agents *string
						Request        releasetransition.ProcessRequest
					}
					if err := json.Unmarshal(payload, &observed); err != nil {
						t.Fatal(err)
					}
					want, _ := json.Marshal(struct{ Mounts, Agents *string }{test.mounts, test.agents})
					got, _ := json.Marshal(struct{ Mounts, Agents *string }{observed.Mounts, observed.Agents})
					wantIDs := []string{"DEV_SUDO", "DEV_UID", "DEV_USER", "FORWARD_SSH_AGENT", "HOST_BASE", "RESTRICTED_DISK_PATHS", "SHIFT_MODE", "SSH_PORT", "STORAGE_PATH"}
					if strings.HasPrefix(test.override, "HOST_MOUNTS=") {
						wantIDs = append(wantIDs, "HOST_MOUNTS")
						slices.Sort(wantIDs)
					}
					if !bytes.Equal(got, want) || !slices.Equal(observed.Request.InheritedSettingIDs, wantIDs) || observed.Request.ConfigHome != loaded.Context.Paths.ConfigHome || observed.Request.RuntimeRoot != runtimeRoot || observed.Request.Yard != "default" {
						t.Fatalf("wrong owner inputs: %s", payload)
					}
					after, err := os.ReadFile(journalPath)
					if err != nil || !bytes.Equal(before, after) {
						t.Fatalf("inspection changed completed journal: %v", err)
					}
				})
			}
		})
	}
}

func TestUpdateProtectedOwnerReceivesOnlyExplicitSettingOverrides(t *testing.T) {
	for _, test := range protectedOwnerInputCases() {
		t.Run(test.name, func(t *testing.T) {
			root, environment, runtimeRoot := updateReleaseFixture(t)
			writeCLIFile(t, filepath.Join(root, "config", "host.env"), "HOST_MOUNTS=derived:/mnt/derived:ro:0700\n", 0o600)
			capture := filepath.Join(root, "protected-owner-request.json")
			environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "UPDATE_OWNER_CAPTURE="+capture)
			if test.override != "" {
				environment = append(environment, test.override)
			}
			journalPath := installCompletedUpdateOwnerFixture(t, environment, runtimeRoot)
			before, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment,
				Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			if test.mounts == nil && loaded.Environment["HOST_MOUNTS"] == "" {
				t.Fatal("fixture must derive the invoking yard's host mounts")
			}
			execution, err := program.prepareRelease(context.Background(), loaded,
				[]string{"--check", "--json", "--version", "1.2.3"})
			if err != nil {
				if test.blocked && strings.Contains(err.Error(), "permits only exact target 1.2.2-aaaaaaaaaaaa") {
					return
				}
				t.Fatalf("completed protected owner prevented newer release inspection: %v", err)
			}
			defer execution.Close()
			if test.blocked {
				t.Fatal("unsafe explicit override bypassed the protected target guard")
			}
			if err := execution.prepared.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			var inspection releasetransition.Inspection
			if err := json.Unmarshal(stdout.Bytes(), &inspection); err != nil || inspection.Outcome == nil ||
				!strings.HasPrefix(string(inspection.Outcome.Target), "1.2.3-") ||
				execution.yard != loaded.Context.YardName {
				t.Fatalf("update changed its target or yard scope: %s, %v", stdout.String(), err)
			}
			payload, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var observed struct {
				Mounts, Agents *string
				Request        releasetransition.ProcessRequest
			}
			if err := json.Unmarshal(payload, &observed); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(struct{ Mounts, Agents *string }{test.mounts, test.agents})
			got, _ := json.Marshal(struct{ Mounts, Agents *string }{observed.Mounts, observed.Agents})
			if !bytes.Equal(got, want) || observed.Request.ConfigHome != loaded.Context.Paths.ConfigHome ||
				observed.Request.Yard != loaded.Context.YardName || observed.Request.RuntimeRoot != runtimeRoot ||
				!slices.Equal(observed.Request.InheritedSettingIDs, program.releaseTransitionInheritedSettingIDs()) {
				t.Fatalf("owner received different explicit settings or scope: %s", payload)
			}
			after, err := os.ReadFile(journalPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read-only inspection changed the completed journal: %v", err)
			}
		})
	}
}

func installCompletedUpdateOwnerFixture(t *testing.T, environment []string, runtimeRoot string) string {
	t.Helper()
	release := releasetransition.ReleaseID("1.2.2-aaaaaaaaaaaa")
	destination := filepath.Join(runtimeRoot, "releases", string(release))
	for _, directory := range []string{"bin", "config"} {
		if err := os.MkdirAll(filepath.Join(destination, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Model a historical owner that mistakes an invoking yard's resolved mounts
	// for command overrides. Capture only synthetic settings and the protocol scope.
	engine := `#!/bin/sh
set -eu
case "${1:-}" in
--version) printf 'yard-engine 1.2.2\n' ;;
_release-transition)
request=$(cat)
printf '%s' "$request" | jq '{Mounts: env.HOST_MOUNTS, Agents: env.AGENTS, Request: .}' > "$UPDATE_OWNER_CAPTURE"
config_home=$(printf '%s' "$request" | jq -r .configHome)
transaction=$(jq -r .transaction "$config_home/release-transition/v2/journal.json")
target=$(printf '%s' "$request" | jq -r .target)
jq -n --arg target "$target" --arg transaction "$transaction" --arg mounts "${HOST_MOUNTS:-}" '{
  schemaVersion: 1, activationReconciliationOwned: true,
  inspection: {
    plan: "plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    assessment: {action: "release.transition.v2", effect: "mutation", changed: false,
      impacts: ["local-metadata", "persistent-data", "yard-runtime"], recovery: "reversible"},
    outcome: {status: "ready", reachedGoal: true, active: $target, previous: "release-old",
      target: $target, code: "ready", message: "verified", transaction: $transaction}
  }
} | if $mounts == "derived:/mnt/derived:ro:0700" then
  .inspection.assessment.changed = true |
  .inspection.assessment.consequences = ["repair route consumers"] |
  .inspection.blockers = [{code: "precondition-blocked", resource: "activation.fixture-route-consumers",
    message: "inherited invoking-yard mounts", retry: "run yard update --check"}] |
  .inspection.outcome += {status: "operator-action-required", reachedGoal: false,
    code: "precondition-blocked", message: "inherited invoking-yard mounts", retry: "run yard update --check"}
  else . end'
;;
*) exit 64 ;;
esac
`
	registry := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`)
	writeCLIFile(t, filepath.Join(destination, "bin", "yard-engine"), engine, 0o700)
	writeCLIFile(t, filepath.Join(destination, "config", "release-transition.json"), string(registry), 0o600)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		sha256.Sum256([]byte(engine)), sha256.Sum256(registry))
	writeCLIFile(t, filepath.Join(destination, "runtime-files.sha256"), manifest, 0o600)
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome:   configHome,
		Releases:     releasetransition.ReleasePair{From: "release-old", Target: release},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		RegistryPayload:     registry,
		ArtifactDigest:      releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256([]byte(manifest)))),
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := transition.Inspect(context.Background(), releasetransition.Goal{
		Target: release, Direction: releasetransition.DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{
		Plan: inspection.Plan, Authorization: "fixture-authorization"})
	if err != nil || outcome.Status != releasetransition.StatusReady {
		t.Fatalf("complete owner fixture: %#v, %v", outcome, err)
	}
	return filepath.Join(configHome, "release-transition", "v2", "journal.json")
}

func TestUpdateHistoryRecordsActivationRollbackFailureAndDecline(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	home := environmentValue(environment, "SUBYARD_HOME")
	var lastStderr string
	run := func(operationID string, prompt *testkit.Prompt, arguments ...string) int {
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: arguments,
			Environment: append(environment, "SUBYARD_OPERATION_ID="+operationID),
			WorkingDir:  root, Prompt: prompt, Config: &recordingConfigApplier{},
			Stdout: &bytes.Buffer{}, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		code := program.Run(context.Background())
		lastStderr = stderr.String()
		return code
	}
	if code := run("op-history-activate", nil,
		"update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot); code != 0 {
		t.Fatalf("activation code=%d", code)
	}
	// Install the fixture's verified retained pair for the next independent rollback attempt.
	// The history lives under SUBYARD_HOME and must survive changes to the runtime links.
	prepareCLIReleaseLinks(t, runtimeRoot, true)
	if code := run("op-history-rollback", nil,
		"update", "--yes", "--rollback", "--runtime-root", runtimeRoot); code != 0 {
		t.Fatalf("rollback code=%d stderr=%q", code, lastStderr)
	}
	privatePath := filepath.Join(root, "synthetic-private-detail", "missing-runtime")
	if code := run("op-history-failure", nil,
		"update", "--rollback", "--runtime-root", privatePath); code != 1 {
		t.Fatalf("failure code=%d", code)
	}
	if code := run("op-history-declined", &testkit.Prompt{Answers: []bool{false}},
		"update", "--version", "1.2.3", "--runtime-root", runtimeRoot); code != 1 {
		t.Fatalf("decline code=%d", code)
	}
	records, err := (audit.UpdateHistory{Home: home}).Read(10)
	if err != nil || len(records) != 4 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	byID := make(map[string]audit.UpdateRecord, len(records))
	for _, record := range records {
		byID[record.OperationID] = record
	}
	if record := byID["op-history-activate"]; record.Status != "success" ||
		record.Direction != "activate" || record.SourceRelease != "release-old" ||
		record.SourceVersion != "" || record.TargetVersion != "1.2.3" {
		t.Fatalf("activation record=%#v", record)
	}
	if record := byID["op-history-rollback"]; record.Status != "success" ||
		record.Direction != "rollback" || record.SourceRelease != "current-a" ||
		record.TargetRelease != "previous-b" {
		t.Fatalf("rollback record=%#v", record)
	}
	if record := byID["op-history-failure"]; record.Status != "failure" ||
		record.ErrorCode != "preparation_failed" || len(record.Events) != 2 ||
		record.Events[0].Phase != "prepare" {
		t.Fatalf("failure record=%#v", record)
	}
	if record := byID["op-history-declined"]; record.Status != "declined" ||
		len(record.Events) != 3 || record.Events[0].Phase != "prepare" ||
		record.Events[1].Phase != "confirmation" || record.TargetVersion != "1.2.3" {
		t.Fatalf("declined record=%#v", record)
	}
	failure := byID["op-history-failure"]
	payload, err := os.ReadFile(filepath.Join(home, "logs", "updates", failure.AttemptID+".json"))
	if err != nil || bytes.Contains(payload, []byte("synthetic-private-detail")) {
		t.Fatalf("history retained private error detail: payload=%q err=%v", payload, err)
	}
}

func TestPostVerificationPreparationFailureKeepsVerifiedTarget(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	environment = append(environment,
		"UPDATE_BLOCK_INSPECTION=1",
		"SUBYARD_OPERATION_ID=op-verified-preparation-failure",
	)
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("blocked preparation code=%d", code)
	}
	records, err := (audit.UpdateHistory{Home: environmentValue(environment, "SUBYARD_HOME")}).Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "failure" ||
		records[0].TargetVersion != "1.2.3" || records[0].TargetRelease != "1.2.3-f16d05ec6b29" {
		t.Fatalf("verified preparation records=%#v err=%v", records, err)
	}
}

func TestPostVerificationPreparationCancellationKeepsTargetAndStatus(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	environment = append(environment,
		"UPDATE_CANCEL_INSPECTION=1",
		"SUBYARD_OPERATION_ID=op-verified-preparation-cancelled",
	)
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if code := program.Run(ctx); code != 1 {
		t.Fatalf("cancelled verified preparation code=%d", code)
	}
	records, err := (audit.UpdateHistory{Home: environmentValue(environment, "SUBYARD_HOME")}).Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "interrupted" ||
		records[0].ErrorCode != "context_cancelled" || records[0].TargetVersion != "1.2.3" {
		t.Fatalf("verified cancellation records=%#v err=%v", records, err)
	}
}

func TestPostVerificationRollbackFailureKeepsVerifiedTarget(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	prepareCLIReleaseLinks(t, runtimeRoot, true)
	releaseRoot := filepath.Join(runtimeRoot, "releases", "previous-b")
	enginePath := filepath.Join(releaseRoot, "bin", "yard-engine")
	engine := []byte("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.3\\n' ;; _release-transition) cat >/dev/null; printf '{}\\n' ;; *) exit 64 ;; esac\n")
	writeCLIFile(t, enginePath, string(engine), 0o700)
	manifestPath := filepath.Join(releaseRoot, "runtime-files.sha256")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(engine)
	lines := strings.Split(strings.TrimSuffix(string(manifest), "\n"), "\n")
	for index, line := range lines {
		if strings.HasSuffix(line, "  ./bin/yard-engine") {
			lines[index] = fmt.Sprintf("%x  ./bin/yard-engine", digest)
		}
	}
	writeCLIFile(t, manifestPath, strings.Join(lines, "\n")+"\n", 0o600)
	environment = append(environment, "SUBYARD_OPERATION_ID=op-verified-rollback-failure")
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--yes", "--rollback", "--runtime-root", runtimeRoot},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("blocked rollback preparation code=%d", code)
	}
	records, err := (audit.UpdateHistory{Home: environmentValue(environment, "SUBYARD_HOME")}).Read(10)
	if err != nil || len(records) != 1 || records[0].TargetRelease != "previous-b" ||
		records[0].TargetVersion != "1.2.3" {
		t.Fatalf("verified rollback records=%#v err=%v", records, err)
	}
}

func TestCancelledUpdatePreparationRecordsInterrupted(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		Environment: append(environment, "SUBYARD_OPERATION_ID=op-prepare-cancelled"), WorkingDir: root,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(ctx); code != 1 {
		t.Fatalf("cancelled preparation code=%d", code)
	}
	records, err := (audit.UpdateHistory{Home: environmentValue(environment, "SUBYARD_HOME")}).Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "interrupted" ||
		records[0].ErrorCode != "context_cancelled" || records[0].Events[0].Phase != "prepare" {
		t.Fatalf("cancelled preparation records=%#v err=%v", records, err)
	}
}

func TestCancelledUpdateConfirmationRecordsInterrupted(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		Environment: append(environment, "SUBYARD_OPERATION_ID=op-confirm-cancelled"), WorkingDir: root,
		Prompt: &testkit.Prompt{Err: context.Canceled}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("cancelled confirmation code=%d", code)
	}
	records, err := (audit.UpdateHistory{Home: environmentValue(environment, "SUBYARD_HOME")}).Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "interrupted" ||
		records[0].ErrorCode != "context_cancelled" || records[0].Events[1].Phase != "confirmation" ||
		records[0].TargetVersion != "1.2.3" {
		t.Fatalf("cancelled confirmation records=%#v err=%v", records, err)
	}
}

func TestUpdateCheckAndHelpDoNotCreateHistory(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	for _, arguments := range [][]string{
		{"update", "--check", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		{"update", "--help"},
	} {
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments,
			Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("arguments=%v code=%d", arguments, code)
		}
	}
	records, err := (audit.UpdateHistory{Home: environmentValue(environment, "SUBYARD_HOME")}).Read(10)
	if err != nil || len(records) != 0 {
		t.Fatalf("check/help history=%#v err=%v", records, err)
	}
}

func TestUpdateHistoryRecordsInterruptedExecutionPhase(t *testing.T) {
	home := t.TempDir()
	history := audit.UpdateHistory{Home: home}
	recorder, err := history.Begin(audit.UpdateRecord{
		OperationID: "op-interrupted", Direction: "activate",
		TargetRelease: "1.2.3-aaaaaaaaaaaa", TargetVersion: "1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	program := &CLI{env: map[string]string{"SUBYARD_HOME": home}}
	execution := &releaseExecution{history: recorder, phase: "execute"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := program.finishUpdateHistory(ctx, execution, domain.AdapterResult{}, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("finish error=%v", err)
	}
	records, err := history.Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "interrupted" ||
		records[0].ErrorCode != "context_cancelled" || records[0].Events[2].Phase != "execute" ||
		records[0].TargetVersion != "1.2.3" {
		t.Fatalf("interrupted records=%#v err=%v", records, err)
	}
}

func TestSuccessfulUpdateHistoryIgnoresLateContextCancellation(t *testing.T) {
	home := t.TempDir()
	history := audit.UpdateHistory{Home: home}
	recorder, err := history.Begin(audit.UpdateRecord{OperationID: "op-late-cancel", Direction: "activate"})
	if err != nil {
		t.Fatal(err)
	}
	program := &CLI{env: map[string]string{"SUBYARD_HOME": home}}
	execution := &releaseExecution{history: recorder, phase: "execute"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := program.finishUpdateHistory(ctx, execution,
		domain.AdapterResult{Status: "ok"}, nil); err != nil {
		t.Fatal(err)
	}
	records, err := history.Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "success" {
		t.Fatalf("late cancellation records=%#v err=%v", records, err)
	}
}

func TestUpdateHistoryWriteFailureDoesNotSkipConfigRefresh(t *testing.T) {
	home := t.TempDir()
	history := audit.UpdateHistory{Home: home}
	recorder, err := history.Begin(audit.UpdateRecord{
		OperationID: "op-history-write-failure", Direction: "activate",
		TargetRelease: "1.2.3-aaaaaaaaaaaa", TargetVersion: "1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "logs", "updates")
	if err := os.WriteFile(filepath.Join(directory, "corrupt.json"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	applier := &recordingConfigApplier{}
	program := &CLI{env: map[string]string{"SUBYARD_HOME": home}, baseEnv: map[string]string{}, options: Options{Config: applier}}
	execution := &releaseExecution{
		history: recorder, phase: "execute", yard: "default",
		prepared: releaseruntime.Prepared{TargetRelease: "1.2.3-aaaaaaaaaaaa", TargetVersion: "1.2.3"},
	}
	runErr := program.refreshReleaseConfig(context.Background(), execution)
	if runErr == nil || len(applier.yards) != 1 || applier.yards[0] != "default" {
		t.Fatalf("refresh error=%v applied=%v", runErr, applier.yards)
	}
	if err := program.finishUpdateHistory(context.Background(), execution,
		domain.AdapterResult{Status: "ok"}, runErr); err == nil {
		t.Fatal("expected persistent history failure")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var durable audit.UpdateRecord
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "attempt-") {
			payload, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err := json.Unmarshal(payload, &durable); err != nil {
				t.Fatal(err)
			}
		}
	}
	if durable.ErrorCode != "history_write_failed" || durable.Events[len(durable.Events)-2].Phase != "refresh" {
		t.Fatalf("durable history failure=%#v", durable)
	}
}

func TestHostLogViewersWorkWithoutYardConfigOrIncus(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_HOME")
	recorder, err := (audit.UpdateHistory{Home: home}).Begin(audit.UpdateRecord{
		OperationID: "op-view-update", Direction: "activate",
		SourceVersion: "1.0.0", TargetVersion: "1.1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Finish("execute", "success", "", "1.1.0-bbbbbbbbbbbb", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := (audit.UpdateHistory{Home: home}).Begin(audit.UpdateRecord{
		OperationID: "op-view-unfinished", Direction: "rollback",
	}); err != nil {
		t.Fatal(err)
	}
	if err := audit.WriteInvocation(audit.Invocation{Home: home, Command: "status", WorkingDir: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "config", "host.env")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, flag, marker string
	}{
		{name: "updates", flag: "--updates", marker: "op-view-update"},
		{name: "audit", flag: "--audit", marker: " -- status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard",
				Arguments:   []string{"logs", test.flag, "-n", "10"},
				Environment: append(environment, "SUBYARD_NO_AUDIT=1"), WorkingDir: root,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 ||
				!strings.Contains(stdout.String(), test.marker) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if test.name == "updates" && (!strings.Contains(stdout.String(), "op-view-unfinished") ||
				!strings.Contains(stdout.String(), "status=unfinished")) {
				t.Fatalf("unfinished attempt missing: stdout=%q", stdout.String())
			}
		})
	}
}

func TestExplicitLocalOwnerUpdateLogViewerUsesHostHistory(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_HOME")
	recorder, err := (audit.UpdateHistory{Home: home}).Begin(audit.UpdateRecord{
		OperationID: "op-owner-view", Direction: "activate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Finish("execute", "failure", "execution_failed", "release-target", ""); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "default", "logs", "--updates", "-n", "1"},
		Environment: append(environment, "SUBYARD_NO_AUDIT=1"), WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		!strings.Contains(stdout.String(), "op-owner-view") {
		t.Fatalf("owner viewer code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestExplicitRemoteUpdateLogViewerUsesForwardRoute(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	configHome := filepath.Dir(stateDirectory)
	if err := os.MkdirAll(filepath.Join(configHome, "yards", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", "remote", "config.env"),
		"ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\nSSH_PORT=4444\n", 0o600)
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	sshLog := filepath.Join(root, "remote-logs-ssh.log")
	writeCLIFile(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
`+trustedSSHMock(t)+`
printf '%s\n' "$@" >"$SUBYARD_TEST_SSH_LOG"
`, 0o700)
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	environment = append(environment,
		"PATH="+os.Getenv("PATH"),
		"SUBYARD_TEST_SSH_LOG="+sshLog,
		"SUBYARD_OPERATION_ID=remote-update-logs",
	)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "remote", "logs", "--updates", "-n", "3"},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("remote update logs code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	forwarded, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"owner.example", "inner", "logs", "--updates", "-n", "3"} {
		if !strings.Contains(string(forwarded), expected) {
			t.Fatalf("remote forwarding omitted %q: %s", expected, forwarded)
		}
	}
}
