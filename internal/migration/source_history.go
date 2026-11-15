package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// InspectSourceHistory preserves the absent-history binding used by source repair.
// Supported installations are assumed to have no V1 files; do not inspect them.
func InspectSourceHistory(_ context.Context, options ReleaseOptions) (releasetransition.Fingerprint, error) {
	type input struct {
		Present              bool
		Digest               releasetransition.Fingerprint
		Device, Inode, Links uint64
		Mode, UID, GID       uint32
	}
	root := filepath.Join(options.ConfigHome, "migrations")
	payload, err := json.Marshal(map[string]input{
		filepath.Join(root, "transactions"): {},
		filepath.Join(root, "state.json"):   {},
	})
	digest := sha256.Sum256(payload)
	return releasetransition.Fingerprint(hex.EncodeToString(digest[:])), err
}
