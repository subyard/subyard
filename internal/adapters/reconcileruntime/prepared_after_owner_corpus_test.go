package reconcileruntime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
)

// This exports owning adapter bytes for the Rust codec/storage consumer. The
// producer tuple is synthetic; the framed native tests independently exercise
// admission of real closed facts. This is not a whole-owner fixture.
func exportAfterOwnerAdapterPair(t *testing.T, kind string, original *PreparedInstalledRuntime, physical []byte, uuid, before string) {
	t.Helper()
	path := os.Getenv("SUBYARD_AFTER_OWNER_" + strings.ToUpper(kind) + "_CORPUS")
	if path == "" {
		return
	}
	hash := strings.Repeat("a", 64)
	binding := releaseowner.Binding{Session: hash, Artifact: hash, Helper: hash, Journal: hash, Transaction: "original-owner", Plan: "plan-v1-" + hash, Ordinal: 1}
	owner, class := "test-vm-broker", "stage"
	if kind == "materialized-integrations" {
		owner, class = "materialized-config", "materialized-integrations"
	}
	if kind == "routes" {
		owner, class = "test-yard-route-consumers", "route-consumers"
	}
	target := releaseowner.Target{Owner: owner, Class: class, Yard: "test-yard", Project: "subyard-test-yard", Instance: "yard-test-yard", InstanceUUID: "absent", State: "deferred", Actual: hash, Desired: hash, Source: original.source, PreparedRecipe: original.Binding(), AfterOwner: "v1:" + kind + ":" + original.Binding(), Before: before}
	delta := releaseowner.AfterOwnerDelta{InstanceUUID: uuid, State: "stale", Actual: hash, Desired: hash, Before: before}
	if kind == "broker" {
		var value afterOwnerBrokerPhysical
		if json.Unmarshal(physical, &value) != nil {
			t.Fatal("physical corpus")
		}
		delta.Before = value.Binding
	}
	digest := func(value any) string { raw, _ := json.Marshal(value); return fmt.Sprintf("%x", sha256.Sum256(raw)) }
	metadata, _ := json.Marshal(releaseowner.AfterOwnerMetadata{Version: 1, Phase: "resolved", Original: digest(target), Producer: binding, ProducerTerminal: hash, Delta: delta, Physical: physical})
	derived, err := original.WithAfterOwnerMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	resolved := delta.Apply(target, derived.Binding())
	rows := []any{map[string]any{"name": kind, "original": map[string]any{"raw": original.Recipe(), "sha256": original.Binding(), "target": target}, "resolved": map[string]any{"raw": derived.Recipe(), "sha256": derived.Binding(), "target": resolved}, "producer": binding, "producerTerminal": hash, "ownerOrdinal": 1}}
	raw, _ := json.Marshal(rows)
	testkit.WriteFile(t, path, raw, 0600)
}
