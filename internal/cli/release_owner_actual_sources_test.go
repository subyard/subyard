package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativeFreshActivationIgnoresVirtualIngressFiles(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	path := filepath.Join(home, "config.env")
	testkit.WriteFile(t, path, []byte("SSH_PORT=2240\n"), 0o600)
	if _, _, err := c.CaptureInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.prospective = preparedActivationSnapshotFixture{home: home, names: []string{"default"}, snapshots: map[string]config.PersistentFileSnapshot{path: {Exists: true, Content: []byte("SSH_PORT=2250\n")}}}
	projected, err := c.CaptureActivation(context.Background(), "host-power", false)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := c.CaptureActivation(context.Background(), "host-power", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || len(actual) != 1 || actual[0].Source == projected[0].Source {
		t.Fatal("fresh actual source was hidden by virtual ingress")
	}
	testkit.WriteFile(t, path, []byte("SSH_PORT=2250\n"), 0o600)
	actual, err = c.CaptureActivation(context.Background(), "host-power", true)
	if err != nil || actual[0].Source != projected[0].Source {
		t.Fatal("own approved publication changed semantic source", err)
	}
}
