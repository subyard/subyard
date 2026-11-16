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
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

const sourceAdmissionProcessEnvironment = "SUBYARD_TEST_SOURCE_ADMISSION_PROCESS"
const sourceAdmissionRegistry = `{
  "schemaVersion": 2,
  "minimumEpochs": {
    "owner-registration": 3,
    "power-metadata": 3,
    "project-state": 1,
    "settings": 3
  },
  "currentEpochs": {
    "owner-registration": 3,
    "power-metadata": 3,
    "project-state": 1,
    "settings": 3
  },
  "migrations": [],
  "retiredPrefixes": {
    "owner-registration": {
      "throughEpoch": 3,
      "digest": "f612c2a88cfb6a2f1f3194acf13b933d3009d8853c9f093e07e5cad491c1aab6"
    },
    "power-metadata": {
      "throughEpoch": 3,
      "digest": "7692dc70c24e437966ba441be41204ec2642f3ceda004ef3492dd10c6826467d"
    },
    "settings": {
      "throughEpoch": 3,
      "digest": "559acd8e21b67b9f4516f44ec2c67f655898a9094147d5686c3816990c47855e"
    }
  }
}`

// The candidate runs the ordinary engine; only the historical owner's Observe
// is unavailable. This exercises admission without inventing a second engine.
func TestSourceAdmissionCandidateProcessHelper(t *testing.T) {
	if os.Getenv(sourceAdmissionProcessEnvironment) == "" {
		return
	}
	var request releasetransition.ProcessRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		os.Exit(1)
	}
	if path := os.Getenv("SUBYARD_TEST_SOURCE_ADMISSION_PHASE_LOG"); path != "" {
		_, publishedErr := os.Stat(filepath.Join(request.RuntimeRoot, "releases", string(request.Target)))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(1)
		}
		_, err = fmt.Fprintf(file, "%s:%t\n", request.Mode, publishedErr == nil)
		file.Close()
		if err != nil {
			os.Exit(1)
		}
	}
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		os.Exit(1)
	}
	actual, err := links.Observe()
	if err != nil {
		os.Exit(1)
	}
	adapter := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": filepath.Dir(request.RuntimeRoot)})})
	var reconcilers []releasetransition.V2ActivationReconciler
	if mode := os.Getenv(sourceAdmissionProcessEnvironment); mode == "repair" || mode == "old-wire" {
		reconcilers = []releasetransition.V2ActivationReconciler{v0111FixtureReconciler{statePath: filepath.Join(request.ConfigHome, "source-repair-state"), desired: v0111FixtureFingerprint("repaired")}}
	}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: request.ConfigHome, RegistryPayload: []byte(sourceAdmissionRegistry), ArtifactDigest: request.ArtifactDigest,
		Reconcilers:  reconcilers,
		Releases:     releasetransition.ReleasePair{From: actual.Active, Previous: actual.Previous, Target: request.Target},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		CompletedMetadata: func(ctx context.Context) (releasetransition.Fingerprint, error) {
			proof, err := adapter.InspectSourceAdmission(ctx, request.RuntimeRoot, request.ConfigHome)
			return releasetransition.Fingerprint(proof.Binding), err
		},
		NewTransactionID: func() releasetransition.TransactionID { return "tx-new-source-admission" },
		VerifyAuthorization: func(_ releasetransition.PlanToken, authorization releasetransition.Authorization) bool {
			return authorization != ""
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	response := releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true}
	if request.Mode == releasetransition.ProcessInspect {
		inspection, inspectErr := transition.Inspect(context.Background(), releasetransition.Goal{Target: request.Target, Direction: request.Direction})
		err, response.Inspection = inspectErr, &inspection
		if err == nil && os.Getenv(sourceAdmissionProcessEnvironment) == "old-wire" && inspection.Outcome.Status == releasetransition.StatusMigrationRequired {
			store, storeErr := releasetransition.NewPOSIXV2Store(request.ConfigHome)
			if storeErr != nil {
				os.Exit(1)
			}
			snapshot, readErr := store.ReadCurrentJournal()
			journal, parseErr := releasetransition.ParseJournal(snapshot.Payload)
			if readErr != nil || parseErr != nil {
				os.Exit(1)
			}
			inspection.Outcome.Status, inspection.Outcome.Code = releasetransition.StatusRecovering, releasetransition.CodeRecoveryPending
			inspection.Outcome.Transaction = &journal.Transaction
		}
	} else if request.Mode == releasetransition.ProcessConverge && request.Execution != nil {
		outcome, convergeErr := transition.Converge(context.Background(), *request.Execution)
		err, response.Outcome = convergeErr, &outcome
	} else {
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if json.NewEncoder(os.Stdout).Encode(response) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func sealSourceAdmissionCandidate(t *testing.T, candidate publishedCandidate) releasetransition.Fingerprint {
	t.Helper()
	registry := filepath.Join(candidate.root, "config", "release-transition.json")
	testkit.WriteFile(t, registry, []byte(sourceAdmissionRegistry), 0o600)
	engine := mustReadFile(t, filepath.Join(candidate.root, "bin", "yard-engine"))
	manifest := []byte(fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", sha256.Sum256(engine), sha256.Sum256([]byte(sourceAdmissionRegistry))))
	testkit.WriteFile(t, filepath.Join(candidate.root, "runtime-files.sha256"), manifest, 0o600)
	return releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest)))
}

