package releasetransition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Diagnostic probe: a completed 0.32.0-era install (its retained transaction
// holds evidence for the six 0.28-era migrations) upgraded by an engine whose
// registry retires those migrations. Source admission after the upgrade must
// still prove the retained install transaction.
func TestRetire028SourceAdmissionAfterUpgrade(t *testing.T) {
	newPayload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, source string }{
		{"0.32.0/same registry", "0.32.0"},
		{"0.32.0/retired registry", "0.32.0"},
		{"0.28.0/retired registry", "0.28.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldPayload, err := os.ReadFile(filepath.Join("testdata", "v"+test.source+"-release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			successor := newPayload
			if strings.HasSuffix(test.name, "same registry") {
				successor = oldPayload
			}
			configHome := settingsV2Fixture(t, map[string]string{
				"yards/hermes/config.env": "YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n",
			})
			links := ReleaseLinks{Active: ReleaseID(test.source + "-aaaaaaaaaaaa")}
			options := V2Options{
				ConfigHome: configHome, Releases: ReleasePair{From: links.Active, Target: links.Active},
				ObserveLinks: func(context.Context) (ReleaseLinks, error) { return links, nil },
				ActivateLinks: func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
					links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
					return links, nil
				},
				RegistryPayload: oldPayload, ArtifactDigest: digestA,
				OwnerRegistration:   &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
				RegisteredYards:     []V2ActivationReconciler{&v2TestReconciler{id: "yard.hermes"}},
				GuestDiskTmp:        []V2ActivationReconciler{&v2TestReconciler{id: "yard.hermes"}},
				MigrationCheckpoint: true, SourceMigrationCheckpoint: true,
				NewTransactionID: func() TransactionID { return "tx-install" },
				VerifyAuthorization: func(plan PlanToken, authorization Authorization) bool {
					return authorization == v2TestAuthorization(plan)
				},
			}
			converge := func(options V2Options) *V2Transition {
				t.Helper()
				transition, err := NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				inspection, err := transition.Inspect(context.Background(), Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget})
				if err != nil {
					t.Fatal(err)
				}
				outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
				if err != nil || outcome.Status != StatusReady {
					t.Fatalf("converge %s: %+v %v", options.Releases.Target, outcome, err)
				}
				return transition
			}
			installed := converge(options)
			store, err := NewPOSIXV2Store(configHome)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.InspectSourceMetadata(installed.registry, installed.registryDigest); err != nil {
				t.Fatalf("installed source metadata: %v", err)
			}
			options.Releases = ReleasePair{From: links.Active, Target: "1.0.0-bbbbbbbbbbbb"}
			options.RegistryPayload = successor
			options.ArtifactDigest = digestB
			options.NewTransactionID = func() TransactionID { return "tx-upgrade-candidate" }
			upgraded := converge(options)
			retained, retainedErr := store.sourceRetainedTerminalEvidence("tx-install", upgraded.registry)
			t.Logf("retained install evidence under the successor registry: steps=%d err=%v", len(retained.Steps), retainedErr)
			if _, err := store.InspectSourceMetadata(upgraded.registry, upgraded.registryDigest); err != nil {
				t.Fatalf("source metadata after upgrade: %v", err)
			}
		})
	}
}
