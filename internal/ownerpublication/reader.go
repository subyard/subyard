package ownerpublication

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// RequireReader admits writes only after the protected typed release boundary
// has committed. Existing epoch-1 power values remain readable without it.
func RequireReader(root, configHome string) error {
	payload, err := os.ReadFile(filepath.Join(root, "config", "release-transition.json"))
	if err != nil {
		return err
	}
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		return err
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return err
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		return err
	}
	if state.Ledger.Domains["power-metadata"].Epoch != 2 {
		return fmt.Errorf("optional publication requires committed power-metadata epoch 2; complete the supported release transition")
	}
	return nil
}
