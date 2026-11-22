package releasetransition

import (
	_ "embed"
	"encoding/json"
)

// This exact published registry bounds the additional reader language. It is
// independent of the current release registry, which later releases may change.
//
//go:embed testdata/v0.30.0-release-transition.json
var frozenV030Payload []byte

var frozenV030Registry = func() RegistryV2 {
	var registry RegistryV2
	if fingerprintPayload(frozenV030Payload) != "3ee060450263c840853ecb639200a6e4a02b52c7a70e7e8c5d35eade99091de2" ||
		json.Unmarshal(frozenV030Payload, &registry) != nil {
		panic("invalid frozen v0.30 migration registry")
	}
	return registry
}()

// closedRetiredMigrationIDs is used only after strict parsing fails. Whole
// registry equality excludes foreign definitions and reused IDs. Closure is
// all-or-nothing: a floor cannot complete a step whose dependency remains live.
func (registry RegistryV2) closedRetiredMigrationIDs() ([]string, error) {
	digest, err := registry.CanonicalDigest()
	if err != nil {
		return nil, err
	}
	if equal, err := retiresPrefixesOf(registry, frozenV030Registry, digest); err != nil || !equal {
		return nil, invalid("registry is not an exact retirement of the frozen v0.30 registry")
	}
	retired := map[string]bool{}
	var ids []string
	for _, migration := range frozenV030Registry.Migrations {
		if migration.ToEpoch <= registry.MinimumEpochs[migration.Domain] {
			retired[migration.ID] = true
			ids = append(ids, migration.ID)
		}
	}
	for _, migration := range frozenV030Registry.Migrations {
		if retired[migration.ID] {
			for _, dependency := range migration.DependsOn {
				if !retired[dependency] {
					return nil, invalid("retired migration %q depends on live migration %q", migration.ID, dependency)
				}
			}
		}
	}
	return ids, nil
}
