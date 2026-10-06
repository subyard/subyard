package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

const v0173ProcessEnvironment = "SUBYARD_TEST_V0173_TRANSITION_PROCESS"

func TestV0173UpdateNamesStandaloneMigrationRecovery(t *testing.T) {
	fixture := newV0173RecoveryFixture(t)
	environment := fixture.environment()
	environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
	runtime := New(Config{RepositoryRoot: fixture.candidate.root, Environment: environment, Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	_, err := runtime.PrepareTransition(context.Background(), []string{"--offline", "--version", "0.17.5"}, fixture.configHome, "default", nil)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(fixture.candidate.root, "bin", "yard")) ||
		!strings.Contains(err.Error(), "migrate --check") || !strings.Contains(err.Error(), "retry yard update after recovery") {
		t.Fatalf("update omitted concrete recovery commands: %v", err)
	}
	store, _ := releasetransition.NewPOSIXV2Store(fixture.configHome)
	journal, _ := store.ReadCurrentJournal()
	if !sameProtectedSnapshot(fixture.journal, journal) {
		t.Fatal("blocked update changed the authorized journal")
	}
}

func TestV0173CurrentRecoveryPreservesOriginalJournalAndAssets(t *testing.T) {
	for _, scenario := range []string{"resume", "interrupted", "delegate changed", "source changed", "ledger changed", "journal changed", "scope changed"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newV0173RecoveryFixture(t)
			var stdout, stderr bytes.Buffer
			environment := fixture.environment()
			environment["YARD_RUNTIME_ROOT"] = fixture.runtimeRoot
			runtime := New(Config{RepositoryRoot: fixture.candidate.root, Environment: environment, Stdout: &stdout, Stderr: &stderr})
			defer runtime.Close()
			check, err := runtime.PrepareCurrentTransition(context.Background(), []string{"--check", "--json"}, fixture.configHome, "default", nil)
			if err != nil || check.Changed {
				t.Fatalf("delegated check: %v, stderr=%s", err, stderr.String())
			}
			if err := check.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			var report currentReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Outcome.Status != releasetransition.StatusRecovering || len(report.Blockers) != 0 {
				t.Fatalf("delegated report: %s, %v, stderr=%s", stdout.String(), err, stderr.String())
			}
			store, _ := releasetransition.NewPOSIXV2Store(fixture.configHome)
			checked, _ := store.ReadCurrentJournal()
			if !sameProtectedSnapshot(fixture.journal, checked) {
				t.Fatal("read-only delegation changed the protected journal")
			}
			apply, err := runtime.PrepareCurrentTransition(context.Background(), nil, fixture.configHome, "default", nil)
			if err != nil || apply.Changed || apply.TargetRelease != string(fixture.source.release) {
				t.Fatalf("delegated apply: %#v, %v, stderr=%s", apply, err, stderr.String())
			}
			switch scenario {
			case "interrupted":
				testkit.WriteFile(t, fixture.statePath+".fail", []byte("interrupt\n"), 0o600)
			case "delegate changed":
				testkit.WriteFile(t, filepath.Join(fixture.candidate.root, "bin", "yard-engine"), []byte("#!/bin/sh\nexit 99\n"), 0o700)
			case "source changed":
				testkit.WriteFile(t, filepath.Join(fixture.source.root, "config", "release-transition.json"), []byte("{}\n"), 0o600)
			case "ledger changed":
				testkit.WriteFile(t, filepath.Join(fixture.configHome, "release-transition", "v2", "ledger.json"), []byte("{}\n"), 0o600)
			case "journal changed":
				testkit.WriteFile(t, filepath.Join(fixture.configHome, "release-transition", "v2", "journal.json"), []byte("{}\n"), 0o600)
			case "scope changed":
				testkit.WriteFile(t, filepath.Join(fixture.configHome, "yards", "unrelated.env"), []byte("YARD_TEMPLATE=synthetic-changed\n"), 0o600)
			}
			err = apply.Execute(context.Background())
			if scenario == "interrupted" {
				if err == nil {
					t.Fatal("interrupted delegate reported readiness")
				}
				if err := os.Remove(fixture.statePath + ".fail"); err != nil {
					t.Fatal(err)
				}
				resumed, prepareErr := runtime.PrepareCurrentTransition(context.Background(), nil, fixture.configHome, "default", nil)
				if prepareErr != nil {
					t.Fatal(prepareErr)
				}
				err = resumed.Execute(context.Background())
			}
			if scenario != "resume" && scenario != "interrupted" {
				if err == nil {
					t.Fatalf("changed %s allowed delegated convergence", scenario)
				}
				if _, err := os.Stat(fixture.statePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale repair mutated runtime state: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("delegated resume: %v, stderr=%s", err, stderr.String())
			}
			completed, _ := store.ReadCurrentJournal()
			before, _ := releasetransition.ParseJournal(fixture.journal.Payload)
			after, err := releasetransition.ParseJournal(completed.Payload)
			if err != nil || after.Checkpoint != releasetransition.JournalComplete || after.Transaction != before.Transaction ||
				after.ResumePlan != before.ResumePlan || after.AuthorizationPlan != before.AuthorizationPlan ||
				after.ArtifactDigest != before.ArtifactDigest || after.ObservationScope != before.ObservationScope || len(after.Steps) != 0 {
				t.Fatalf("delegation changed original bindings: %#v, %v", after, err)
			}
			ledger, _ := store.ReadLedger()
			if !bytes.Equal(ledger.Payload, fixture.ledger) || string(mustReadFile(t, filepath.Join(fixture.configHome, "yards", "unrelated.env"))) != "YARD_TEMPLATE=synthetic-other\n" {
				t.Fatal("delegation changed ledger or unrelated settings")
			}
			links, _ := runtime.inspectRuntimeLinks(fixture.runtimeRoot)
			if links.current.target != "releases/"+string(fixture.source.release) || links.previous.present {
				t.Fatalf("delegated migration changed stable release links: %#v", links)
			}
		})
	}
}

