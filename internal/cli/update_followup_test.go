package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Exercise real protected journals and process boundaries. The synthetic owner
// keeps the source transaction's selected scope for its entire process, then
// observes the second yard in a fresh inspection after completion.
func runUpdateFollowupOwnerProcess() {
	var request releasetransition.ProcessRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		panic(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		panic(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		panic(err)
	}
	allLocal := snapshot.Exists
	if snapshot.Exists {
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			panic(err)
		}
		allLocal = journal.Checkpoint == releasetransition.JournalComplete || journal.Releases.From == journal.Releases.Target
	}
	names := []string{"default"}
	if allLocal {
		names = append(names, "secondary")
	}
	var actors []releasetransition.V2ActivationReconciler
	for _, name := range names {
		actors = append(actors, &updateFollowupYard{home: request.ConfigHome, name: name})
	}
	response, err := executeReleaseTransitionRequest(context.Background(), os.Getenv("UPDATE_FOLLOWUP_REPOSITORY"), request,
		func(_ releasetransition.PlanToken, grant releasetransition.Authorization) bool { return grant != "" },
		actors, releaseTransitionOwnerFixture{}, nil)
	if err != nil {
		panic(err)
	}
	if allLocal && request.Mode == releasetransition.ProcessConverge && os.Getenv("UPDATE_FOLLOWUP_SCENARIO") == "drift" && response.Outcome != nil && response.Outcome.ReachedGoal {
		if err := os.Remove(filepath.Join(request.ConfigHome, "converged-secondary")); err != nil {
			panic(err)
		}
	}
	if allLocal && request.Mode == releasetransition.ProcessInspect && os.Getenv("UPDATE_FOLLOWUP_SCENARIO") == "blockers" && response.Inspection != nil {
		response.Inspection.Blockers = []releasetransition.Blocker{
			{Code: releasetransition.CodePreconditionBlocked, Resource: "activation.fixture-one", Message: "first refusal", Retry: "inspect first"},
			{Code: releasetransition.CodePreconditionBlocked, Resource: "activation.fixture-two", Message: "second refusal", Retry: "inspect second"},
		}
		response.Inspection.Outcome.Status = releasetransition.StatusOperatorActionRequired
		response.Inspection.Outcome.Code = releasetransition.CodePreconditionBlocked
		// A public outcome must describe the first blocker exactly.
		response.Inspection.Outcome.Message = "first refusal"
		response.Inspection.Outcome.Retry = "inspect first"
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		panic(err)
	}
	os.Exit(0)
}

type updateFollowupYard struct{ home, name string }

func (yard *updateFollowupYard) ID() string { return "fixture-" + yard.name }
func (yard *updateFollowupYard) Observe(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	_, err := os.Stat(filepath.Join(yard.home, "converged-"+yard.name))
	desired := releasetransition.Fingerprint(strings.Repeat("d", 64))
	actual := desired
	if err != nil {
		actual = releasetransition.Fingerprint(strings.Repeat("e", 64))
	}
	return releasetransition.V2ActivationObservation{Actual: actual, Desired: desired, Converged: err == nil,
		Consequences: []string{"yard " + yard.name + ": reconcile materialized configuration"}}, nil
}
func (yard *updateFollowupYard) Reconcile(context.Context, releasetransition.ReleaseLinks) error {
	return os.WriteFile(filepath.Join(yard.home, "converged-"+yard.name), []byte("ready"), 0o600)
}

func updateFollowupFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, environment, runtimeRoot := updateReleaseFixture(t)
	// The protected store never creates the config home root itself (it opens it
	// with O_NOFOLLOW and treats ENOENT as fatal when writing); an installed yard
	// always has it. Model that precondition with an exact private mode.
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	engine := `#!/bin/sh
set -eu
if [ "$1" = --version ]; then printf 'yard-engine 1.2.3\n'; exit; fi
[ "$1" = _release-transition ] || exit 64
UPDATE_FOLLOWUP_HELPER=1 exec "$UPDATE_FOLLOWUP_BINARY" -test.run='^TestUpdateCompletesFreshActivationRepairInOneCommand$' --
`
	writeCLIFile(t, filepath.Join(root, "followup-engine"), engine, 0o700)
	registry := `{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`
	writeCLIFile(t, filepath.Join(root, "config", "release-transition.json"), registry, 0o600)
	installer := `#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  case "$1" in --runtime-root) root=$2; shift 2 ;; --publish-only) shift ;; *) shift ;; esac
done
destination="$root/releases/1.2.3-f16d05ec6b29"
install -d -m 0700 "$destination/bin" "$destination/config"
install -m 0700 "$UPDATE_FOLLOWUP_REPOSITORY/followup-engine" "$destination/bin/yard-engine"
install -m 0600 "$UPDATE_FOLLOWUP_REPOSITORY/config/release-transition.json" "$destination/config/release-transition.json"
(cd "$destination"; sha256sum ./bin/yard-engine ./config/release-transition.json > runtime-files.sha256)
printf 'releases/1.2.3-f16d05ec6b29\n'
`
	writeCLIFile(t, filepath.Join(root, "scripts", "install-runtime-release.sh"), installer, 0o700)
	return root, append(environment, "UPDATE_FOLLOWUP_BINARY="+binary, "UPDATE_FOLLOWUP_REPOSITORY="+root), runtimeRoot
}

