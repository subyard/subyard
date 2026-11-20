package releasetransition

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

// retiringRegistry re-expresses this release's registry the way a successor
// retires the six historical migrations: the floor rises to their top epochs,
// only the NetworkManager device guard stays live and still depends on them.
func retiringRegistry(t *testing.T) RegistryV2 {
	t.Helper()
	payload, err := os.ReadFile("../../config/release-transition.json")
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	retired := RegistryV2{SchemaVersion: registry.SchemaVersion, MinimumEpochs: map[string]int{}, CurrentEpochs: registry.CurrentEpochs, RetiredPrefixes: map[string]RetiredMigrationPrefixV2{}}
	for domain, minimum := range registry.MinimumEpochs {
		retired.MinimumEpochs[domain] = minimum
		top := historyV028.CurrentEpochs[domain]
		if top == minimum {
			continue
		}
		digest, err := registry.MigrationPrefixDigest(domain, top)
		if err != nil {
			t.Fatal(err)
		}
		retired.MinimumEpochs[domain] = top
		retired.RetiredPrefixes[domain] = RetiredMigrationPrefixV2{ThroughEpoch: top, Digest: digest}
	}
	for _, migration := range registry.Migrations {
		if migration.ToEpoch > historyV028.CurrentEpochs[migration.Domain] {
			retired.Migrations = append(retired.Migrations, migration)
		}
	}
	if len(retired.Migrations) != 1 || retired.Migrations[0].ID != "introduce-nm-device-guard-v4" ||
		!strings.HasPrefix(retired.Migrations[0].DependsOn[0], "complete-registered-guest-disk-tmp") {
		t.Fatalf("unexpected live successor definitions: %#v", retired.Migrations)
	}
	return retired
}

func TestRegistryAcceptsDependencyOnProvedRetiredPrefix(t *testing.T) {
	retired := retiringRegistry(t)
	payload, err := json.Marshal(retired)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatalf("successor registry with retired dependencies: %v", err)
	}
	owner, err := parsed.historyRegistry()
	if err != nil {
		t.Fatal(err)
	}
	// The floor satisfies the retired dependency in both pending and applied history.
	floor, err := owner.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := owner.Pending(floor); err != nil || len(pending) != 1 {
		t.Fatalf("pending from the floor: %v %v", pending, err)
	}
	domains := floor.Domains()
	domains["power-metadata"] = migrationhistory.DomainState{Epoch: 4, Applied: []string{"introduce-nm-device-guard-v4"}}
	if _, err := owner.History(domains); err != nil {
		t.Fatalf("applied live step after a retired dependency: %v", err)
	}
}

func TestRegistryRefusesRetiredDependencyWithoutPrefixProof(t *testing.T) {
	for _, damage := range []string{"digest", "epoch-only"} {
		t.Run(damage, func(t *testing.T) {
			retired := retiringRegistry(t)
			prefix := retired.RetiredPrefixes["power-metadata"]
			if damage == "digest" {
				prefix.Digest = Fingerprint(strings.Repeat("a", 64))
			} else {
				// The settings digest at the same epoch proves other definitions.
				prefix.Digest = retired.RetiredPrefixes["settings"].Digest
			}
			retired.RetiredPrefixes["power-metadata"] = prefix
			payload, err := json.Marshal(retired)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = ParseRegistryV2(payload, BuiltinCapabilityCatalog())
			if err == nil || !strings.Contains(err.Error(), "dependency") {
				t.Fatalf("unproved retired dependency was admitted: %v", err)
			}
		})
	}
}
