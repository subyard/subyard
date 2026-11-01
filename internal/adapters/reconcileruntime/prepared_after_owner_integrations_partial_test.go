package reconcileruntime

import (
	"bytes"
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

func TestAfterOwnerIntegrationFullSetPublicationAcrossFreshProcess(t *testing.T) {
	root := os.Getenv("SUBYARD_INTEGRATION_PARTIAL_CHILD")
	child := root != ""
	if !child {
		root = testkit.TempDir(t)
	}
	home, state := filepath.Join(root, "home"), filepath.Join(root, "inventory")
	program := strings.NewReplacer("@STATE_ROOT@", state, "@STATE_UID@", fmt.Sprint(os.Getuid())).Replace(integrationInventoryProgram)
	entries := []integrationArtifact{}
	for _, name := range []string{"a", "b"} {
		payload := []byte("approved " + name)
		entries = append(entries, integrationArtifact{ID: name, Kind: "file", Path: filepath.Join(home, name), Digest: fmt.Sprintf("%x", sha256.Sum256(payload)), Content: base64.StdEncoding.EncodeToString(payload)})
	}
	var original integrationObservation
	run := func(mode string, cut bool) integrationObservation {
		t.Helper()
		source := program
		if cut {
			hook := fmt.Sprintf("\n_original_atomic = atomic\ndef _cut(path, *args, **kwargs):\n    if path == %q:\n        raise ValueError('fixture cut before B publication')\n    _original_atomic(path, *args, **kwargs)\natomic = _cut\n", entries[1].Path)
			source = strings.Replace(source, "\ntry:\n    locked_main()", hook+"\ntry:\n    locked_main()", 1)
		}
		guard := any(map[string]bool{"capture": true})
		if original.Inventory != nil {
			guard = original.Inventory
		}
		input, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": entries, "after_owner": guard})
		cmd := exec.Command("python3", "-B", "-c", source, mode)
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.CombinedOutput()
		if cut {
			if err == nil {
				t.Fatal("real B cut missing")
			}
			return integrationObservation{}
		}
		if err != nil {
			t.Fatalf("%s: %v %s", mode, err, output)
		}
		var observation integrationObservation
		if mode == "observe" && json.Unmarshal(output, &observation) != nil {
			t.Fatal("invalid native observation")
		}
		return observation
	}
	plan := func(o integrationObservation) IntegrationPlan {
		return IntegrationPlan{Fingerprint: o.Fingerprint, Changed: o.Changed, Scope: "original full approved set", facts: o.Facts}
	}
	if child {
		raw, err := os.ReadFile(filepath.Join(root, "original.physical"))
		if err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal(raw, &original) != nil {
			t.Fatal("original record")
		}
		before := afterOwnerFileStat(t, entries[0].Path)
		fresh := plan(run("observe", false))
		compared, err := projectAfterOwnerIntegrationInventory(plan(original), fresh, &afterOwnerIntegrationPhysical{Inventory: original.Inventory})
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckIntegrationPlan(plan(original), compared); err != nil {
			t.Fatal("original full-set retry", err)
		}
		run("apply", false)
		run("commit", false)
		if before != afterOwnerFileStat(t, entries[0].Path) {
			t.Fatal("retry rewrote desired A")
		}
		if run("observe", false).Changed {
			t.Fatal("full-set commit did not converge")
		}
		return
	}
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	original = run("observe", false)
	raw, _ := json.Marshal(original)
	testkit.WriteFile(t, filepath.Join(root, "original.physical"), raw, 0600)
	run("apply", true)
	inventoryRaw, err := os.ReadFile(filepath.Join(state, "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Entries []map[string]any `json:"entries"`
	}
	if json.Unmarshal(inventoryRaw, &inventory) != nil || len(inventory.Entries) != 2 {
		t.Fatal("full pending rowset")
	}
	for i, e := range inventory.Entries {
		if e["pending_digest"] != entries[i].Digest {
			t.Fatal("pending set omitted original target")
		}
	}
	if raw, err := os.ReadFile(entries[0].Path); err != nil || string(raw) != "approved a" {
		t.Fatal("real A publication missing", err)
	}
	if _, err := os.Stat(entries[1].Path); !os.IsNotExist(err) {
		t.Fatal("B published before destination cut", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAfterOwnerIntegrationFullSetPublicationAcrossFreshProcess$", "-test.v")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SUBYARD_INTEGRATION_PARTIAL_CHILD=" + root}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh helper: %v\n%s", err, output)
	}
}

func TestAfterOwnerIntegrationInventoryRejectsForeignSuccessors(t *testing.T) {
	for _, change := range []string{"third-state", "mode", "uid", "path", "link", "receipt-mode", "foreign-pending", "foreign-config-pending", "old-missing-witness"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			home := filepath.Join(root, "home")
			state := filepath.Join(root, "inventory")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			payload := []byte("approved file")
			entry := integrationArtifact{ID: "sample", Kind: "file", Path: filepath.Join(home, "a"), Digest: fmt.Sprintf("%x", sha256.Sum256(payload)), Content: base64.StdEncoding.EncodeToString(payload)}
			program := strings.NewReplacer("@STATE_ROOT@", state, "@STATE_UID@", fmt.Sprint(os.Getuid())).Replace(integrationInventoryProgram)
			invoke := func(mode string, guard any) (integrationObservation, error) {
				t.Helper()
				input, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": []integrationArtifact{entry}, "after_owner": guard})
				command := exec.Command("python3", "-B", "-c", program, mode)
				command.Stdin = bytes.NewReader(input)
				output, err := command.CombinedOutput()
				if err != nil {
					return integrationObservation{}, fmt.Errorf("%s: %w %s", mode, err, output)
				}
				var value integrationObservation
				if mode == "observe" && json.Unmarshal(output, &value) != nil {
					t.Fatal("invalid native observation")
				}
				return value, nil
			}
			// Use ordinary publication to establish the original protected inventory.
			if _, err := invoke("apply", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := invoke("commit", nil); err != nil {
				t.Fatal(err)
			}
			original, err := invoke("observe", map[string]bool{"capture": true})
			if err != nil {
				t.Fatal(err)
			}
			before := afterOwnerFileStat(t, entry.Path)
			receipt := filepath.Join(state, "inventory.json")
			switch change {
			case "third-state":
				testkit.WriteFile(t, entry.Path, []byte("foreign owned bytes"), 0644)
			case "link":
				if err := os.Remove(entry.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"/foreign", entry.Path); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(entry.Path, 0600); err != nil {
					t.Fatal(err)
				}
			case "uid":
				if os.Getuid() != 0 {
					t.Skip("fixture UID mutation requires root")
				}
				if err := os.Chown(entry.Path, 1000, 1000); err != nil {
					t.Fatal(err)
				}
			case "path":
				if err := os.Rename(home, home+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(home+"-original", home); err != nil {
					t.Fatal(err)
				}
			case "receipt-mode":
				if err := os.Chmod(receipt, 0644); err != nil {
					t.Fatal(err)
				}
			case "foreign-pending", "foreign-config-pending":
				raw, err := os.ReadFile(receipt)
				if err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if json.Unmarshal(raw, &value) != nil {
					t.Fatal("original receipt")
				}
				row := value["entries"].([]any)[0].(map[string]any)
				key := "pending_digest"
				if change == "foreign-config-pending" {
					key = "config_pending_digest"
				}
				row[key] = strings.Repeat("f", 64)
				raw, _ = json.Marshal(value)
				testkit.WriteFile(t, receipt, raw, 0600)
			}
			guard := any(original.Inventory)
			if change == "old-missing-witness" {
				guard = map[string]any{}
			}
			if _, err := invoke("observe", guard); err == nil {
				t.Fatal("foreign successor became original authority")
			}
			if _, err := invoke("apply", guard); err == nil {
				t.Fatal("foreign successor reached publisher")
			}
			if change == "foreign-config-pending" {
				if _, err := invoke("observe", map[string]bool{"capture": true}); err == nil {
					t.Fatal("capture adopted a foreign config pending intent")
				}
			}
			if change != "third-state" && change != "mode" && change != "uid" && change != "path" && change != "link" && before != afterOwnerFileStat(t, entry.Path) {
				t.Fatal("refused receipt changed destination")
			}
		})
	}
}
