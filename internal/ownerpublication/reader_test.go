package ownerpublication

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	checkedEpoch2 := false
	for _, migration := range registry.Migrations {
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
		if ledger.Domains["power-metadata"].Epoch == 2 {
			write()
			if err := RequireReader(root, home); err != nil {
				t.Fatalf("committed epoch-2 reader not admitted: %v", err)
			}
			checkedEpoch2 = true
		}
	}
	if !checkedEpoch2 || ledger.Domains["power-metadata"].Epoch != 3 {
		t.Fatal("fixture must cover known reader epochs 2 and 3")
	}
	write()
	if err := RequireReader(root, home); err != nil {
		t.Fatalf("committed epoch-3 reader not admitted: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := RequireReader(root, home); err == nil {
		t.Fatal("unsafe boundary permissions were accepted")
	}

	// A valid future typed ledger reaches this consumer, rather than failing
	// an unrelated codec check. Its unknown semantics must not inherit writes.
	futureRoot := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(futureRoot, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	future := releasetransition.RegistryV2{SchemaVersion: 2, MinimumEpochs: map[string]int{"power-metadata": 4}, CurrentEpochs: map[string]int{"power-metadata": 4}, Migrations: []releasetransition.MigrationDefinitionV2{}}
	futurePayload, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	future, digest, err := releasetransition.ParseRegistryV2(futurePayload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(futureRoot, "config/release-transition.json"), futurePayload, 0600)
	futurePayload, _, err = releasetransition.MarshalLedgerV2(releasetransition.BaselineLedgerV2(future), future)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, futurePayload, 0600)
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := store.ReadMigrationLedger(future, digest); err != nil || state.Ledger.Domains["power-metadata"].Epoch != 4 {
		t.Fatalf("future fixture did not reach consumer boundary: %v", err)
	}
	if err := RequireReader(futureRoot, home); err == nil || !strings.Contains(err.Error(), "supported power-metadata") {
		t.Fatalf("unknown future reader was admitted or failed outside consumer: %v", err)
	}
}
