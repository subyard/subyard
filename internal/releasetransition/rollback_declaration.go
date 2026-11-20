package releasetransition

import (
	"encoding/json"
	"slices"
)

const (
	RollbackDeclarationSchemaV1 = 1
	MaxRollbackDeclarationBytes = 64 << 10
)

// RegistryFeatureRetiredPrefixDependencies means the release parses a registry
// whose live migrations depend on definitions retired into a proved prefix.
const RegistryFeatureRetiredPrefixDependencies = "retired-prefix-dependencies"

// RegistryReaderFeatures lists, sorted, the registry forms this release's
// compiled parser reads beyond the v0.30.0 form. Its own sealed declaration
// lists exactly these (checked at packaging time), so a newer release can ask a
// retained target what it reads. A reader ignores features it does not know.
func RegistryReaderFeatures() []string {
	return []string{RegistryFeatureRetiredPrefixDependencies}
}

// RollbackDeclaration is a release's sealed config/release-rollback.json. It
// names the one predecessor registry the release prepares rollback to, by
// canonical digest. Schema 1 defines no migration or profile-format entries yet,
// so both lists must be present and empty. RegistryFeatures names the registry
// forms the release reads; an absent list, or a release without a declaration,
// reads only the v0.30.0 form.
type RollbackDeclaration struct {
	SchemaVersion             int               `json:"schemaVersion"`
	PredecessorRegistryDigest Fingerprint       `json:"predecessorRegistryDigest"`
	Migrations                []json.RawMessage `json:"migrations"`
	ProfileFormats            []json.RawMessage `json:"profileFormats"`
	RegistryFeatures          []string          `json:"registryFeatures,omitempty"`
}

// ReadsRegistryFeature reports whether the declaring release reads that registry form.
func (declaration RollbackDeclaration) ReadsRegistryFeature(feature string) bool {
	return slices.Contains(declaration.RegistryFeatures, feature)
}

func ParseRollbackDeclaration(payload []byte) (RollbackDeclaration, error) {
	var declaration RollbackDeclaration
	if err := decodeBoundedRecord(payload, MaxRollbackDeclarationBytes, &declaration); err != nil {
		return RollbackDeclaration{}, invalid("decode rollback declaration: %v", err)
	}
	if declaration.SchemaVersion != RollbackDeclarationSchemaV1 {
		return RollbackDeclaration{}, invalid("unsupported rollback declaration schema %d", declaration.SchemaVersion)
	}
	if err := validateFingerprint(declaration.PredecessorRegistryDigest, "rollback predecessor registry digest"); err != nil {
		return RollbackDeclaration{}, err
	}
	if declaration.Migrations == nil || declaration.ProfileFormats == nil {
		return RollbackDeclaration{}, invalid("rollback declaration must list migrations and profile formats")
	}
	if len(declaration.Migrations) != 0 || len(declaration.ProfileFormats) != 0 {
		return RollbackDeclaration{}, invalid("rollback declaration schema 1 defines no migration or profile-format entries")
	}
	return declaration, nil
}

// CheckOwnRollbackDeclaration is the packaging check of a release's own
// declaration: it must parse and claim no registry feature this release's
// compiled parser does not implement. Another release's declaration is read
// tolerantly with ParseRollbackDeclaration instead.
func CheckOwnRollbackDeclaration(payload []byte) error {
	declaration, err := ParseRollbackDeclaration(payload)
	if err != nil {
		return err
	}
	implemented := RegistryReaderFeatures()
	for index, feature := range declaration.RegistryFeatures {
		if !slices.Contains(implemented, feature) {
			return invalid("rollback declaration claims registry feature %q this release does not implement", feature)
		}
		if index > 0 && declaration.RegistryFeatures[index-1] >= feature {
			return invalid("rollback declaration registry features must be sorted and unique")
		}
	}
	return nil
}
