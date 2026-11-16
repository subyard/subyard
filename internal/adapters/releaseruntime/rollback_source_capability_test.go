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

func bindRollbackFixtureRegistry(t *testing.T, candidate publishedCandidate, registry []byte) {
	t.Helper()
	testkit.WriteFile(t, filepath.Join(candidate.root, "config/release-transition.json"), registry, 0600)
	engine := mustReadFile(t, filepath.Join(candidate.root, "bin/yard-engine"))
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", sha256.Sum256(engine), sha256.Sum256(registry))
	testkit.WriteFile(t, filepath.Join(candidate.root, "runtime-files.sha256"), []byte(manifest), 0600)
}

func TestFreshRollbackOpaqueWorkRequiresSourceCapability(t *testing.T) {
	// The retired native handlers cannot be authorized through an older target's
	// capability probe. Source admission refuses before target selection/effects.
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%t", rollback), func(t *testing.T) {
			root := testkit.TempDir(t)
			home := filepath.Join(root, "config")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			runtimeRoot := filepath.Join(root, "runtime")
			candidate := writeVersionedRuntimeCandidate(t, runtimeRoot, "0.28.1-aaaaaaaaaaaa", "0.28.1", `#!/bin/sh
case "$1" in --version) echo 'yard-engine 0.28.1';; *) exit 71;; esac
`)
			payload := mustReadFile(t, filepath.Join("..", "..", "releasetransition", "history_v028.json"))
			bindRollbackFixtureRegistry(t, candidate, payload)
			if err := os.Symlink("releases/"+string(candidate.release), filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			ledger, _, err := releasetransition.MarshalLedgerV2(releasetransition.FreshBootstrapLedgerV2(registry), registry)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "release-transition/v2/ledger.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, ledger, 0600)
			runtime := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": root, "YARD_RUNTIME_ROOT": runtimeRoot}), Stdout: &bytes.Buffer{}})
			defer runtime.Close()
			args := []string{"--runtime-root", runtimeRoot}
			if rollback {
				args = append(args, "--rollback")
			}
			_, err = runtime.PrepareTransition(context.Background(), args, home, "default")
			if err == nil || !strings.Contains(err.Error(), "migration-manual-repair-required:") {
				t.Fatalf("below-floor source was admitted: %v", err)
			}
			if !bytes.Equal(ledger, mustReadFile(t, path)) {
				t.Fatal("refusal rewrote source")
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
				runtime := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": root}), Stdout: &stdout, Stderr: &bytes.Buffer{}})
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
		{name: "aware-opaque-owner", queried: false, probe: "fixed", allowed: true},
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
			runtime := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": root}), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
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
				prepared, err = runtime.prepareVerifiedTransition(context.Background(), options{root: root}, verifiedOwner, verifiedTarget, request)
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
