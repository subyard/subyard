package ownerpublication

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPublicationWritesRequireCommittedProtectedReaderEpoch(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	home := testkit.TempDir(t)
	if err := RequireReader(root, home); err == nil {
		t.Fatal("absent reader boundary admitted publication writes")
	}
	payload, err := os.ReadFile(filepath.Join(root, "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(registry)
	path := filepath.Join(home, "release-transition", "v2", "ledger.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(home, "release-transition"), filepath.Dir(path)} {
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func() {
		payload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, payload, 0600)
	}
	write()
	if err := RequireReader(root, home); err == nil {
		t.Fatal("epoch-1 reader admitted publication writes")
	}
	for _, migration := range registry.Migrations {
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	write()
	if err := RequireReader(root, home); err != nil {
		t.Fatalf("committed reader not admitted: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := RequireReader(root, home); err == nil {
		t.Fatal("unsafe boundary permissions were accepted")
	}
}
