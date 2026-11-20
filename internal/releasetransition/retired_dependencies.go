package releasetransition

import (
	_ "embed"
	"encoding/json"
)

// The six historical migration definitions, byte-identical to their v0.28.0
// registry. A later registry may retire them into prefixes while its live
// definitions still depend on them; this release must read that registry.
//
//go:embed history_v028.json
var historyV028Payload []byte

const historyV028Digest Fingerprint = "372ccefc089e8b31099df7e08d8a783f2dbf5af5dbaf8e4afa6f41c6a4f1f28e"

var historyV028 = func() RegistryV2 {
	var registry RegistryV2
	if fingerprintPayload(historyV028Payload) != historyV028Digest || json.Unmarshal(historyV028Payload, &registry) != nil {
		panic("invalid frozen v0.28 migration history")
	}
	return registry
}()

// retiredMigrationIDs names the historical definitions a retired prefix covers.
// A prefix counts only when its digest proves exactly those definitions, so a
// matching domain and epoch alone never satisfy a dependency. A retired
// definition counts as completed only when each of its own frozen dependencies
// is retired too: a live dependency is still pending at the floor.
func (registry RegistryV2) retiredMigrationIDs() []string {
	proved := map[string]bool{}
	for domain, prefix := range registry.RetiredPrefixes {
		if prefix.ThroughEpoch > historyV028.CurrentEpochs[domain] {
			continue
		}
		digest, err := historyV028.MigrationPrefixDigest(domain, prefix.ThroughEpoch)
		if err != nil || digest != prefix.Digest {
			continue
		}
		for _, migration := range historyV028.Migrations {
			if migration.Domain == domain && migration.ToEpoch <= prefix.ThroughEpoch {
				proved[migration.ID] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, migration := range historyV028.Migrations {
			if !proved[migration.ID] {
				continue
			}
			for _, dependency := range migration.DependsOn {
				if !proved[dependency] {
					delete(proved, migration.ID)
					changed = true
					break
				}
			}
		}
	}
	ids := []string{}
	for _, migration := range historyV028.Migrations {
		if proved[migration.ID] {
			ids = append(ids, migration.ID)
		}
	}
	return ids
}
