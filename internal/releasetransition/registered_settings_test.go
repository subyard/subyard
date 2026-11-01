package releasetransition

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

type registeredOutputOwner struct {
	*nativeConsentOwner
	path    string
	content []byte
}

func (owner registeredOutputOwner) PreparedInputPaths() ([]string, error) {
	return []string{owner.path}, nil
}
func (owner registeredOutputOwner) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	return config.PersistentFileSnapshot{Exists: true, Content: owner.content}, path == owner.path, nil
}

func TestRegisteredEndpointWorkRejectsUndeclaredOrUnrelatedChanges(t *testing.T) {
	for _, scenario := range []string{"valid", "missing-vocabulary", "core-setting", "foreign-path", "existing-value", "unrelated-record"} {
		t.Run(scenario, func(t *testing.T) {
			transition, home, _ := v2TransitionFixture(t, nil)
			path := filepath.Join(home, "yards/hermes/config.env")
			testkit.WriteFile(t, path, []byte("# preserve exactly\n"), 0600)
			transition.options.RegisteredEndpointSettings = []string{"OLD_SAMPLE_HOST"}
			value := "127.0.0.1"
			content, err := config.EditPersistentAssignmentContent(path, []byte("# preserve exactly\n"), "OLD_SAMPLE_HOST", &value)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing-vocabulary":
				transition.options.RegisteredEndpointSettings = nil
			case "core-setting":
				content = []byte("# preserve exactly\nSSH_PORT=2240\n")
			case "foreign-path":
				path = filepath.Join(home, "config.env")
			case "existing-value":
				testkit.WriteFile(t, path, []byte("OLD_SAMPLE_HOST=localhost\n"), 0600)
			case "unrelated-record":
				content = []byte("# changed comment\nOLD_SAMPLE_HOST=127.0.0.1\n")
			}
			owner := registeredOutputOwner{nativeConsentOwner: &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.hermes"}}, path: path, content: content}
			transition.options.RegisteredYards = []V2ActivationReconciler{owner}
			plan, err := transition.inspectRegisteredSettings(context.Background(), MigrationDefinitionV2{Kind: registeredYardsMigration, Domain: "owner-registration", FromEpoch: 2, ToEpoch: 3}, nil, ReleaseLinks{})
			if scenario == "valid" {
				if err != nil || len(plan.Files) != 1 {
					t.Fatalf("valid original descriptor vocabulary rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("unsafe prepared settings accepted")
			}
		})
	}
}

func TestRegisteredEndpointTerminalRecoveryRequiresOriginalVocabulary(t *testing.T) {
	for _, scenario := range []string{"valid-original-name", "missing-vocabulary", "core-setting", "prior-value"} {
		t.Run(scenario, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			journal := JournalRecord{Transaction: "tx-original"}
			step := JournalStep{ID: "registered.hermes", Migration: "registered", Resource: "hermes", Expected: digestA}
			recovery := settingsRecoveryV1{SchemaVersion: 2, Kind: registeredYardsMigration, Transaction: journal.Transaction, Step: step.ID, Migration: step.Migration, Yard: step.Resource, Expected: step.Expected, Before: []settingsV2BeforeValue{{Setting: "OLD_SAMPLE_HOST"}}}
			allowed := []string{"OLD_SAMPLE_HOST"}
			if scenario == "missing-vocabulary" {
				allowed = nil
			}
			if scenario == "core-setting" {
				recovery.Before[0].Setting = "SSH_PORT"
			}
			if scenario == "prior-value" {
				recovery.Before[0].Present = true
				recovery.Before[0].Value = "localhost"
			}
			payload, err := json.Marshal(recovery)
			if err != nil {
				t.Fatal(err)
			}
			if err := transition.store.CreateRecovery(journal.Transaction, step.ID, append(payload, '\n')); err != nil {
				t.Fatal(err)
			}
			snapshot, err := transition.store.ReadRecovery(journal.Transaction, step.ID)
			if err != nil {
				t.Fatal(err)
			}
			step.Evidence = &EvidenceRecord{Recovery: snapshot.Fingerprint}
			transition.store.endpointSettings = allowed
			err = transition.store.sourceTypedRecovery(journal, step, MigrationDefinitionV2{Kind: registeredYardsMigration, Domain: "owner-registration", FromEpoch: 2, ToEpoch: 3})
			if scenario == "valid-original-name" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("uncompiled terminal setting accepted")
			}
		})
	}
}
