package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func bindRollbackFixtureRegistry(t *testing.T, candidate publishedCandidate, registry []byte) {
	t.Helper()
	testkit.WriteFile(t, filepath.Join(candidate.root, "config/release-transition.json"), registry, 0600)
	engine := mustReadFile(t, filepath.Join(candidate.root, "bin/yard-engine"))
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", sha256.Sum256(engine), sha256.Sum256(registry))
	testkit.WriteFile(t, filepath.Join(candidate.root, "runtime-files.sha256"), []byte(manifest), 0600)
}

func TestFreshRollbackOpaqueWorkRequiresSourceCapability(t *testing.T) {
	for _, kind := range []string{"registered-yards-v2-to-v3", "registered-guest-disk-tmp-v2-to-v3", "complete", "newer-checkpoint", "pending-checkpoint", "invalid-ledger", "invalid-checkpoint", "invalid-registry", "unknown-kind", "known-old-pending"} {
		t.Run(kind, func(t *testing.T) {
			root := testkit.TempDir(t)
			home := filepath.Join(root, "config")
			if err := os.MkdirAll(filepath.Join(home, "release-transition/v2"), 0700); err != nil {
				t.Fatal(err)
			}
			registryPayload := mustReadFile(t, filepath.Join("..", "..", "..", "config/release-transition.json"))
			registry, digest, err := releasetransition.ParseRegistryV2(registryPayload, releasetransition.BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			if kind == "known-old-pending" {
				registry.Migrations = registry.Migrations[:4]
				registry.CurrentEpochs["owner-registration"], registry.CurrentEpochs["power-metadata"] = 2, 2
				registryPayload, err = json.Marshal(registry)
				if err != nil {
					t.Fatal(err)
				}
			}
			ledger := releasetransition.BaselineLedgerV2(registry)
			for _, migration := range registry.Migrations {
				if migration.Kind == kind || kind == "known-old-pending" || kind == "pending-checkpoint" && migration.Kind == "registered-guest-disk-tmp-v2-to-v3" {
					break
				}
				ledger, err = ledger.Advance(registry, migration)
				if err != nil {
					t.Fatal(err)
				}
			}
			ledgerPayload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
			if err != nil {
				t.Fatal(err)
			}
			ledgerPath := filepath.Join(home, "release-transition/v2/ledger.json")
			checkpointPath := filepath.Join(home, "release-transition/v2/history-checkpoint.json")
			testkit.WriteFile(t, ledgerPath, ledgerPayload, 0600)
			if kind == "newer-checkpoint" || kind == "pending-checkpoint" {
				if kind == "newer-checkpoint" {
					// A newer producer compacts a completed prefix without rewriting
					// the legacy projection. The exact older target reads that proof.
					registry.CurrentEpochs["power-metadata"] = 4
					registry.Migrations = append(registry.Migrations, releasetransition.MigrationDefinitionV2{ID: "future-publication-ledger-v4", Domain: "power-metadata", FromEpoch: 3, ToEpoch: 4, Kind: "owner-publication-state-v1"})
					newer, err := json.Marshal(registry)
					if err != nil {
						t.Fatal(err)
					}
					registry, digest, err = releasetransition.ParseRegistryV2(newer, releasetransition.BuiltinCapabilityCatalog())
					if err != nil {
						t.Fatal(err)
					}
				}
				store, err := releasetransition.NewPOSIXV2Store(home)
				if err != nil {
					t.Fatal(err)
				}
				state, err := store.ReadMigrationLedger(registry, digest)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.ConvertMigrationLedger(state, registry, digest, registry, digest); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "invalid-ledger" || kind == "known-old-pending" {
				// The known old producer contract does not require a ledger read.
				testkit.WriteFile(t, ledgerPath, []byte("{invalid"), 0600)
			}
			if kind == "invalid-checkpoint" {
				testkit.WriteFile(t, checkpointPath, []byte("{invalid"), 0600)
			}
			if kind == "invalid-registry" {
				registryPayload = []byte("{}")
			}
			if kind == "unknown-kind" {
				registryPayload = bytes.ReplaceAll(registryPayload, []byte("registered-yards-v2-to-v3"), []byte("unimplemented-owner-v4"))
			}
			response := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the inspected rollback"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","previous":"release-b","target":"release-b","code":"transition-required","message":"confirmation required","retry":"confirm the inspected rollback"}}}`
			candidate := writeRuntimeCandidateFixture(t, root, response)
			bindRollbackFixtureRegistry(t, candidate, registryPayload)
			for link, target := range map[string]string{"current": "releases/release-a", "previous": "releases/release-b"} {
				if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
					t.Fatal(err)
				}
			}
			sourcePath := filepath.Join(home, "config.env")
			testkit.WriteFile(t, sourcePath, []byte("SSH_PORT=2244\n"), 0600)
			beforeLedger := mustReadFile(t, ledgerPath)
			beforeCheckpoint, beforeErr := os.ReadFile(checkpointPath)
			request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: root, ConfigHome: home, Target: "release-b", Direction: releasetransition.DirectionActivatePrevious}
			var output bytes.Buffer
			runtime := New(Config{Stdout: &output, Stderr: &output})
			defer runtime.Close()
			prepared, err := prepareCandidateTransitionForTest(runtime, context.Background(), options{root: root}, candidate, request)
			allowed := kind == "complete" || kind == "newer-checkpoint" || kind == "known-old-pending"
			if allowed {
				if err != nil || prepared.run == nil {
					t.Fatalf("compatible history refused: prepared=%+v err=%v", prepared, err)
				}
			} else {
				var refusal publicReleaseInspectionError
				if !errors.As(err, &refusal) || refusal.outcome.Code != releasetransition.CodeRollbackIncompatible || prepared.run != nil {
					t.Fatalf("opaque pending work exposed an unsupported owner for consent: prepared=%+v err=%v", prepared, err)
				}
			}
			if !bytes.Equal(mustReadFile(t, ledgerPath), beforeLedger) || string(mustReadFile(t, sourcePath)) != "SSH_PORT=2244\n" || output.Len() != 0 {
				t.Fatal("rollback inspection changed protected state or requested consent")
			}
			afterCheckpoint, afterErr := os.ReadFile(checkpointPath)
			if !bytes.Equal(beforeCheckpoint, afterCheckpoint) || (beforeErr == nil) != (afterErr == nil) {
				t.Fatal("rollback inspection changed checkpoint authority")
			}
			for link, target := range map[string]string{"current": "releases/release-a", "previous": "releases/release-b"} {
				if actual, err := os.Readlink(filepath.Join(root, link)); err != nil || actual != target {
					t.Fatal("rollback inspection changed stable links", actual, err)
				}
			}
			if _, err := os.Stat(filepath.Join(home, "release-transition/v2/journal.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rollback inspection published a journal", err)
			}
		})
	}
}

func TestFreshRollbackRequiresSourceActivationCapabilityBeforeConsent(t *testing.T) {
	for _, scope := range []string{"yard", "yard.test-yard", "owner-registration", "source-install"} {
		for _, check := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/check=%t", scope, check), func(t *testing.T) {
				root := t.TempDir()
				response := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the inspected rollback"]},"decisions":[{"resource":"test-resource","scope":%q,"decision":"canonicalize","result":"pending"}],"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","previous":"release-b","target":"release-b","code":"transition-required","message":"confirmation required","retry":"confirm the inspected rollback"}}}`
				candidate := writeRuntimeCandidateFixture(t, root, fmt.Sprintf(response, scope))
				request := releasetransition.ProcessRequest{
					SchemaVersion: 1, Mode: releasetransition.ProcessInspect,
					RuntimeRoot: root, ConfigHome: root, Target: "release-b", Direction: releasetransition.DirectionActivatePrevious,
				}
				var stdout bytes.Buffer
				runtime := New(Config{Stdout: &stdout, Stderr: &bytes.Buffer{}})
				defer runtime.Close()
				prepared, err := prepareCandidateTransitionForTest(runtime, context.Background(), options{root: root, check: check, json: true}, candidate, request)
				if check {
					if err != nil {
						t.Fatal(err)
					}
					if err := prepared.Execute(context.Background()); err != nil {
						t.Fatal(err)
					}
					var inspection releasetransition.Inspection
					if err := json.Unmarshal(stdout.Bytes(), &inspection); err != nil || inspection.Outcome == nil || inspection.Outcome.Code != releasetransition.CodeRollbackIncompatible || inspection.Outcome.Message != "the retained release cannot safely bind pending source work to rollback activation" || inspection.Outcome.Retry != "run yard migrate to complete current-release migrations, then retry yard update --rollback; otherwise retain a release with compatible rollback source planning" {
						t.Fatalf("check did not render source rollback refusal: %s err=%v", stdout.String(), err)
					}
				} else {
					var refusal publicReleaseInspectionError
					if !errors.As(err, &refusal) || refusal.outcome.Code != releasetransition.CodeRollbackIncompatible || prepared.run != nil {
						t.Fatalf("fresh source rollback exposed an unsupported owner for consent: prepared=%+v err=%v", prepared, err)
					}
				}
				if strings.Contains(stdout.String(), "confirmed") {
					t.Fatal("inspection requested consent")
				}
			})
		}
	}
}

