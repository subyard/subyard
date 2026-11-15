package releasetransition

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
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

type transientRegisteredOutputOwner struct {
	registeredOutputOwner
	refuse bool
}

func (owner *transientRegisteredOutputOwner) Observe(ctx context.Context, releases ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	if owner.refuse {
		owner.refuse = false
		return V2ActivationObservation{}, &registeredVerdict{"yard hermes: temporary endpoint refusal"}
	}
	return owner.nativeConsentOwner.Observe(ctx, releases, links)
}
