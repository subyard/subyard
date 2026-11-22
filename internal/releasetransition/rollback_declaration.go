package releasetransition

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/blang/semver/v4"
)

const (
	RollbackDeclarationSchemaV1 = 1
	MaxRollbackDeclarationBytes = 64 << 10
)

// RegistryFeatureRetiredPrefixDependencies is the closed frozen-v0.30 reader,
// not permission to satisfy dependencies from arbitrary retired prefixes.
// Its meaning is frozen while any release declaring it can be a rollback target.
const RegistryFeatureRetiredPrefixDependencies = "retired-prefix-dependencies"

// RegistryReaderFeatures is bound to the shipped declaration by a repository
// test. Packaging does not infer or negotiate compiled parser capabilities.
func RegistryReaderFeatures() []string {
	return []string{RegistryFeatureRetiredPrefixDependencies}
}

// RollbackDeclaration is a release's sealed config/release-rollback.json. It
// names the one predecessor registry the release prepares rollback to, by
// canonical digest. Schema 1 defines no migration or profile-format entries yet,
// so both lists must be present and empty. RegistryFeatures names the compiled
// reader forms sealed into this release; absent flags select the v0.30 rules.
// Boundaries declare installed state that requires manual recovery instead of rollback.
type RollbackDeclaration struct {
	SchemaVersion             int                `json:"schemaVersion"`
	PredecessorRegistryDigest Fingerprint        `json:"predecessorRegistryDigest"`
	Migrations                []json.RawMessage  `json:"migrations"`
	ProfileFormats            []json.RawMessage  `json:"profileFormats"`
	RegistryFeatures          []string           `json:"registryFeatures,omitempty"`
	Boundaries                []RollbackBoundary `json:"boundaries,omitempty"`
}

// RollbackBoundary belongs to the release introducing incompatible state. It
// blocks targets at or below ThroughVersion, including explicit version selection.
// Agent boundaries bind the reviewed source tree and list surviving yard files.
type RollbackBoundary struct {
	Component      string      `json:"component"`
	ThroughVersion string      `json:"throughVersion"`
	Reason         string      `json:"reason"`
	Recovery       string      `json:"recovery"`
	SourceDigest   Fingerprint `json:"sourceDigest,omitempty"`
	Files          []string    `json:"files,omitempty"`
}

func (boundary RollbackBoundary) validate() error {
	if boundary.Component != "registry" && boundary.Component != "agents" &&
		!(strings.HasPrefix(boundary.Component, "profile:") && validateSafeID(strings.TrimPrefix(boundary.Component, "profile:"), "profile runtime ID") == nil) {
		return invalid("unknown rollback boundary component %q", boundary.Component)
	}
	if _, err := semver.Parse(boundary.ThroughVersion); err != nil {
		return invalid("rollback boundary requires a semantic throughVersion")
	}
	for _, text := range []string{boundary.Reason, boundary.Recovery} {
		if validateText(text, "rollback boundary explanation", maxDiagnosticText, true) != nil {
			return invalid("rollback boundary requires a single-line reason and manual recovery instruction")
		}
	}
	if boundary.Component == "agents" {
		if err := validateFingerprint(boundary.SourceDigest, "agent boundary source digest"); err != nil {
			return err
		}
		if len(boundary.Files) == 0 {
			return invalid("agent rollback boundary must list surviving yard files")
		}
	} else if boundary.SourceDigest != "" || len(boundary.Files) != 0 {
		return invalid("only agent boundaries declare source digests and yard files")
	}
	for i, path := range boundary.Files {
		if !(strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~/")) || validateText(path, "agent boundary file", maxDiagnosticText, true) != nil ||
			strings.Contains(path, "/../") || strings.HasSuffix(path, "/..") || i > 0 && boundary.Files[i-1] >= path {
			return invalid("agent boundary files must be yard paths, sorted and unique")
		}
	}
	return nil
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
	for i, feature := range declaration.RegistryFeatures {
		if !slices.Contains(RegistryReaderFeatures(), feature) || i > 0 && declaration.RegistryFeatures[i-1] >= feature {
			return RollbackDeclaration{}, invalid("rollback declaration registry features must be known, sorted and unique")
		}
	}
	for i, boundary := range declaration.Boundaries {
		if err := boundary.validate(); err != nil {
			return RollbackDeclaration{}, err
		}
		if i > 0 && declaration.Boundaries[i-1].ThroughVersion+"/"+declaration.Boundaries[i-1].Component >= boundary.ThroughVersion+"/"+boundary.Component {
			return RollbackDeclaration{}, invalid("rollback boundaries must be sorted and unique by throughVersion/component")
		}
	}
	return declaration, nil
}

// ParseRollbackRegistryFeatures reads another release's sealed reader flags.
// Unknown features (including future entry shapes) are ignored. Malformed
// declarations grant no capability; predecessor validation stays shared.
func ParseRollbackRegistryFeatures(payload []byte) ([]string, error) {
	if _, err := ParseRollbackPredecessor(payload); err != nil {
		return nil, err
	}
	var declaration struct {
		RegistryFeatures []json.RawMessage `json:"registryFeatures"`
	}
	if err := decodeBounded(payload, MaxRollbackDeclarationBytes, &declaration, false); err != nil {
		return nil, invalid("decode registry features: %v", err)
	}
	var features []string
	for _, entry := range declaration.RegistryFeatures {
		var feature string
		if json.Unmarshal(entry, &feature) == nil && slices.Contains(RegistryReaderFeatures(), feature) && !slices.Contains(features, feature) {
			features = append(features, feature)
		}
	}
	return features, nil
}

// ParseRollbackPredecessor reads only the predecessor registry digest from
// another release's declaration. A newer release may add schema versions,
// fields and entries this release does not know, and the digest keeps its
// meaning across them, so only the bound, the JSON object, a schema version of
// at least 1 and a well-formed digest are checked. A release's own declaration
// is parsed strictly by ParseRollbackDeclaration.
func ParseRollbackPredecessor(payload []byte) (Fingerprint, error) {
	var declaration struct {
		SchemaVersion             *int        `json:"schemaVersion"`
		PredecessorRegistryDigest Fingerprint `json:"predecessorRegistryDigest"`
	}
	if err := decodeBounded(payload, MaxRollbackDeclarationBytes, &declaration, false); err != nil {
		return "", invalid("decode rollback declaration: %v", err)
	}
	if declaration.SchemaVersion == nil || *declaration.SchemaVersion < RollbackDeclarationSchemaV1 {
		return "", invalid("rollback declaration has no supported schema version")
	}
	if err := validateFingerprint(declaration.PredecessorRegistryDigest, "rollback predecessor registry digest"); err != nil {
		return "", err
	}
	return declaration.PredecessorRegistryDigest, nil
}
