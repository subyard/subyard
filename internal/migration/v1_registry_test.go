package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLiveV1RegistryHasNoMigrationDefinitions(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Migrations) != 0 || registry.MinimumLayout != 1 || registry.CurrentLayout != 5 {
		t.Fatalf("retired compatibility header = %#v", registry)
	}
}
