package tests

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestOrcaShippedSettings(t *testing.T) {
	catalog, err := config.LoadCatalog(filepath.Clean(filepath.Join("..", "..", "..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	foundRuntime := false
	for _, owner := range catalog.Profiles() {
		if owner.Name != "orca" {
			continue
		}
		// The ID is persisted in release journals and must survive extraction.
		if owner.Runtime == nil || owner.Runtime.ActivationID != "orca-runtime" || owner.Runtime.Handler != "runtime.sh" {
			t.Fatalf("Orca release activation contract changed: %#v", owner.Runtime)
		}
		if owner.GuestEnvironment == nil || owner.GuestEnvironment.Handler != "ssh-agent-environment.sh" {
			t.Fatalf("Orca guest SSH repair declaration is missing: %#v", owner.GuestEnvironment)
		}
		foundRuntime = true
	}
	if !foundRuntime {
		t.Fatal("Orca profile is missing")
	}
	for _, name := range []string{"ORCA_ADVERTISE_HOST", "ORCA_HOST_PORT"} {
		definition, ok := catalog.LookupSetting(name)
		if !ok || !definition.Syncable || definition.Application != config.SettingNextCommand {
			t.Fatalf("%s = %#v", name, definition)
		}
	}
}