func TestUpdateCompletesFreshActivationRepairInOneCommand(t *testing.T) {
	if os.Getenv("UPDATE_FOLLOWUP_HELPER") == "1" {
		runUpdateFollowupOwnerProcess()
		return
	}
	for _, scenario := range []string{"automation", "consent", "decline", "EOF", "non-TTY", "drift", "blockers", "remote-consent", "remote-automation"} {
		t.Run(scenario, func(t *testing.T) {
			if strings.HasPrefix(scenario, "remote-") {
				checkRemoteUpdateConsent(t, scenario == "remote-automation")
				return
			}
			root, environment, runtimeRoot := updateFollowupFixture(t)
			environment = append(environment, "UPDATE_FOLLOWUP_SCENARIO="+scenario)
			arguments := []string{"update", "--version", "1.2.3", "--runtime-root", runtimeRoot}
			prompt := &testkit.Prompt{Answers: []bool{true, scenario != "decline"}}
			if slices.Contains([]string{"automation", "drift", "blockers"}, scenario) {
				arguments = append(arguments, "--yes")
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Arguments: arguments,
				WorkingDir: root, Stdin: strings.NewReader(""), Prompt: prompt, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			// Approve only the first plan; the actual prompt boundary must reject
			// the next plan on EOF or a non-terminal without explicit consent.
			if scenario == "EOF" || scenario == "non-TTY" {
				program.options.Prompt = &firstUpdatePrompt{program: program, first: prompt}
				program.promptInputTerminal = func() bool { return scenario == "EOF" }
			}
			code := program.Run(context.Background())
			want := 0
			if slices.Contains([]string{"decline", "EOF", "non-TTY", "drift", "blockers"}, scenario) {
				want = 1
			}
			if code != want {
				t.Fatalf("code=%d want=%d stdout=%s stderr=%s", code, want, stdout.String(), stderr.String())
			}
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			if _, err := os.Stat(filepath.Join(configHome, "converged-default")); err != nil {
				t.Fatalf("selected yard did not converge: %v", err)
			}
			_, repaired := os.Stat(filepath.Join(configHome, "converged-secondary"))
			if (repaired == nil) != (want == 0) {
				t.Fatalf("secondary repaired=%v output=%s %s", repaired, stdout.String(), stderr.String())
			}
			if scenario != "blockers" && strings.Contains(stdout.String(), "Update did not complete successfully") {
				t.Fatalf("completed source migration presented as failed: %s", stdout.String())
			}
			if want != 0 && scenario != "blockers" && (!strings.Contains(stdout.String(), "Status: migration-required") || !strings.Contains(stdout.String(), "yard secondary")) {
				t.Fatalf("pending repair not explained: %s", stdout.String())
			}
			if scenario == "consent" && len(prompt.Requests) != 2 {
				t.Fatalf("prompts=%#v", prompt.Requests)
			}
			if scenario == "consent" && (strings.Contains(strings.Join(prompt.Requests[0].Consequences, " "), "fixture-secondary") ||
				strings.Contains(strings.Join(prompt.Requests[1].Consequences, " "), "fixture-default") ||
				!strings.Contains(strings.Join(prompt.Requests[1].Consequences, " "), "fixture-secondary")) {
				t.Fatalf("authorization scopes were combined: %#v", prompt.Requests)
			}
			if want == 0 && !strings.Contains(stdout.String(), "Status: ready") {
				t.Fatalf("final readiness missing: %s", stdout.String())
			}
			records, err := (audit.UpdateHistory{Home: environmentValue(environment, "SUBYARD_HOME")}).Read(10)
			wantAttempts := 2
			if scenario == "blockers" {
				wantAttempts = 1
				if first, second := strings.Index(stdout.String(), "first refusal"), strings.Index(stdout.String(), "second refusal"); first < 0 || second <= first {
					t.Fatalf("refusals lost or reordered: %s", stdout.String())
				}
			}
			if err != nil || len(records) != wantAttempts {
				t.Fatalf("missing separate repair attempt history: %#v err=%v", records, err)
			}
			if scenario == "drift" && !strings.Contains(stdout.String(), "[FAIL] Current release repair did not complete successfully") {
				t.Fatalf("second readiness failure was hidden: %s", stdout.String())
			}
			if scenario == "automation" {
				checkCompletedUpdateActivationRepairEligibility(t, configHome, "1.2.3-f16d05ec6b29")
			}
			if scenario == "decline" {
				// A process stopping between the phases has exactly this durable
				// state: completed source history and an unrepaired second yard.
				resume, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root,
					Arguments: []string{"update", "--runtime-root", runtimeRoot, "--yes"}, Incus: &testkit.Incus{}, Stdout: &stdout, Stderr: &stderr})
				if err != nil {
					t.Fatal(err)
				}
				if code := resume.Run(context.Background()); code != 0 {
					t.Fatalf("plain update did not recover between phases: code=%d %s %s", code, stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestUpdateExactRPCPlanDoesNotAuthorizeFollowup(t *testing.T) {
	root, environment, runtimeRoot := updateFollowupFixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	defer handler.closePlans()
	params, _ := json.Marshal(map[string]any{"command": "update", "arguments": []string{"--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot}, "exact": true, "stepSchema": 1})
	value, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "pinned-update", Params: params}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := value.(exactOperationPlan)
	params, _ = json.Marshal(map[string]any{"confirmed": true, "digest": plan.Digest})
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "pinned-update", Params: params}, nil)
	var fault *rpc.Error
	if !errors.As(err, &fault) || fault.Code != "transition_required" || !strings.Contains(fault.Message, "status=migration-required") || !strings.Contains(fault.Message, "yard secondary") {
		t.Fatalf("RPC lost pending readiness: %v", err)
	}
	if _, err := os.Stat(filepath.Join(loaded.Context.Paths.ConfigHome, "converged-secondary")); !os.IsNotExist(err) {
		t.Fatalf("pinned approval authorized another yard: %v", err)
	}
}

func checkRemoteUpdateConsent(t *testing.T, yes bool) {
	t.Helper()
	root, environment, state := nativeFixture(t)
	remote := filepath.Join(filepath.Dir(state), "yards", "remote")
	if err := os.MkdirAll(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(remote, "config.env"),
		"ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\nSSH_PORT=4444\n", 0o600)
	bin, capture := filepath.Join(root, "fake-bin"), filepath.Join(root, "forwarded")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(bin, "ssh"), "#!/bin/sh\n"+trustedSSHMock(t)+"\nprintf '%s\\n' \"$@\" >\"$SUBYARD_TEST_SSH_LOG\"\n", 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	environment = append(environment, "PATH="+os.Getenv("PATH"), "SUBYARD_TEST_SSH_LOG="+capture)
	arguments := []string{"-Y", "remote", "update", "--version", "1.2.3"}
	if yes {
		arguments = append(arguments, "--yes")
	}
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Arguments: arguments, WorkingDir: root, Prompt: prompt, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("remote code=%d stderr=%s", code, stderr.String())
	}
	payload, err := os.ReadFile(capture)
	if err != nil || strings.Contains(string(payload), "--yes") != yes || len(prompt.Requests) != 0 {
		t.Fatalf("controller altered owner consent: %s prompts=%v err=%v", payload, prompt.Requests, err)
	}
}

func checkCompletedUpdateActivationRepairEligibility(t *testing.T, home, target string) {
	t.Helper()
	for _, scenario := range []string{"eligible", "blockers", "recovering", "other-code", "different-target", "different-active", "resume", "missing-journal"} {
		t.Run(scenario, func(t *testing.T) {
			inspection := releasetransition.Inspection{Outcome: &releasetransition.Outcome{
				Status: releasetransition.StatusMigrationRequired, Code: releasetransition.CodeTransitionRequired, Active: releasetransition.ReleaseID(target), Target: releasetransition.ReleaseID(target),
			}}
			execution := &releaseExecution{configHome: home, inspection: &inspection, prepared: releaseruntime.Prepared{TargetRelease: target}}
			switch scenario {
			case "blockers":
				inspection.Blockers = []releasetransition.Blocker{{Code: releasetransition.CodePreconditionBlocked}}
			case "recovering":
				inspection.Outcome.Status = releasetransition.StatusRecovering
			case "other-code":
				inspection.Outcome.Code = releasetransition.CodeRecoveryPending
			case "different-target":
				inspection.Outcome.Target = "release-other"
			case "different-active":
				inspection.Outcome.Active = "release-old"
			case "resume":
				transaction := releasetransition.TransactionID("tx-0123456789abcdef")
				inspection.Resume = &transaction
			case "missing-journal":
				execution.configHome = testkit.TempDir(t)
			}
			if got := execution.needsActivationRepair(); got != (scenario == "eligible") {
				t.Fatalf("follow-up eligibility=%v for %s", got, scenario)
			}
		})
	}
}

type firstUpdatePrompt struct {
	program *CLI
	first   *testkit.Prompt
}

func (prompt *firstUpdatePrompt) Confirm(ctx context.Context, request domain.ConfirmationRequest) (bool, error) {
	prompt.program.options.Prompt = nil
	return prompt.first.Confirm(ctx, request)
}

// A marker-only retry runs after the bound native plan, but before final
// readiness. Retiring the marker must not invalidate that native plan.
func TestUpdateDiskTmpRetryPrecedesFreshFinalCheck(t *testing.T) {
	for _, runtimeDrift := range []bool{false, true} {
		t.Run(fmt.Sprintf("boot-restored-runtime-drift=%t", runtimeDrift), func(t *testing.T) {
			root, environment, runtimeRoot := updateFollowupFixture(t)
			var output bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, Stdout: &output, Stderr: &output})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("update")
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{"--version", "1.2.3", "--runtime-root", runtimeRoot}})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			calls := 0
			home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			prepared.release.retryDiskTmp = func(context.Context) error {
				calls++
				if _, err := os.Stat(filepath.Join(home, "converged-default")); err != nil {
					return fmt.Errorf("retry ran before native convergence: %w", err)
				}
				// Model marker completion after boot restored a pending yard. Runtime
				// drift must use the existing freshly planned activation follow-up.
				if runtimeDrift {
					return nil
				}
				return os.WriteFile(filepath.Join(home, "converged-secondary"), []byte("ready"), 0o600)
			}
			if code := program.runPreparedCommand(context.Background(), prepared, true); code != 0 || calls != 1 || strings.Count(output.String(), "Operation plan:") != map[bool]int{false: 1, true: 2}[runtimeDrift] || !strings.Contains(output.String(), "Status: ready") {
				t.Fatalf("retry did not precede the final check: code=%d calls=%d %s", code, calls, output.String())
			}
		})
	}
}
