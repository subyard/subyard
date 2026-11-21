package releasetransition

import "encoding/json"

const (
	RollbackDeclarationSchemaV1 = 1
	MaxRollbackDeclarationBytes = 64 << 10
)

// RollbackDeclaration is a release's sealed config/release-rollback.json. It
// names the one predecessor registry the release prepares rollback to, by
// canonical digest. Schema 1 defines no migration or profile-format entries yet,
// so both lists must be present and empty.
type RollbackDeclaration struct {
	SchemaVersion             int               `json:"schemaVersion"`
	PredecessorRegistryDigest Fingerprint       `json:"predecessorRegistryDigest"`
	Migrations                []json.RawMessage `json:"migrations"`
	ProfileFormats            []json.RawMessage `json:"profileFormats"`
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
