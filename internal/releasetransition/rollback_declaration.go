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
// lists no others (checked at packaging time), so a newer release can ask a
// retained target what it reads.
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

// ParseRollbackDeclaration parses a release's own declaration strictly: it
// claims no registry feature this release's compiled parser does not implement.
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
	implemented := RegistryReaderFeatures()
	for index, feature := range declaration.RegistryFeatures {
		if !slices.Contains(implemented, feature) {
			return RollbackDeclaration{}, invalid("rollback declaration claims registry feature %q this release does not implement", feature)
		}
		if index > 0 && declaration.RegistryFeatures[index-1] >= feature {
			return RollbackDeclaration{}, invalid("rollback declaration registry features must be sorted and unique")
		}
	}
	return declaration, nil
}

// RollbackPredecessor is what one release reads from another release's
// declaration: the predecessor digest and the registry features it names.
type RollbackPredecessor struct {
	Digest           Fingerprint
	RegistryFeatures []string
}

// ReadsRegistryFeature reports whether the declaring release reads that registry form.
func (predecessor RollbackPredecessor) ReadsRegistryFeature(feature string) bool {
	return slices.Contains(predecessor.RegistryFeatures, feature)
}

// ParseRollbackPredecessor reads only the predecessor registry digest and the
// registry features from another release's declaration. A newer release may
// add schema versions, fields, entries and features this release does not
// know, and the digest and each known feature keep their meaning across them,
// so only the bound, the JSON object, a schema version of at least 1 and a
// well-formed digest are checked. Feature entries that are not strings are
// ignored. A release's own declaration is parsed strictly by
// ParseRollbackDeclaration.
func ParseRollbackPredecessor(payload []byte) (RollbackPredecessor, error) {
	var declaration struct {
		SchemaVersion             *int              `json:"schemaVersion"`
		PredecessorRegistryDigest Fingerprint       `json:"predecessorRegistryDigest"`
		RegistryFeatures          []json.RawMessage `json:"registryFeatures"`
	}
	if err := decodeBounded(payload, MaxRollbackDeclarationBytes, &declaration, false); err != nil {
		return RollbackPredecessor{}, invalid("decode rollback declaration: %v", err)
	}
	if declaration.SchemaVersion == nil || *declaration.SchemaVersion < RollbackDeclarationSchemaV1 {
		return RollbackPredecessor{}, invalid("rollback declaration has no supported schema version")
	}
	if err := validateFingerprint(declaration.PredecessorRegistryDigest, "rollback predecessor registry digest"); err != nil {
		return RollbackPredecessor{}, err
	}
	predecessor := RollbackPredecessor{Digest: declaration.PredecessorRegistryDigest, RegistryFeatures: []string{}}
	for _, raw := range declaration.RegistryFeatures {
		var feature string
		if json.Unmarshal(raw, &feature) == nil {
			predecessor.RegistryFeatures = append(predecessor.RegistryFeatures, feature)
		}
	}
	return predecessor, nil
}
