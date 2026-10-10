package configsync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestConfigSyncKeepsResetYardAbsentWithoutDeletingSource(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("shared/config.env", "YARD_IMAGE=images:debian/13\n")
	fixture.writeSource("hosts/owner-a/yards/named/config.env", "SSH_PORT=2234\n")
	fixture.commit("initial")
	noOp := func() error { return nil }
	if err := config.ResetYardConfiguration(fixture.configHome, "named", noOp, noOp, noOp); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := config.FindYardRegistrationFile(filepath.Join(fixture.root, "config"), fixture.configHome, "named"); !errors.Is(err, config.ErrUnknownYard) {
		t.Fatalf("sync reactivated reset yard: %v", err)
	}
	for _, path := range []string{filepath.Join(fixture.source, "hosts/owner-a/yards/named/config.env"), filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "yards/named/config.env")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Git definition removed: %v", err)
		}
	}
	fixture.writeSource("hosts/owner-a/yards/named/config.env", "SSH_PORT=invalid\n")
	fixture.commit("invalid source")
	if _, err := BuildPlan(fixture.options(false)); err == nil {
		t.Fatal("suppressed source bypassed source validation")
	}
}

func TestConfigSyncResetMarkerIsAStaleInput(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/yards/named/config.env", "SSH_PORT=2234\n")
	fixture.commit("initial")
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	noOp := func() error { return nil }
	if err := config.ResetYardConfiguration(fixture.configHome, "named", noOp, noOp, noOp); err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err == nil {
		t.Fatal("changed reset ownership marker accepted by sync")
	}
}
