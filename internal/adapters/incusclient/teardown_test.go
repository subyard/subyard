package incusclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTeardownRejectsSharedDefaultProject(t *testing.T) {
	for _, project := range []string{"", "default"} {
		if _, err := New("unused").TeardownInventory(context.Background(), project); err == nil {
			t.Fatal("shared default project accepted")
		}
	}
}

func TestPhysicalTeardownGuardMatchesCapturedMetadataAndSnapshots(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	root := testkit.TempDir(t)
	observed := map[string]any{"name": "yard", "type": "container", "created_at": "2026-10-03T12:00:00.123456789Z", "config": map[string]string{"user.description": "<safe>"}, "devices": map[string]any{}, "profiles": []string{"default"}, "snapshots": []string{"approved"}}
	payload, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	resources := []ports.TeardownResource{{Kind: "instance", Name: "yard", Binding: hex.EncodeToString(digest[:])}}
	inventory, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "instance.json")
	testkit.WriteFile(t, data, raw, 0o600)
	snapshots := filepath.Join(root, "snapshots.json")
	testkit.WriteFile(t, snapshots, []byte(`["/1.0/instances/yard/snapshots/approved?project=fixture"]`), 0o600)
	fake := filepath.Join(root, "incus")
	testkit.WriteFile(t, fake, []byte("#!/bin/sh\ncase \"$2\" in */snapshots\\?*) cat \"$SNAPSHOT_DATA\";; *) cat \"$INSTANCE_DATA\";; esac\n"), 0o700)
	guard := filepath.Join("..", "..", "..", "scripts", "lib", "teardown-plan.py")
	run := func(args ...string) error {
		command := exec.Command("python3", append([]string{guard}, args...)...)
		command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "INSTANCE_DATA="+data, "SNAPSHOT_DATA="+snapshots, "SUBYARD_TEARDOWN_INVENTORY="+string(inventory))
		_, err := command.CombinedOutput()
		return err
	}
	if err := run("guard", "instance", "yard", "fixture"); err != nil {
		t.Fatalf("captured physical metadata was rejected: %v", err)
	}
	if err := run("guard", "instance", "unapproved", "fixture"); err == nil {
		t.Fatal("unapproved physical deletion target accepted")
	}
	testkit.WriteFile(t, snapshots, []byte(`["/1.0/instances/yard/snapshots/approved", "/1.0/instances/yard/snapshots/new"]`), 0o600)
	if err := run("guard", "instance", "yard", "fixture"); err == nil {
		t.Fatal("newly created snapshot expanded physical deletion")
	}
}

func TestTeardownResourceContainment(t *testing.T) {
	approved := []ports.TeardownResource{{Kind: "volume", Pool: "pool", Name: "owned", Binding: strings.Repeat("a", 64)}}
	if err := ports.CheckTeardownResources(approved, nil); err != nil {
		t.Fatal(err)
	}
	current := append(append([]ports.TeardownResource{}, approved...), ports.TeardownResource{Kind: "volume", Pool: "pool", Name: "new", Binding: strings.Repeat("b", 64)})
	if err := ports.CheckTeardownResources(approved, current); err == nil {
		t.Fatal("new volume approved for physical deletion")
	}
}