func TestCompletedHistoryAdmissionPreparesNewGoalWhenOldObserveUnavailable(t *testing.T) {
	for _, drift := range []string{"missing history", "missing history without journal", "none", "journal replacement", "catalog", "registry", "sealed asset", "unfinished", "unfinished same target", "current repair", "current repair old-wire", "current repair replacement", "private draft", "private draft parent replacement", "private draft parent mode", "private draft immutable hardlink"} {
		t.Run(drift, func(t *testing.T) {
			missingHistory := strings.HasPrefix(drift, "missing history")
			ctx := context.Background()
			root := testkit.TempDir(t)
			runtimeRoot, home := filepath.Join(root, "runtime"), filepath.Join(root, "config")
			for _, path := range []string{home, filepath.Join(runtimeRoot, "releases", "release-a")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			oldCalls := filepath.Join(root, "old-observe-called")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			oldEngine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.3\\n';; _release-transition) cat >/dev/null; printf called >> %q; exit 99;; *) exit 64;; esac\n", oldCalls)
			if strings.HasPrefix(drift, "current repair") || strings.HasPrefix(drift, "private draft") || missingHistory {
				mode := "repair"
				if drift == "current repair old-wire" {
					mode = "old-wire"
				}
				if strings.HasPrefix(drift, "private draft") || missingHistory {
					mode = "ready"
				}
				oldEngine = fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.3\\n';; _release-transition) printf called >> %q; %s=%s exec %q -test.run '^TestSourceAdmissionCandidateProcessHelper$';; *) exit 64;; esac\n", oldCalls, sourceAdmissionProcessEnvironment, mode, executable)
			}
			old := writeVersionedRuntimeCandidate(t, runtimeRoot, "1.2.3-original", "1.2.3", oldEngine)
			artifact := sealSourceAdmissionCandidate(t, old)
			links, _ := releasetransition.NewRuntimeLinkStore(runtimeRoot)
			seedExistingCurrentLedger(t, home, []byte(sourceAdmissionRegistry))
			original, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: home, MigrationCheckpoint: missingHistory, SourceMigrationCheckpoint: missingHistory,
				RegistryPayload: []byte(sourceAdmissionRegistry), ArtifactDigest: artifact,
				Releases:     releasetransition.ReleasePair{From: "release-a", Target: old.release},
				ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
				ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
					return links.Activate(pair)
				},
				NewTransactionID: func() releasetransition.TransactionID { return "tx-original-source-admission" },
				VerifyAuthorization: func(_ releasetransition.PlanToken, authorization releasetransition.Authorization) bool {
					return authorization == "original-consent"
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := original.Inspect(ctx, releasetransition.Goal{Target: old.release, Direction: releasetransition.DirectionActivateTarget})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := original.Converge(ctx, releasetransition.Execution{Plan: inspection.Plan, Authorization: "original-consent"})
			if err != nil || outcome.Status != releasetransition.StatusReady {
				t.Fatalf("original fixture: %v %v", outcome, err)
			}

			journalPath := filepath.Join(home, "release-transition", "v2", "journal.json")
			before := mustReadFile(t, journalPath)
			bundle := []byte("synthetic verified publication input")
			candidateID := fmt.Sprintf("1.2.4-%x", sha256.Sum256(bundle))[:18]
			candidateRoot := runtimeRoot
			if strings.HasPrefix(drift, "private draft") || missingHistory {
				candidateRoot = filepath.Join(root, "candidate-template")
			}
			phaseLog := filepath.Join(root, "candidate-phases")
			candidate := writeVersionedRuntimeCandidate(t, candidateRoot, candidateID, "1.2.4", fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.4\\n';; _release-transition) %s=1 SUBYARD_TEST_SOURCE_ADMISSION_PHASE_LOG=%q exec %q -test.run '^TestSourceAdmissionCandidateProcessHelper$';; *) exit 64;; esac\n", sourceAdmissionProcessEnvironment, phaseLog, executable))
			sealSourceAdmissionCandidate(t, candidate)
			cache := filepath.Join(root, "cache", "1.2.4")
			if err := os.MkdirAll(cache, 0o700); err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("subyard-1.2.4-linux-%s.tar.gz", runtime.GOARCH)
			for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
				testkit.WriteFile(t, filepath.Join(cache, name+suffix), bundle, 0o600)
			}
			draftWrites := filepath.Join(root, "installer-writes")
			audit := ""
			if missingHistory {
				audit = fmt.Sprintf("find \"$destination\" -type f -print > %q\n", draftWrites)
			}
			installer := filepath.Join(root, "installer")
			testkit.WriteFile(t, installer, []byte(fmt.Sprintf("#!/bin/sh\n# --publish-only\nwhile [ $# -gt 0 ]; do case \"$1\" in --runtime-root) shift; destination=$1;; esac; shift; done\nmkdir -p \"$destination/releases\"\ncp -a %q \"$destination/releases/%s\"\n%sprintf 'releases/%s\\n'\n", candidate.root, candidateID, audit, candidateID)), 0o700)
			var engineDiagnostics bytes.Buffer
			adapter := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": root, "YARD_RUNTIME_ROOT": runtimeRoot, "YARD_RELEASE_CACHE": filepath.Dir(cache)}), Installer: installer, Stdout: &bytes.Buffer{}, Stderr: &engineDiagnostics})
			defer adapter.Close()
			if drift == "catalog" || strings.HasPrefix(drift, "unfinished") {
				journal, err := releasetransition.ParseJournal(before)
				if err != nil {
					t.Fatal(err)
				}
				if drift == "catalog" {
					journal.CatalogDigest = releasetransition.Fingerprint(strings.Repeat("c", 64))
				} else {
					journal.Checkpoint = releasetransition.JournalReconciling
				}
				payload, err := releasetransition.MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, journalPath, payload, 0o600)
				before = payload
			} else if drift == "registry" || drift == "sealed asset" {
				path := filepath.Join(old.root, "config", "release-transition.json")
				if drift == "sealed asset" {
					path = filepath.Join(old.root, "bin", "yard-engine")
				}
				testkit.WriteFile(t, path, []byte("changed\n"), 0o600)
			}
			if drift == "none" || drift == "journal replacement" || strings.HasPrefix(drift, "current repair") || strings.HasPrefix(drift, "private draft") {
				if _, proofErr := adapter.InspectSourceAdmission(ctx, runtimeRoot, home); proofErr != nil {
					for cause := proofErr; cause != nil; cause = errors.Unwrap(cause) {
						t.Logf("synthetic proof prerequisite: %v", cause)
					}
					t.Fatal("valid completed metadata prerequisite failed")
				}
			}
			version := "1.2.4"
			if drift == "unfinished same target" || strings.HasPrefix(drift, "current repair") {
				version = "1.2.3"
			}
			var beforeTree map[string]string
			if missingHistory {
				for _, name := range []string{"ledger.json", "history-checkpoint.json"} {
					if err := os.Remove(filepath.Join(home, "release-transition/v2", name)); err != nil {
						t.Fatal(err)
					}
				}
				if drift == "missing history without journal" {
					if err := os.Remove(journalPath); err != nil {
						t.Fatal(err)
					}
				}
				beforeTree = sourceAdmissionTreeSnapshot(t, root)
			}
			prepared, err := adapter.PrepareTransition(ctx, []string{"--offline", "--version", version}, home, "default")
			if missingHistory {
				if err == nil || !strings.Contains(engineDiagnostics.String(), releasetransition.ErrMigrationLedgerAbsent.Error()) {
					t.Errorf("engine did not refuse missing history: %v, diagnostic=%q", err, engineDiagnostics.String())
				}
				if !reflect.DeepEqual(beforeTree, sourceAdmissionTreeSnapshot(t, root)) {
					t.Error("missing-history refusal changed installation, cache, protected bytes or links")
				}
				drafts, globErr := filepath.Glob(filepath.Join(root, ".subyard-release-draft-*"))
				if globErr != nil || len(drafts) != 0 {
					t.Errorf("refusal left a private draft: %v %v", drafts, globErr)
				}
				return
			}

			if drift == "catalog" || drift == "registry" || drift == "sealed asset" || strings.HasPrefix(drift, "unfinished") {
				if err == nil {
					t.Fatal("unsupported original history admitted a new candidate")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Close != nil {
				defer prepared.Close()
			}
			if strings.HasPrefix(drift, "current repair") {
				if !prepared.Changed || !prepared.RepairCurrent || prepared.TargetRelease != string(old.release) || prepared.nativePlan == inspection.Plan {
					t.Fatalf("ordinary original-owner fresh repair was replaced: %#v", prepared)
				}
				if !bytes.Equal(before, mustReadFile(t, journalPath)) {
					t.Fatal("fresh repair preparation reopened completed history")
				}
				if drift == "current repair replacement" {
					temporary := journalPath + ".replacement"
					testkit.WriteFile(t, temporary, before, 0o600)
					if err := os.Rename(temporary, journalPath); err != nil {
						t.Fatal(err)
					}
					if err := prepared.Execute(ctx); err == nil {
						t.Fatal("replaced protected metadata accepted fresh repair consent")
					}
					return
				}
				if err := prepared.Execute(ctx); err != nil {
					t.Fatal(err)
				}
				journal, err := releasetransition.ParseJournal(mustReadFile(t, journalPath))
				if err != nil || journal.Transaction == *outcome.Transaction || journal.Goal.Target != old.release || journal.Checkpoint != releasetransition.JournalComplete || journal.AuthorizationPlan == inspection.Plan {
					t.Fatalf("original owner did not execute a new ordinary repair lifecycle: %v %v", journal, err)
				}
				return
			}
			if !prepared.Changed || prepared.TargetRelease != candidateID || prepared.RepairCurrent {
				t.Fatalf("new consent/goal missing: %#v", prepared)
			}
			if !bytes.Equal(before, mustReadFile(t, journalPath)) || len(mustReadFile(t, oldCalls)) == 0 {
				t.Fatal("preparation reopened history or skipped original operational inspection")
			}
			if drift == "journal replacement" {
				temporary := journalPath + ".replacement"
				testkit.WriteFile(t, temporary, before, 0o600)
				if err := os.Rename(temporary, journalPath); err != nil {
					t.Fatal(err)
				}
				if err := prepared.Execute(ctx); err == nil {
					t.Fatal("metadata replacement accepted stale consent")
				}
				return
			}
			if drift == "private draft parent replacement" || drift == "private draft parent mode" || drift == "private draft immutable hardlink" {
				parent := filepath.Join(runtimeRoot, "releases")
				switch drift {
				case "private draft parent replacement":
					if err := os.Rename(parent, parent+".before"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(parent, 0o700); err != nil {
						t.Fatal(err)
					}
				case "private draft parent mode":
					if err := os.Chmod(parent, 0o750); err != nil {
						t.Fatal(err)
					}
				case "private draft immutable hardlink":
					if err := os.Link(filepath.Join(old.root, "bin", "yard-engine"), filepath.Join(root, "engine-hardlink")); err != nil {
						t.Fatal(err)
					}
				}
				if err := prepared.Execute(ctx); err == nil {
					t.Fatal("changed source authority accepted draft publication consent")
				}
				if !bytes.Equal(before, mustReadFile(t, journalPath)) {
					t.Fatal("stale publication changed protected history")
				}
				return
			}
			if err := prepared.Execute(ctx); err != nil {
				t.Fatal(err)
			}
			if drift == "private draft" {
				phases := string(mustReadFile(t, phaseLog))
				if strings.Count(phases, "inspect:false\n") < 2 || !strings.Contains(phases, "converge:true\n") {
					t.Fatalf("private draft did not inspect/recheck before publication and converge after publication: %q", phases)
				}
			}
			journal, err := releasetransition.ParseJournal(mustReadFile(t, journalPath))
			if err != nil || journal.Transaction != "tx-new-source-admission" || journal.Goal.Target != candidate.release || journal.Checkpoint != releasetransition.JournalComplete || journal.AuthorizationDigest == releasetransition.Fingerprint(strings.Repeat("0", 64)) {
				t.Fatalf("new ordinary lifecycle missing: %v %v", journal, err)
			}
			if journal.Transaction == *outcome.Transaction || journal.AuthorizationPlan == inspection.Plan {
				t.Fatal("old transaction or consent reused")
			}
		})
	}
}

func TestSourceAdmissionErrorNamesProtectedMetadataOperation(t *testing.T) {
	cause := errors.New("private source value must not appear")
	for _, phase := range []string{"resource declarations", "V2 history", "journal consistency", "legacy history"} {
		err := sourceAdmissionError{phase: phase, cause: cause}
		if !strings.Contains(err.Error(), "("+phase+")") || strings.Contains(err.Error(), cause.Error()) || !errors.Is(err, cause) {
			t.Fatalf("unsafe or ambiguous admission diagnostic for %s: %v", phase, err)
		}
	}
}

// A same-target refusal reports the original owner's blocker, not only that
// convergence belongs to that owner.
func TestSameTargetRefusalReportsOriginalInspection(t *testing.T) {
	outcome := releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired, Active: "release-a", Target: "release-a",
		Code: releasetransition.CodeDependencyUnavailable, Message: "integration claude inspect in yard default: native dependency or observation unavailable", Retry: "yard update --check"}
	runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	_, err := runtime.inspectionFailure(options{}, publicReleaseInspectionError{cause: errors.New("same-target convergence must use the exact sealed original owner"), outcome: outcome})
	var failure transitionOutcomeFailure
	if !errors.As(err, &failure) || failure.outcome.Code != outcome.Code || !strings.Contains(err.Error(), outcome.Message) {
		t.Fatalf("same-target refusal lost the original blocker: %v", err)
	}
	var stdout bytes.Buffer
	check := New(Config{Stdout: &stdout, Stderr: &bytes.Buffer{}})
	defer check.Close()
	prepared, err := check.inspectionFailure(options{check: true, json: true}, publicReleaseInspectionError{cause: errors.New("refused"), outcome: outcome})
	if err != nil || prepared.Changed || prepared.run == nil {
		t.Fatalf("check refusal is not a read-only report: %#v %v", prepared, err)
	}
}

// These fixtures describe an already installed source, not initial bootstrap.
func seedExistingCurrentLedger(t *testing.T, home string, payload []byte) {
	t.Helper()
	registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.FreshBootstrapLedgerV2(registry)
	pending, err := registry.PendingPath(ledger)
	if err != nil || len(pending) != 0 {
		t.Fatal("fixture requires completed current epochs", err)
	}
	data, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "release-transition/v2/ledger.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, data, 0600)
}

// Ignore only the fixture's process/audit logs; include all installation/cache
// paths, modes and bytes. Transient staging is separately witnessed by installer-writes.
func sourceAdmissionTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "old-observe-called" || rel == "candidate-phases" || rel == "installer-writes" {
			return nil
		}
		value := info.Mode().String()
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += ":" + target
		} else if !info.IsDir() {
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += fmt.Sprintf(":%x", sha256.Sum256(payload))
		}
		files[rel] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestInitialBootstrapRoutesResumeWithAbsentLedger(t *testing.T) {
	ctx := context.Background()
	root := testkit.TempDir(t)
	home, runtimeRoot := filepath.Join(root, "config"), filepath.Join(root, "runtime")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 0.29.0\\n';; _release-transition) %s=ready exec %q -test.run '^TestSourceAdmissionCandidateProcessHelper$';; *) exit 64;; esac\n", sourceAdmissionProcessEnvironment, executable)
	candidate := writeVersionedRuntimeCandidate(t, runtimeRoot, "0.29.0-first", "0.29.0", engine)
	artifact := sealSourceAdmissionCandidate(t, candidate)
	if err := os.Symlink("releases/"+string(candidate.release), filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := false
	original, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: home, RegistryPayload: []byte(sourceAdmissionRegistry), ArtifactDigest: artifact,
		Releases: releasetransition.ReleasePair{From: candidate.release, Target: candidate.release},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			snapshot, err := store.ReadCurrentJournal()
			if err != nil {
				return releasetransition.ReleaseLinks{}, err
			}
			if snapshot.Exists && !interrupted {
				interrupted = true
				return releasetransition.ReleaseLinks{}, errors.New("stop after initial journal publication")
			}
			return links.Observe()
		},
		NewTransactionID:    func() releasetransition.TransactionID { return "tx-initial-bootstrap" },
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := original.Inspect(ctx, releasetransition.Goal{Target: candidate.release, Direction: releasetransition.DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	adapter := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": root, "YARD_RUNTIME_ROOT": runtimeRoot}), Installer: "/missing", Stdout: &output, Stderr: &bytes.Buffer{}})
	defer adapter.Close()
	preview, err := adapter.PrepareCurrentTransition(ctx, []string{"--check", "--json"}, home, "default")
	if err != nil {
		t.Fatal("initial current bootstrap refused absent authority", err)
	}
	if err := preview.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	var initialReport currentReport
	if err := json.Unmarshal(output.Bytes(), &initialReport); err != nil || initialReport.Outcome.Status != releasetransition.StatusMigrationRequired || initialReport.Outcome.ReachedGoal {
		t.Fatalf("initial bootstrap readiness: %s %v", output.String(), err)
	}
	output.Reset()
	out, err := original.Converge(ctx, releasetransition.Execution{Plan: plan.Plan, Authorization: "original-bootstrap-consent"})
	if err != nil || !interrupted || out.Status != releasetransition.StatusRecovering {
		t.Fatalf("bootstrap interruption: %#v %v", out, err)
	}
	ledger, err := store.ReadLedger()
	if err != nil || ledger.Exists {
		t.Fatalf("baseline already published: %#v %v", ledger, err)
	}
	initial, err := releasetransition.ParseJournal(mustReadFile(t, filepath.Join(home, "release-transition/v2/journal.json")))
	if err != nil {
		t.Fatal(err)
	}
	check, err := adapter.PrepareCurrentTransition(ctx, []string{"--check"}, home, "default")
	if err != nil {
		t.Fatal("current bootstrap inspection refused absent authority", err)
	}
	if err := check.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	resume, err := adapter.PrepareTransition(ctx, nil, home, "default")
	if err != nil {
		t.Fatal("installed owner could not resume initial ledger publication", err)
	}
	if !resume.RepairCurrent || resume.nativePlan != initial.ResumePlan {
		t.Fatalf("bootstrap lost its transaction: %#v", resume)
	}
	if err := resume.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(mustReadFile(t, filepath.Join(home, "release-transition/v2/journal.json")))
	if err != nil || journal.Transaction != "tx-initial-bootstrap" || journal.Checkpoint != releasetransition.JournalComplete || journal.AuthorizationPlan != initial.AuthorizationPlan || journal.AuthorizationDigest != initial.AuthorizationDigest {
		t.Fatalf("bootstrap resume: %#v %v", journal, err)
	}
	ledger, err = store.ReadLedger()
	if err != nil || !ledger.Exists {
		t.Fatalf("resumed bootstrap did not publish baseline: %v", err)
	}
}

// The real installer retries update, rather than migrate, after publishing the
// first current link and before creating configuration or a protected journal.
func TestPrepareTransitionBootstrapsPublishedCurrentWithoutHistory(t *testing.T) {
	ctx := context.Background()
	root := testkit.TempDir(t)
	home, runtimeRoot := filepath.Join(root, "config"), filepath.Join(root, "runtime")
	version := "0.29.0"
	bundle := []byte("synthetic verified bootstrap publication")
	id := fmt.Sprintf("%s-%x", version, sha256.Sum256(bundle))[:len(version)+13]
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine %s\\n';; _release-transition) %s=ready exec %q -test.run '^TestSourceAdmissionCandidateProcessHelper$';; *) exit 64;; esac\n", version, sourceAdmissionProcessEnvironment, executable)
	candidate := writeVersionedRuntimeCandidate(t, runtimeRoot, id, version, engine)
	sealSourceAdmissionCandidate(t, candidate)
	if err := os.Symlink("releases/"+id, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "cache", version)
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("subyard-%s-linux-%s.tar.gz", version, runtime.GOARCH)
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		testkit.WriteFile(t, filepath.Join(cache, name+suffix), bundle, 0600)
	}
	installer := filepath.Join(root, "installer")
	testkit.WriteFile(t, installer, []byte(fmt.Sprintf("#!/bin/sh\n# --publish-only\nwhile [ $# -gt 0 ]; do case \"$1\" in --runtime-root) shift; destination=$1;; esac; shift; done\nmkdir -p \"$destination/releases\"\ncp -a %q \"$destination/releases/%s\"\nprintf 'releases/%s\\n'\n", candidate.root, id, id)), 0700)
	adapter := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": root, "YARD_RUNTIME_ROOT": runtimeRoot, "YARD_RELEASE_CACHE": filepath.Dir(cache)}), Installer: installer, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	defer adapter.Close()
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("config already exists before first transition: %v", err)
	}
	prepared, err := adapter.PrepareTransition(ctx, []string{"--offline", "--version", version}, home, "default")
	if err != nil {
		t.Fatal("first-publication retry did not reach bootstrap", err)
	}
	if prepared.Close != nil {
		defer prepared.Close()
	}
	inspection, err := prepared.check(ctx)
	if err != nil || inspection.Outcome == nil || inspection.Outcome.Status != releasetransition.StatusMigrationRequired || inspection.Outcome.ReachedGoal {
		t.Fatalf("bootstrap retry inspection: %#v %v", inspection, err)
	}
	if !slices.ContainsFunc(inspection.Decisions, func(d releasetransition.RedactedDecision) bool {
		return d.Resource == "ledger.bootstrap" && d.Result == "initialize-ledger"
	}) {
		t.Fatal("retry did not plan the baseline publication")
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("read-only bootstrap retry created config: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(runtimeRoot, "current")); err != nil || target != "releases/"+id {
		t.Fatalf("bootstrap inspection changed current: %q %v", target, err)
	}
}
