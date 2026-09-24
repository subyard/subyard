package releaseruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type currentNoNetwork struct{ t *testing.T }

func TestCurrentReportPreservesWarningsInHumanAndJSONOutput(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		runtime := New(Config{Stdout: &stdout, Stderr: &stderr})
		report := currentReport{Current: "release-b", Outcome: releasetransition.Outcome{
			Status: releasetransition.StatusReady, Warnings: []string{"yard stopped: refresh deferred"},
		}}
		prepared := runtime.prepareCurrentReport(currentOptions{json: asJSON}, report)
		if err := prepared.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
		if stderr.Len() != 0 || strings.Count(stdout.String(), report.Outcome.Warnings[0]) != 1 {
			t.Fatalf("warning missing, repeated or on stderr: stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		if asJSON {
			var decoded currentReport
			if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || len(decoded.Outcome.Warnings) != 1 {
				t.Fatalf("warning lost from JSON: %q, %v", stdout.String(), err)
			}
		}
	}
}

func (transport currentNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	transport.t.Error("current migration contacted release network")
	return nil, errors.New("network forbidden")
}

func newCurrentProcessFixture(t *testing.T) v0111ProcessFixture {
	t.Helper()
	fixture, _ := newV0111ProcessFixture(t)
	fixture.previous = fixture.candidate.release
	fixture.candidate = writeV0111ProcessRuntime(t, fixture, string(fixture.candidate.release), "0.11.2", fixture.candidateTx, fixture.registry)
	if err := os.Remove(filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+string(fixture.candidate.release), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestCurrentTransitionCatchesUpInstalledReleaseWithoutSelection(t *testing.T) {
	fixture := newCurrentProcessFixture(t)
	environment := fixture.environment()
	environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
	environment["YARD_RELEASE_CACHE"] = "invalid-relative-cache"
	environment["YARD_RELEASE_VERSION"] = "invalid/selected/version"
	environment["YARD_RELEASE_TAG"] = "unrelated-tag"
	environment["SUBYARD_SOURCE_INGRESS_V1_ROOT"] = "incomplete-untrusted-source"
	var stdout bytes.Buffer
	runtime := New(Config{Environment: environment, Installer: "/missing-installer", RepositoryRoot: "/untrusted-source", Stdout: &stdout, HTTPClient: &http.Client{Transport: currentNoNetwork{t}}})
	defer runtime.Close()
	check, err := runtime.PrepareCurrentTransition(context.Background(), []string{"--check", "--json"}, fixture.configHome, "recovery-yard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if check.Action != "migrate.check" || check.Changed {
		t.Fatalf("check assessment = %#v", check)
	}
	if err := check.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Current string `json:"current"`
		Domains []struct {
			Pending []string `json:"pending"`
		} `json:"domains"`
		Outcome releasetransition.Outcome `json:"outcome"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Current != string(fixture.candidate.release) || report.Outcome.Status != releasetransition.StatusMigrationRequired || len(report.Domains) == 0 {
		t.Fatalf("check report = %s", stdout.String())
	}
	pending := 0
	for _, domain := range report.Domains {
		pending += len(domain.Pending)
	}
	if pending != 2 {
		t.Fatalf("pending migrations = %d: %s", pending, stdout.String())
	}
	if _, err := os.Stat(filepath.Join(fixture.configHome, "release-transition")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check mutated target: %v", err)
	}
	apply, err := runtime.PrepareCurrentTransition(context.Background(), []string{"--yes"}, fixture.configHome, "recovery-yard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if apply.Action != "migrate.apply" || !apply.Changed {
		t.Fatalf("apply assessment = %#v", apply)
	}
	stdout.Reset()
	if err := apply.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "ready") || !strings.Contains(stdout.String(), string(fixture.candidate.release)) {
		t.Fatalf("apply output = %q", stdout.String())
	}
	store, _ := releasetransition.NewPOSIXV2Store(fixture.configHome)
	before, _ := store.ReadCurrentJournal()
	second, err := runtime.PrepareCurrentTransition(context.Background(), nil, fixture.configHome, "recovery-yard", nil)
	if err != nil || second.Changed {
		t.Fatalf("repeat assessment = %#v, %v", second, err)
	}
	if err := second.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := store.ReadCurrentJournal()
	if !sameProtectedSnapshot(before, after) {
		t.Fatal("ready repeat rewrote journal")
	}
	links, _ := runtime.inspectRuntimeLinks(fixture.runtimeRoot)
	if links.current.target != "releases/"+string(fixture.candidate.release) || links.previous.present {
		t.Fatalf("current migration switched links: %#v", links)
	}
}

func TestStaleCompletedRepairKeepsPlanStaleCurrentRetry(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalComplete)
	marker := filepath.Join(filepath.Dir(fixture.runtimeRoot), "plan-stale")
	initialPlan := "plan-v1-" + strings.Repeat("1", 64)
	recheckedPlan := "plan-v1-" + strings.Repeat("2", 64)
	driftInspection := func(plan string) string {
		return fmt.Sprintf(
			`{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":%q,"assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"recovering","reachedGoal":false,"active":%q,"previous":"release-a","target":%q,"code":"recovery-pending","message":"the inspected release transition has not started","retry":"run yard update","transaction":%q}}}`,
			plan, fixture.target, fixture.target, fixture.transaction,
		)
	}
	stale := fmt.Sprintf(
		`{"schemaVersion":1,"activationReconciliationOwned":true,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":%q,"previous":"release-a","target":%q,"code":"plan-stale","message":"the inspected release transition changed before convergence","retry":"run yard update --check"}}`,
		fixture.target, fixture.target,
	)
	engine := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request=$(cat)
    case "$request" in
      *'"mode":"inspect"'*)
        if [ -e %q ]; then printf '%%s\n' %q; else printf '%%s\n' %q; fi
        ;;
      *'"mode":"converge"'*)
        : > %q
        printf '%%s\n' %q
        ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, marker, driftInspection(recheckedPlan), driftInspection(initialPlan), marker, stale)
	writeProtectedRuntimeFixtureEngine(t, fixture, engine)
	environment := fixture.environment()
	environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
	runtime := New(Config{
		Environment: environment, Installer: fixture.installer,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	defer runtime.Close()

	prepared, err := runtime.PrepareTransition(
		context.Background(), []string{"--runtime-root", fixture.runtimeRoot},
		fixture.configHome, "default", nil,
	)
	if err != nil {
		cause := err
		for errors.Unwrap(cause) != nil {
			cause = errors.Unwrap(cause)
		}
		t.Fatalf("prepare completed activation repair: %v (cause: %v)", err, cause)
	}
	err = prepared.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "code=plan-stale") ||
		!strings.Contains(err.Error(), "next: run yard migrate --check") ||
		strings.Contains(err.Error(), "code=recovery-pending") {
		t.Fatalf("stale completed activation repair error = %v", err)
	}
}

