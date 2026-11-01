package ownerpublication

import (
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
	// Reader admission is the committed physical schema, not executable policy
	// for unrelated Rust settings kinds in the same authenticated source.
	rustRoot := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(rustRoot, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	rustPayload := []byte(strings.Replace(string(payload), "local-ssh-port-settings-v2-to-v3", "new-rust-settings-kind", 1))
	if _, _, err := releasetransition.ParseRegistryV2(rustPayload, releasetransition.BuiltinCapabilityCatalog()); err == nil {
		t.Fatal("Go executable policy admitted Rust-only kind")
	}
	testkit.WriteFile(t, filepath.Join(rustRoot, "config/release-transition.json"), rustPayload, 0644)
	if err := RequireReader(rustRoot, home); err != nil {
		t.Fatal("unrelated Rust kind hid committed power reader", err)
	}
	invalid := []byte(strings.Replace(string(rustPayload), `"toEpoch": 3`, `"toEpoch": 4`, 1))
	testkit.WriteFile(t, filepath.Join(rustRoot, "config/release-transition.json"), invalid, 0644)
	if err := RequireReader(rustRoot, home); err == nil {
		t.Fatal("invalid history admitted publication")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := RequireReader(root, home); err == nil {
		t.Fatal("unsafe boundary permissions were accepted")
	}
}
