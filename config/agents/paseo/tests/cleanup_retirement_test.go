package paseo_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPaseoCleanupPreservesMultipleRetirementGenerationsAndRejectsStalePlan(t *testing.T) {
	root := testkit.TempDir(t)
	system := filepath.Join(root, "etc/systemd/system")
	unit := filepath.Join(system, "paseo.service")
	for _, path := range []string{system, filepath.Join(root, "var/lib/subyard"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile("../cleanup.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(source), "SYSTEM_DIR = '/etc/systemd/system'", fmt.Sprintf("SYSTEM_DIR = %q", system))
	script = strings.ReplaceAll(script, "RECEIPT = '/var/lib/subyard/paseo-ownership'", fmt.Sprintf("RECEIPT = %q", filepath.Join(root, "var/lib/subyard/paseo-ownership")))
	script = strings.ReplaceAll(script, "SAFE_ROOT = '/'", fmt.Sprintf("SAFE_ROOT = %q", root))
	script = strings.ReplaceAll(script, "OWNER_UID = 0", fmt.Sprintf("OWNER_UID = %d", os.Getuid()))
	path := filepath.Join(root, "cleanup.sh")
	testkit.WriteFile(t, path, []byte(script), 0o700)
	stub := "#!/bin/sh\nif [ \"$1\" = show ]; then printf '%s\\n' ActiveState=inactive UnitFileState=disabled FragmentPath= NeedDaemonReload=no LoadState=not-found; fi\n"
	testkit.WriteFile(t, filepath.Join(root, "bin/systemctl"), []byte(stub), 0o700)
	backup := unit + ".subyard-retired"
	testkit.WriteFile(t, backup, []byte("old generation\n"), 0o600)
	run := func(verb, fingerprint string, success bool) map[string]any {
		t.Helper()
		command := exec.Command(path, verb, "dev", "1000", fingerprint)
		command.Env = []string{"PATH=" + filepath.Join(root, "bin") + ":" + os.Getenv("PATH")}
		output, err := command.CombinedOutput()
		if success != (err == nil) {
			t.Fatalf("%s: %v %s", verb, err, output)
		}
		var result map[string]any
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("native result: %s %v", output, err)
		}
		return result
	}
	for generation := 1; generation <= 2; generation++ {
		content := fmt.Sprintf("generation %d\n", generation)
		testkit.WriteFile(t, unit, []byte(content), 0o600)
		plan := run("observe", "", true)
		result := run("apply", plan["fingerprint"].(string), true)
		if result["changed"] != false {
			t.Fatal("native cleanup did not converge")
		}
		retired, err := os.ReadFile(fmt.Sprintf("%s.%d", backup, generation))
		if err != nil || string(retired) != content {
			t.Fatal("later retirement was lost")
		}
		original, _ := os.ReadFile(backup)
		if string(original) != "old generation\n" {
			t.Fatal("original backup was overwritten")
		}
	}
	testkit.WriteFile(t, unit, []byte("third generation\n"), 0o600)
	plan := run("observe", "", true)
	testkit.WriteFile(t, backup+".3", []byte("concurrent backup\n"), 0o600)
	if result := run("apply", plan["fingerprint"].(string), false); result["code"] != "cleanup_stale" {
		t.Fatalf("stale plan admitted: %#v", result)
	}
	current, _ := os.ReadFile(unit)
	if string(current) != "third generation\n" {
		t.Fatal("stale cleanup changed live unit")
	}
	if err := os.Symlink(unit, backup+".4"); err != nil {
		t.Fatal(err)
	}
	if result := run("observe", "", false); result["code"] != "cleanup_unsafe_file" {
		t.Fatalf("symlink destination admitted: %#v", result)
	}
}
