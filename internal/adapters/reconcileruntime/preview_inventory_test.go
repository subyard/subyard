package reconcileruntime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPreviewInstructionAdoptionAndUnownedRefresh(t *testing.T) {
	root := testkit.TempDir(t)
	home, state := filepath.Join(root, "home"), filepath.Join(root, "inventory")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "AGENTS.md")
	raw := []byte("Keep the host instructions.\n")
	payload := append(append([]byte(nil), raw...), []byte("\n\n"+previewInstructions)...)
	hash := func(value []byte) string { return fmt.Sprintf("%x", sha256.Sum256(value)) }
	entry := integrationArtifact{ID: "synthetic", Kind: "file", Path: path,
		Digest: hash(payload), SourceDigest: hash(raw), Content: base64.StdEncoding.EncodeToString(payload)}
	program := strings.NewReplacer("@STATE_ROOT@", state, "@STATE_UID@", fmt.Sprint(os.Getuid())).Replace(integrationInventoryProgram)
	run := func(mode string, adopt, wantError bool) integrationObservation {
		t.Helper()
		input, err := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": []integrationArtifact{entry}, "adopt": adopt})
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(input))
		output, err := command.CombinedOutput()
		if (err != nil) != wantError {
			t.Fatalf("%s adopt=%v: err=%v output=%s", mode, adopt, err, output)
		}
		var result integrationObservation
		if mode == "observe" && !wantError {
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	testkit.WriteFile(t, path, raw, 0o644)
	if err := os.Chown(path, -1, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	run("configs-prepare", false, true)
	run("observe", false, true)
	observed := run("observe", true, false)
	if len(observed.Adopted) != 1 || !observed.Changed {
		t.Fatalf("exact raw instructions were not offered for adoption: %#v", observed)
	}
	testkit.WriteFile(t, path, []byte("unmanaged drift"), 0o644)
	run("observe", true, true)
	testkit.WriteFile(t, path, raw, 0o644)
	run("apply", true, false)
	run("commit", true, false)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("adoption did not preserve raw text and add preview: %q err=%v", got, err)
	}
	inventory, err := os.ReadFile(filepath.Join(state, "inventory.json"))
	if err != nil || strings.Contains(string(inventory), "source_digest") {
		t.Fatalf("input-only adoption evidence entered persistent schema: %s err=%v", inventory, err)
	}
	if run("observe", false, false).Changed {
		t.Fatal("adopted instructions did not converge")
	}
	run("configs-prepare", false, false)
	run("configs-commit", false, false)
}
