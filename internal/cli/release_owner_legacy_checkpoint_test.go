package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRustIngressDoesNotConstructOrInspectLegacyHistory(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	path := filepath.Join(home, "migrations")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(path, "transaction.json"), []byte("deliberately invalid old history"), 0600)
	facts, err := c.CaptureRustIngress(context.Background(), nil)
	if err != nil || len(facts.Operations) != 0 || len(facts.Blockers) != 0 {
		t.Fatal("native source capture inspected legacy history", err)
	}
	physical, ok := c.ingress.(*migration.V2CompatibilityIngress)
	if !ok || physical.Legacy != nil || !c.ingressRust {
		t.Fatal("Rust capture retained a Go legacy owner")
	}
}