func TestRollbackSourceCapabilityPreservesCompatibleAndPinnedPaths(t *testing.T) {
	for _, scenario := range []struct {
		name, scope, decision, probe                  string
		resume, registryless, stale, allowed, queried bool
	}{
		{name: "aware-opaque-owner", queried: true, probe: "fixed", allowed: true},
		{name: "aware-owner", queried: true, scope: "yard.test-yard", decision: "reset", probe: "fixed", allowed: true},
		{name: "unknown-contract", queried: true, scope: "owner-registration", decision: "canonicalize", probe: "unknown"},
		{name: "malformed-capability", queried: true, scope: "yard", decision: "canonicalize", probe: "malformed"},
		{name: "unknown-source-scope", queried: true, scope: "future-source", decision: "transform", probe: "unsupported"},
		{name: "no-source-work", probe: "unsupported", allowed: true},
		{name: "preserved-source", scope: "yard.test-yard", decision: "preserve", probe: "unsupported", allowed: true},
		{name: "ledger-only", scope: "power-metadata", decision: "transform", probe: "unsupported", allowed: true},
		{name: "history-only", scope: "migration-history", decision: "canonicalize", probe: "unsupported", allowed: true},
		{name: "identity-only", scope: "legacy-v1", decision: "canonicalize", probe: "unsupported", allowed: true},
		{name: "activation-only", scope: "activation", decision: "canonicalize", probe: "unsupported", allowed: true},
		{name: "original-inflight-owner", scope: "owner-registration", decision: "canonicalize", probe: "unsupported", resume: true, allowed: true},
		{name: "registryless-active-owner", scope: "yard.test-yard", decision: "reset", probe: "unsupported", registryless: true, allowed: true},
		{name: "sealed-owner-changed", queried: true, scope: "yard", decision: "canonicalize", probe: "fixed", stale: true, allowed: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			var response releasetransition.ProcessResponse
			if err := json.Unmarshal([]byte(`{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the inspected rollback"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","previous":"release-b","target":"release-b","code":"transition-required","message":"confirmation required","retry":"confirm the inspected rollback"}}}`), &response); err != nil {
				t.Fatal(err)
			}
			if scenario.scope != "" {
				response.Inspection.Decisions = []releasetransition.RedactedDecision{{Resource: "test-resource", Scope: scenario.scope, Decision: releasetransition.Decision(scenario.decision)}}
			}
			if scenario.resume {
				transaction := releasetransition.TransactionID("tx-" + strings.Repeat("a", 32))
				response.Inspection.Resume = &transaction
				response.Inspection.Plan = releasetransition.PlanToken("resume-v1-" + strings.Repeat("a", 64))
				response.Inspection.Outcome.Transaction = &transaction
				response.Inspection.Outcome.Status = releasetransition.StatusRecovering
				response.Inspection.Outcome.Code = releasetransition.CodeVerificationFailed
			}
			inspection, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			probe := "exit 64"
			switch scenario.probe {
			case "fixed":
				probe = `printf '%s\n' '{"schemaVersion":2,"capabilities":{"contract":"rollback-source-activation-v1","processSchema":2,"journalSchema":2,"receiptSchema":0}}'`
			case "unknown":
				probe = `printf '%s\n' '{"schemaVersion":2,"capabilities":{"contract":"unrecognized","processSchema":2,"journalSchema":2,"receiptSchema":0}}'`
			case "malformed":
				probe = `printf 'malformed\n'`
			}
			capture := filepath.Join(root, "capability-probe")
			effect := filepath.Join(root, "converged-effect")
			engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine 1.2.3\\n';;\n _release-transition) request=$(cat); case \"$request\" in\n *rollback-source-activation-capabilities*) printf 'probe\\n' >> %q; %s;;\n *'\"mode\":\"converge\"'*) printf 'effect\\n' > %q; exit 64;;\n *) printf '%%s\\n' %q;;\n esac;;\n *) exit 64;;\nesac\n", capture, probe, effect, inspection)
			candidate := writeRuntimeCandidatePayload(t, root, engine)
			registryPayload := []byte(sourceAdmissionRegistry)
			if scenario.name == "aware-opaque-owner" {
				registryPayload = mustReadFile(t, filepath.Join("..", "..", "..", "config/release-transition.json"))
			}
			bindRollbackFixtureRegistry(t, candidate, registryPayload)
			request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: root, ConfigHome: root, Target: "release-b", Direction: releasetransition.DirectionActivatePrevious}
			runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
			defer runtime.Close()
			var prepared Prepared
			if scenario.registryless {
				owner := writeVersionedRuntimeCandidate(t, root, "release-a", "1.2.3", engine)
				unbindCandidateRegistry(t, candidate, true)
				verifiedOwner, verifyErr := runtime.verifyPublishedCandidate(context.Background(), owner, root, nil)
				if verifyErr != nil {
					t.Fatal(verifyErr)
				}
				defer verifiedOwner.Close()
				verifiedTarget, verifyErr := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
				if verifyErr != nil {
					t.Fatal(verifyErr)
				}
				defer verifiedTarget.Close()
				request.ArtifactDigest = verifiedTarget.manifestDigest
				prepared, err = runtime.prepareVerifiedTransition(context.Background(), options{root: root}, verifiedOwner, verifiedTarget, request, nil)
			} else {
				prepared, err = prepareCandidateTransitionForTest(runtime, context.Background(), options{root: root}, candidate, request)
			}
			if (err == nil) != scenario.allowed {
				t.Fatalf("rollback capability decision: allowed=%t prepared=%+v err=%v", scenario.allowed, prepared, err)
			}
			queried, readErr := os.ReadFile(capture)
			wantProbe := scenario.queried
			if (readErr == nil) != wantProbe || readErr == nil && string(queried) != "probe\n" {
				t.Fatalf("unexpected capability query: wanted=%t bytes=%q err=%v", wantProbe, queried, readErr)
			}
			if scenario.stale {
				testkit.WriteFile(t, filepath.Join(candidate.root, "bin/yard-engine"), []byte(engine+"\n# changed\n"), 0700)
				if err := prepared.Execute(context.Background()); err == nil {
					t.Fatal("changed sealed owner executed")
				}
			}
			if _, err := os.Stat(effect); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rollback executed before exact owner check", err)
			}
		})
	}
}
