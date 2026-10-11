package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"errors"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

const sourceAdmissionProcessEnvironment = "SUBYARD_TEST_SOURCE_ADMISSION_PROCESS"
const sourceAdmissionRegistry = `{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`

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
	adapter := New(Config{Environment: map[string]string{"HOME": filepath.Dir(request.RuntimeRoot)}})
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
	for _, drift := range []string{"none", "journal replacement", "catalog", "registry", "sealed asset", "unfinished", "unfinished same target", "current repair", "current repair old-wire", "current repair replacement", "private draft", "private draft parent replacement", "private draft parent mode", "private draft immutable hardlink"} {
		t.Run(drift, func(t *testing.T) {
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
			if strings.HasPrefix(drift, "current repair") || strings.HasPrefix(drift, "private draft") {
				mode := "repair"
				if drift == "current repair old-wire" {
					mode = "old-wire"
				}
				if strings.HasPrefix(drift, "private draft") {
					mode = "ready"
				}
				oldEngine = fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.3\\n';; _release-transition) printf called >> %q; %s=%s exec %q -test.run '^TestSourceAdmissionCandidateProcessHelper$';; *) exit 64;; esac\n", oldCalls, sourceAdmissionProcessEnvironment, mode, executable)
			}
			old := writeVersionedRuntimeCandidate(t, runtimeRoot, "1.2.3-original", "1.2.3", oldEngine)
			artifact := sealSourceAdmissionCandidate(t, old)
			links, _ := releasetransition.NewRuntimeLinkStore(runtimeRoot)
			original, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: home,
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
			registry, _, err := releasetransition.ParseRegistryV2([]byte(sourceAdmissionRegistry), releasetransition.BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			ledger, _, err := releasetransition.MarshalLedgerV2(releasetransition.BaselineLedgerV2(registry), registry)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(home, "release-transition", "v2", "ledger.json"), ledger, 0o600)
			journalPath := filepath.Join(home, "release-transition", "v2", "journal.json")
			before := mustReadFile(t, journalPath)
			bundle := []byte("synthetic verified publication input")
			candidateID := fmt.Sprintf("1.2.4-%x", sha256.Sum256(bundle))[:18]
			candidateRoot := runtimeRoot
			if strings.HasPrefix(drift, "private draft") {
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
			installer := filepath.Join(root, "installer")
			testkit.WriteFile(t, installer, []byte(fmt.Sprintf("#!/bin/sh\n# --publish-only\nwhile [ $# -gt 0 ]; do case \"$1\" in --runtime-root) shift; destination=$1;; esac; shift; done\nmkdir -p \"$destination/releases\"\ncp -a %q \"$destination/releases/%s\"\nprintf 'releases/%s\\n'\n", candidate.root, candidateID, candidateID)), 0o700)
			adapter := New(Config{Environment: map[string]string{"HOME": root, "YARD_RUNTIME_ROOT": runtimeRoot, "YARD_RELEASE_CACHE": filepath.Dir(cache)}, Installer: installer, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
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
			prepared, err := adapter.PrepareTransition(ctx, []string{"--offline", "--version", version}, home, "default", nil)
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