func TestV0173RecoveryRequiresExactActivationOnlyDefect(t *testing.T) {
	fixture := newV0173RecoveryFixture(t)
	runtime := New(Config{Environment: fixture.environment(), Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	protected, err := runtime.inspectProtectedTransition(context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil)
	if err != nil || !isV0173ActivationOnlyBlocker(protected) {
		t.Fatalf("eligible original defect: %v", err)
	}
	for _, scenario := range []struct {
		name   string
		change func(*protectedTransitionInspection)
	}{
		{"another release", func(p *protectedTransitionInspection) { p.target.version = "0.17.4" }},
		{"another owner", func(p *protectedTransitionInspection) { p.owner.digest = v0111FixtureFingerprint("other") }},
		{"migration steps", func(p *protectedTransitionInspection) {
			p.journal.Steps = []releasetransition.JournalStep{{ID: "unfinished-migration"}}
		}},
		{"another checkpoint", func(p *protectedTransitionInspection) { p.journal.Checkpoint = releasetransition.JournalAuthorized }},
		{"cross-release activation", func(p *protectedTransitionInspection) { p.journal.Releases.From = "0.17.2-original" }},
		{"rollback", func(p *protectedTransitionInspection) {
			p.journal.Goal.Direction = releasetransition.DirectionActivatePrevious
		}},
		{"source ingress", func(p *protectedTransitionInspection) {
			p.journal.SourceIngress = &releasetransition.SourceIngressRequest{}
		}},
		{"different catalog", func(p *protectedTransitionInspection) { p.journal.CatalogDigest = v0111FixtureFingerprint("other") }},
		{"different resume plan", func(p *protectedTransitionInspection) {
			p.journal.ResumePlan = releasetransition.PlanToken("plan-v1-" + string(v0111FixtureFingerprint("other")))
		}},
		{"additional blocker", func(p *protectedTransitionInspection) {
			p.inspection.Blockers = append(p.inspection.Blockers, releasetransition.Blocker{Code: releasetransition.CodePlanStale})
		}},
		{"different failure", func(p *protectedTransitionInspection) { p.inspection.Blockers[0].Message = "unrelated failure" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			changed := *protected
			changed.inspection.Blockers = append([]releasetransition.Blocker(nil), protected.inspection.Blockers...)
			scenario.change(&changed)
			if isV0173ActivationOnlyBlocker(&changed) {
				t.Fatal("unqualified recovery could delegate execution")
			}
		})
	}
}

func newV0173RecoveryFixture(t *testing.T) v0111ProcessFixture {
	t.Helper()
	root := testkit.TempDir(t)
	fixture := v0111ProcessFixture{runtimeRoot: filepath.Join(root, "runtime"), configHome: filepath.Join(root, "config"), statePath: filepath.Join(root, "activation-state"), sourceTx: "tx-activation-only"}
	fixture.registry = mustReadFile(t, filepath.Join("..", "..", "..", "config", "release-transition.json"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"0.17.3", "0.17.5"} {
		payload := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine %s\\n' ;;\n _release-transition) %s=1 SUBYARD_TEST_V0173_VERSION=%q SUBYARD_TEST_V0173_STATE=%q exec %q -test.run '^TestV0173TransitionProcessHelper$' ;;\n *) exit 64 ;;\nesac\n", version, v0173ProcessEnvironment, version, fixture.statePath, executable)
		candidate := writeVersionedRuntimeCandidate(t, fixture.runtimeRoot, version+"-fixture", version, payload)
		testkit.WriteFile(t, filepath.Join(candidate.root, "config", "release-transition.json"), fixture.registry, 0o600)
		manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", sha256.Sum256([]byte(payload)), sha256.Sum256(fixture.registry))
		testkit.WriteFile(t, filepath.Join(candidate.root, "runtime-files.sha256"), []byte(manifest), 0o600)
		if version == "0.17.3" {
			fixture.source = candidate
		} else {
			fixture.candidate = candidate
		}
	}
	if err := os.Symlink("releases/"+string(fixture.source.release), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fixture.configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	links, _ := releasetransition.NewRuntimeLinkStore(fixture.runtimeRoot)
	options := releasetransition.V2Options{
		ConfigHome: fixture.configHome, Releases: releasetransition.ReleasePair{From: fixture.source.release, Target: fixture.source.release},
		ObserveLinks:      func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		OwnerRegistration: v0111AbsentOwnerRegistration{}, RegistryPayload: fixture.registry,
		ArtifactDigest:      v0111FixtureFingerprintBytes(mustReadFile(t, filepath.Join(fixture.source.root, "runtime-files.sha256"))),
		VerifyAuthorization: func(_ releasetransition.PlanToken, grant releasetransition.Authorization) bool { return grant != "" },
	}
	initial, err := releasetransition.NewV2Transition(options)
	if err != nil {
		t.Fatalf("new initial transition: %v", err)
	}
	goal := releasetransition.Goal{Target: fixture.source.release, Direction: releasetransition.DirectionActivateTarget}
	if _, err := links.Observe(); err != nil {
		t.Fatalf("observe seed links: %v", err)
	}
	inspection, err := initial.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatalf("inspect initial transition: %v", err)
	}
	if outcome, err := initial.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "fixture-grant"}); err != nil || outcome.Status != releasetransition.StatusReady {
		t.Fatalf("seed completed migrations: %#v, %v", outcome, err)
	}
	if err := os.Mkdir(filepath.Join(fixture.configHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(fixture.configHome, "yards", "unrelated.env"), []byte("YARD_TEMPLATE=synthetic-other\n"), 0o600)
	interruptedReconciler := v0173Reconciler(fixture.configHome, fixture.statePath)
	interruptedReconciler.fail = true
	options.Reconcilers = []releasetransition.V2ActivationReconciler{interruptedReconciler}
	options.NewTransactionID = func() releasetransition.TransactionID { return fixture.sourceTx }
	repair, err := releasetransition.NewV2Transition(options)
	if err != nil {
		t.Fatalf("new repair transition: %v", err)
	}
	inspection, err = repair.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatalf("inspect repair transition: %v", err)
	}
	if outcome, err := repair.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "fixture-grant"}); err != nil || outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("seed activation-only journal: %#v, %v", outcome, err)
	}
	store, _ := releasetransition.NewPOSIXV2Store(fixture.configHome)
	fixture.journal, _ = store.ReadCurrentJournal()
	ledger, _ := store.ReadLedger()
	fixture.ledger = ledger.Payload
	return fixture
}