func TestCurrentTransitionRejectsVersionSelectionAndCurrentDrift(t *testing.T) {
	for _, arguments := range [][]string{{"--json"}, {"--version", "0.11.2"}, {"--offline"}, {"--rollback"}, {"--force"}, {"--runtime-root", "/tmp/runtime"}} {
		runtime := New(Config{})
		if _, err := runtime.PrepareCurrentTransition(context.Background(), arguments, "/tmp/config", "default", nil); err == nil {
			t.Fatalf("accepted flags %v", arguments)
		}
	}
	fixture := newCurrentProcessFixture(t)
	environment := fixture.environment()
	environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
	runtime := New(Config{Environment: environment, Stdout: &bytes.Buffer{}})
	defer runtime.Close()
	prepared, err := runtime.PrepareCurrentTransition(context.Background(), nil, fixture.configHome, "recovery-yard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+string(fixture.source.release), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("drift error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.configHome, "release-transition")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale plan mutated target: %v", err)
	}
}

func TestCurrentTransitionReportsPartialLedgerAndHumanNextAction(t *testing.T) {
	fixture := newCurrentProcessFixture(t)
	registry, _, err := releasetransition.ParseRegistryV2(fixture.registry, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := releasetransition.BaselineLedgerV2(registry).Advance(registry, registry.Migrations[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(fixture.configHome, "release-transition", "v2", "ledger.json")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := fixture.environment()
	environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
	var stdout bytes.Buffer
	runtime := New(Config{Environment: environment, Stdout: &stdout})
	defer runtime.Close()
	prepared, err := runtime.PrepareCurrentTransition(context.Background(), []string{"--check"}, fixture.configHome, "recovery-yard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"applied: " + registry.Migrations[0].ID, "pending: " + registry.Migrations[1].ID, "Next: yard migrate", "activation.runtime-config"} {
		if !strings.Contains(stdout.String(), wanted) {
			t.Fatalf("missing %q in %s", wanted, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "activation setting") {
		t.Fatalf("runtime action described as a setting: %s", stdout.String())
	}
	if !bytes.Equal(mustReadFile(t, ledgerPath), payload) {
		t.Fatal("check rewrote partial ledger")
	}
}

func newCurrentInterruptedFixture(t *testing.T, preactivation bool) v0111ProcessFixture {
	t.Helper()
	fixture, _ := newV0111ProcessFixture(t)
	fixture.previous = fixture.source.release
	fixture.candidate = writeV0111ProcessRuntime(t, fixture, string(fixture.candidate.release), "0.11.2", fixture.candidateTx, fixture.registry)
	if err := os.Remove(filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+string(fixture.source.release), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	links, err := releasetransition.NewRuntimeLinkStore(fixture.runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: fixture.configHome, Releases: releasetransition.ReleasePair{From: fixture.source.release, Target: fixture.candidate.release},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			if preactivation {
				return releasetransition.ReleaseLinks{}, errors.New("fixture activation interrupted")
			}
			return links.Activate(pair)
		},
		Reconcilers:       []releasetransition.V2ActivationReconciler{v0111FixtureReconciler{statePath: fixture.statePath, desired: v0111FixtureFingerprint("candidate-scope"), fail: true}},
		OwnerRegistration: v0111AbsentOwnerRegistration{}, RegistryPayload: fixture.registry,
		ArtifactDigest: v0111FixtureFingerprintBytes(mustReadFile(t, filepath.Join(fixture.candidate.root, "runtime-files.sha256"))), CandidateVersion: "0.11.2",
		NewTransactionID:    func() releasetransition.TransactionID { return fixture.candidateTx },
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := transition.Inspect(context.Background(), releasetransition.Goal{Target: fixture.candidate.release, Direction: releasetransition.DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "fixture-authorization"})
	if err != nil || outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("interrupt = %#v, %v", outcome, err)
	}
	return fixture
}

func TestCurrentTransitionPreActivationRefusesAndPostActivationResumes(t *testing.T) {
	for _, preactivation := range []bool{true, false} {
		t.Run(map[bool]string{true: "before-activation", false: "after-activation"}[preactivation], func(t *testing.T) {
			fixture := newCurrentInterruptedFixture(t, preactivation)
			environment := fixture.environment()
			environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
			var stdout bytes.Buffer
			runtime := New(Config{Environment: environment, Stdout: &stdout})
			defer runtime.Close()
			check, err := runtime.PrepareCurrentTransition(context.Background(), []string{"--check", "--json"}, fixture.configHome, "recovery-yard", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := check.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			var report struct {
				SchemaVersion int    `json:"schemaVersion"`
				Current       string `json:"current"`
				Next          string `json:"next"`
				Journal       struct {
					Transaction string `json:"transaction"`
				} `json:"journal"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.SchemaVersion != 1 || report.Journal.Transaction != string(fixture.candidateTx) {
				t.Fatalf("report = %s", stdout.String())
			}
			apply, err := runtime.PrepareCurrentTransition(context.Background(), nil, fixture.configHome, "recovery-yard", nil)
			if preactivation {
				if err == nil || !strings.Contains(err.Error(), "yard update") || report.Next != "yard update" || report.Current != string(fixture.source.release) {
					t.Fatalf("preactivation apply = %v; report = %s", err, stdout.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if apply.Changed {
				t.Fatal("protected resume requested a fresh confirmation")
			}
			if err := apply.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			store, _ := releasetransition.NewPOSIXV2Store(fixture.configHome)
			snapshot, _ := store.ReadCurrentJournal()
			journal, err := releasetransition.ParseJournal(snapshot.Payload)
			if err != nil || journal.Transaction != fixture.candidateTx || journal.Checkpoint != releasetransition.JournalComplete || journal.Releases.From != fixture.source.release {
				t.Fatalf("resumed journal = %#v, %v", journal, err)
			}
		})
	}
}

func TestCurrentTransitionDoesNotClaimReadinessWithoutOwnerReconciliation(t *testing.T) {
	fixture := newCurrentProcessFixture(t)
	response := fmt.Sprintf(`{"schemaVersion":1,"inspection":{"plan":"plan-v1-%s","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":%q,"target":%q,"code":"ready","message":"verified"}}}`, strings.Repeat("a", 64), fixture.candidate.release, fixture.candidate.release)
	payload := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine 0.11.2\\n' ;;\n _release-transition) cat >/dev/null; printf '%%s\\n' %q ;;\n *) exit 64 ;;\nesac\n", response)
	writeVersionedRuntimeCandidate(t, fixture.runtimeRoot, string(fixture.candidate.release), "0.11.2", payload)
	environment := fixture.environment()
	environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
	var stdout bytes.Buffer
	runtime := New(Config{Environment: environment, Stdout: &stdout})
	defer runtime.Close()
	check, err := runtime.PrepareCurrentTransition(context.Background(), []string{"--check", "--json"}, fixture.configHome, "recovery-yard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := check.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var report currentReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Outcome.Status != releasetransition.StatusOperatorActionRequired || report.Outcome.Code != releasetransition.CodeDependencyUnavailable || len(report.Domains) != 0 {
		t.Fatalf("unsupported owner report = %s", stdout.String())
	}
	if _, err := runtime.PrepareCurrentTransition(context.Background(), nil, fixture.configHome, "recovery-yard", nil); err == nil {
		t.Fatal("accepted owner without activation reconciliation")
	}
}

func TestCurrentReleaseRetryPreservesCrossReleaseAndSpecificRecovery(t *testing.T) {
	for _, scenario := range []struct {
		active, target releasetransition.ReleaseID
		retry, want    string
	}{
		{"release-a", "release-a", "run yard update", "run yard migrate"},
		{"release-a", "release-a", "run yard update --check", "run yard migrate --check"},
		{"release-a", "release-b", "run yard update", "run yard update"},
		{"release-a", "release-a", "restore a verified release, then run yard update --check", "restore a verified release, then run yard update --check"},
		{"", "", "run yard update", "run yard update"},
	} {
		outcome := releasetransition.Outcome{Active: scenario.active, Target: scenario.target, Retry: scenario.retry}
		if got := CurrentReleaseRetry(outcome); got != scenario.want {
			t.Fatalf("retry %#v = %q; want %q", scenario, got, scenario.want)
		}
	}
}

const currentScopeProcessEnvironment = "SUBYARD_TEST_CURRENT_NAMED_SCOPE"

func currentScopeTransition(request releasetransition.ProcessRequest, statePath string, fail bool) (*releasetransition.V2Transition, error) {
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	registry, err := os.ReadFile(filepath.Join(request.RuntimeRoot, "releases", string(request.Target), "config", "release-transition.json"))
	if err != nil {
		return nil, err
	}
	desiredScope := strings.TrimSuffix(request.Yard, "-alias")
	store, _ := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return nil, err
	}
	if snapshot.Exists {
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return nil, err
		}
		if journal.Checkpoint == releasetransition.JournalComplete {
			desiredScope = "named"
			if os.Getenv("SUBYARD_TEST_CURRENT_SCOPE_EXPANDS") == "1" {
				desiredScope = "all-local"
			}
		}
	}
	return releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: request.ConfigHome, Releases: releasetransition.ReleasePair{From: "0.11.1-source", Target: request.Target}, Direction: request.Direction,
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		Reconcilers:       []releasetransition.V2ActivationReconciler{v0111FixtureReconciler{statePath: statePath, desired: v0111FixtureFingerprint("scope:" + desiredScope), fail: fail}},
		OwnerRegistration: v0111AbsentOwnerRegistration{}, RegistryPayload: registry, ArtifactDigest: request.ArtifactDigest, CandidateVersion: "0.11.2",
		NewTransactionID: func() releasetransition.TransactionID { return "tx-current-named-scope" },
		VerifyAuthorization: func(_ releasetransition.PlanToken, authorization releasetransition.Authorization) bool {
			return authorization != ""
		},
	})
}

func TestCurrentNamedScopeProcessHelper(t *testing.T) {
	if os.Getenv(currentScopeProcessEnvironment) == "" {
		return
	}
	var request releasetransition.ProcessRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	transition, err := currentScopeTransition(request, os.Getenv("SUBYARD_TEST_CURRENT_SCOPE_STATE"), false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	response := releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true}
	switch request.Mode {
	case releasetransition.ProcessInspect:
		inspection, inspectErr := transition.Inspect(context.Background(), releasetransition.Goal{Target: request.Target, Direction: request.Direction})
		err = inspectErr
		response.Inspection = &inspection
	case releasetransition.ProcessConverge:
		if request.Execution == nil {
			os.Exit(1)
		}
		outcome, convergeErr := transition.Converge(context.Background(), *request.Execution)
		err = convergeErr
		response.Outcome = &outcome
	default:
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestCurrentTransitionRecoversUniqueNamedPostActivationScope(t *testing.T) {
	tooMany := []string{"named"}
	for index := 0; index < 127; index++ {
		tooMany = append(tooMany, fmt.Sprintf("context-%03d", index))
	}
	for _, scenario := range []struct {
		name          string
		registrations []string
		resumes       bool
		remote        bool
	}{
		{"unique", []string{"broken", "unrelated", "named"}, true, false},
		{"scope-expands", []string{"named"}, true, false},
		{"unregistered", []string{"unrelated"}, false, false},
		{"ambiguous", []string{"named", "named-alias"}, false, false},
		{"remote", []string{"named"}, false, true},
		{"too-many", tooMany, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture, _ := newV0111ProcessFixture(t)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine 0.11.2\\n' ;;\n _release-transition) %s=1 SUBYARD_TEST_CURRENT_SCOPE_STATE=%q exec %q -test.run '^TestCurrentNamedScopeProcessHelper$' ;;\n *) exit 64 ;;\nesac\n", currentScopeProcessEnvironment, fixture.statePath, executable)
			if scenario.name == "scope-expands" {
				engine = strings.Replace(engine, currentScopeProcessEnvironment+"=1", currentScopeProcessEnvironment+"=1 SUBYARD_TEST_CURRENT_SCOPE_EXPANDS=1", 1)
			}
			fixture.candidate = writeVersionedRuntimeCandidate(t, fixture.runtimeRoot, string(fixture.candidate.release), "0.11.2", engine)
			registryPath := filepath.Join(fixture.candidate.root, "config", "release-transition.json")
			if err := os.WriteFile(registryPath, fixture.registry, 0o600); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf("%s  ./bin/yard-engine\n%s  ./config/release-transition.json\n", v0111FixtureFingerprintBytes([]byte(engine)), v0111FixtureFingerprintBytes(fixture.registry))
			for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "ports.env"} {
				payload := mustReadFile(t, filepath.Join("..", "..", "..", "config", name))
				if err := os.WriteFile(filepath.Join(fixture.candidate.root, "config", name), payload, 0o600); err != nil {
					t.Fatal(err)
				}
				manifest += fmt.Sprintf("%s  ./config/%s\n", v0111FixtureFingerprintBytes(payload), name)
			}

			if err := os.WriteFile(filepath.Join(fixture.candidate.root, "runtime-files.sha256"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(fixture.runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/"+string(fixture.source.release), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			for _, name := range scenario.registrations {
				path := filepath.Join(fixture.configHome, "yards", name+".env")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				registration := "# registered fixture\n"
				if scenario.remote {
					registration = "ACCESS_KIND=remote\nOWNER_ENDPOINT=owner@example.invalid\nOWNER_YARD_NAME=named\n"
				}
				if name == "broken" {
					registration = "SSH_PORT=not-a-number\n"
				}
				if err := os.WriteFile(path, []byte(registration), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.resumes || scenario.remote {
				loaded, err := config.Load(config.LoadOptions{RepositoryRoot: fixture.candidate.root, OperatorHome: filepath.Dir(fixture.runtimeRoot), YardName: "named", Environment: map[string]string{"HOME": filepath.Dir(fixture.runtimeRoot), "SUBYARD_CONFIG_HOME": fixture.configHome}})
				if err != nil {
					t.Fatalf("load named fixture: %v", err)
				}
				if scenario.remote && loaded.Context.AccessKind != domain.AccessRemote {
					t.Fatal("remote fixture did not resolve as remote")
				}
			}
			request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: fixture.runtimeRoot, ConfigHome: fixture.configHome, Yard: "named", Target: fixture.candidate.release, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: v0111FixtureFingerprintBytes([]byte(manifest))}
			transition, err := currentScopeTransition(request, fixture.statePath, true)
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := transition.Inspect(context.Background(), releasetransition.Goal{Target: request.Target, Direction: request.Direction})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "fixture-authorization"})
			if err != nil || outcome.Status != releasetransition.StatusRecovering {
				t.Fatalf("seed named transition = %#v, %v", outcome, err)
			}
			environment := fixture.environment()
			environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
			var stdout bytes.Buffer
			sourceRoot := filepath.Join(filepath.Dir(fixture.runtimeRoot), "source-checkout")
			if err := os.MkdirAll(filepath.Join(sourceRoot, "private", "yards"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sourceRoot, "private", "yards", "named.env"), []byte("# source-only fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runtime := New(Config{Environment: environment, RepositoryRoot: sourceRoot, Stdout: &stdout})
			defer runtime.Close()
			wrong, err := runtime.inspectProtectedTransition(context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil)
			if err != nil || wrong == nil || len(wrong.inspection.Blockers) != 1 || wrong.inspection.Blockers[0].Resource != "transition.observation-scope" {
				t.Fatalf("wrong default scope = %#v, %v", wrong, err)
			}
			before, err := runtime.readCurrentSnapshot(fixture.runtimeRoot, fixture.configHome)
			if err != nil {
				t.Fatal(err)
			}
			check, err := runtime.PrepareCurrentTransition(context.Background(), []string{"--check", "--json"}, fixture.configHome, "default", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := check.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			var report currentReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			after, err := runtime.readCurrentSnapshot(fixture.runtimeRoot, fixture.configHome)
			if err != nil || !sameCurrentSnapshot(before, after) {
				t.Fatalf("scope search mutated snapshot: %v", err)
			}
			prepared, err := runtime.PrepareCurrentTransition(context.Background(), nil, fixture.configHome, "default", nil)
			if !scenario.resumes {
				if err == nil || report.Outcome.Status != releasetransition.StatusOperatorActionRequired || report.Next == "run yard migrate --check" || report.Next == "yard migrate" {
					t.Fatalf("unsafe scope selection = %v; report=%s", err, stdout.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Changed || report.Outcome.Status != releasetransition.StatusRecovering || len(report.Blockers) != 0 {
				t.Fatalf("named resume preparation = %#v; report=%s", prepared, stdout.String())
			}
			stdout.Reset()
			executeErr := prepared.Execute(context.Background())
			if scenario.name == "scope-expands" {
				if executeErr == nil || !strings.Contains(executeErr.Error(), "migrate") || strings.Contains(stdout.String(), ": ready") {
					t.Fatalf("historical convergence claimed final readiness: %v, %q", executeErr, stdout.String())
				}
			} else if executeErr != nil {
				t.Fatal(executeErr)
			}
			store, _ := releasetransition.NewPOSIXV2Store(fixture.configHome)
			snapshot, _ := store.ReadCurrentJournal()
			journal, err := releasetransition.ParseJournal(snapshot.Payload)
			if err != nil || journal.Transaction != wrong.journal.Transaction || journal.Checkpoint != releasetransition.JournalComplete || journal.Releases != wrong.journal.Releases {
				t.Fatalf("named resume changed transaction: %#v, %v", journal, err)
			}
			if string(mustReadFile(t, fixture.statePath)) != string(v0111FixtureFingerprint("scope:named"))+"\n" {
				t.Fatal("convergence lost the recovered named scope")
			}
		})
	}
}