func v0173Reconciler(configHome, statePath string) v0111FixtureReconciler {
	payload, _ := os.ReadFile(filepath.Join(configHome, "yards", "unrelated.env"))
	_, interrupted := os.Stat(statePath + ".fail")
	return v0111FixtureReconciler{statePath: statePath, desired: v0111FixtureFingerprintBytes(payload), fail: interrupted == nil}
}

func TestV0173TransitionProcessHelper(t *testing.T) {
	if os.Getenv(v0173ProcessEnvironment) == "" {
		return
	}
	var request releasetransition.ProcessRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		panic(err)
	}
	assets := os.Getenv("SUBYARD_REPOSITORY_ROOT")
	registry, err := os.ReadFile(filepath.Join(assets, "config", "release-transition.json"))
	if err != nil || v0111FixtureFingerprintBytes(registry) != request.RegistryDigest ||
		v0111FixtureFingerprintBytes(mustReadFile(t, filepath.Join(assets, "runtime-files.sha256"))) != request.ArtifactDigest {
		panic("delegate did not use original verified assets")
	}
	links, _ := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	observed, _ := links.Observe()
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: request.ConfigHome, Releases: releasetransition.ReleasePair{From: observed.Active, Previous: observed.Previous, Target: request.Target},
		ObserveLinks:    func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		RegistryPayload: registry, ArtifactDigest: request.ArtifactDigest, OwnerRegistration: v0111AbsentOwnerRegistration{},
		Reconcilers:         []releasetransition.V2ActivationReconciler{v0173Reconciler(request.ConfigHome, os.Getenv("SUBYARD_TEST_V0173_STATE"))},
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return false },
	})
	if err != nil {
		panic(err)
	}
	response := releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true}
	if request.Mode == releasetransition.ProcessInspect {
		inspection, err := transition.Inspect(context.Background(), releasetransition.Goal{Target: request.Target, Direction: request.Direction})
		if err != nil {
			panic(err)
		}
		// Model the published inspector defect while preserving the wire contract.
		if os.Getenv("SUBYARD_TEST_V0173_VERSION") == "0.17.3" && inspection.Resume != nil {
			inspection.Blockers = []releasetransition.Blocker{{Code: releasetransition.CodePreconditionBlocked, Resource: "yard.unrelated", Message: "the yard template is not supported by this migration", Retry: "repair the named yard settings, then run yard update"}}
			inspection.Outcome.Status, inspection.Outcome.Code = releasetransition.StatusOperatorActionRequired, releasetransition.CodePreconditionBlocked
			inspection.Outcome.Message, inspection.Outcome.Retry = inspection.Blockers[0].Message, inspection.Blockers[0].Retry
		}
		response.Inspection = &inspection
	} else {
		if os.Getenv("SUBYARD_TEST_V0173_VERSION") == "0.17.3" {
			panic("activation-only recovery must execute the newer engine")
		}
		outcome, err := transition.Converge(context.Background(), *request.Execution)
		if err != nil {
			panic(err)
		}
		response.Outcome = &outcome
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		panic(err)
	}
	os.Exit(0)
}
