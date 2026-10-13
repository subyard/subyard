package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLegacySSHSourceCleanupRetainsDefinitionsModesAndUnrelatedOwners(t *testing.T) {
	root := testkit.TempDir(t)
	files := map[string]string{
		"hosts/owner-a/config.env":                           "# preserve\nSSH_PORT='2300'\nDEV_UID=1000\n",
		"hosts/owner-a/yards/named/config.env":               "SSH_PORT=2301\n",
		"hosts/owner-a/overrides/agents/sample/settings.env": "SSH_PORT=asset-content\n",
		"hosts/owner-b/config.env":                           "SSH_PORT=2400\n",
		"shared/config.env":                                  "DEV_SUDO=0\n",
	}
	for path, content := range files {
		writeConfigCommandFile(t, filepath.Join(root, path), content, 0o640)
	}
	changed, err := sanitizeLegacySSHSource(root, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, []string{"hosts/owner-a/config.env", "hosts/owner-a/yards/named/config.env"}) {
		t.Fatalf("cleanup paths: %#v", changed)
	}
	for path, before := range files {
		want := before
		if path == "hosts/owner-a/config.env" {
			want = "# preserve\nDEV_UID=1000\n"
		}
		if path == "hosts/owner-a/yards/named/config.env" {
			want = ""
		}
		full := filepath.Join(root, path)
		if string(mustReadConfigFixture(t, full)) != want {
			t.Fatalf("cleanup changed unrelated records/definition: %s", path)
		}
		info, err := os.Stat(full)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("cleanup mode: %s %v", path, err)
		}
	}
}

func TestLegacySSHSourceCleanupUsesPreparedConsentAndRejectsNewGitAuthoring(t *testing.T) {
	f := newConfigGitAuthoringFixture(t)
	checkout := filepath.Join(f.home, ".local/share/subyard-config")
	source := filepath.Join(checkout, "hosts/owner-a/config.env")
	before := "# preserve source\nSSH_PORT=2300\nDEV_UID=2297\n"
	writeConfigCommandFile(t, source, before, 0o600)
	commitConfigSource(t, checkout, "Legacy owner transport setting")
	writeConfigCommandFile(t, filepath.Join(f.configHome, "config.env"), "# local transport\nSSH_PORT=2299\n", 0o640)
	local := mustReadConfigFixture(t, filepath.Join(f.configHome, "config.env"))
	checkoutBefore := snapshotConfigCheckoutRaw(t, checkout)
	remoteBefore := strings.TrimSpace(configSourceGitOutput(t, f.remote, "rev-parse", "HEAD"))
	prompt := &testkit.Prompt{Answers: []bool{false}}
	program, err := New(Options{RepositoryRoot: f.root, Environment: f.environment, Arguments: []string{"config", "sync", "push"}, Prompt: prompt, Stdout: &f.stdout, Stderr: &f.stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || len(prompt.Requests) != 1 {
		t.Fatalf("source cleanup consent: %d %s", code, f.stderr.String())
	}
	if !strings.Contains(strings.Join(prompt.Requests[0].Consequences, "\n"), "remove SSH_PORT from hosts/owner-a/config.env") {
		t.Fatal("source cleanup consent omitted its exact source effect")
	}
	if !reflect.DeepEqual(checkoutBefore, snapshotConfigCheckoutRaw(t, checkout)) || remoteBefore != strings.TrimSpace(configSourceGitOutput(t, f.remote, "rev-parse", "HEAD")) {
		t.Fatal("declined cleanup changed registered Git source")
	}
	f.run(0, "config", "sync", "push", "--yes")
	if after := configSourceGitOutput(t, f.remote, "show", "HEAD:hosts/owner-a/config.env"); after != "# preserve source\nDEV_UID=2297\n" {
		t.Fatalf("source cleanup did not publish only sanitation: %q", after)
	}
	if !bytes.Equal(local, mustReadConfigFixture(t, filepath.Join(f.configHome, "config.env"))) {
		t.Fatal("explicit source cleanup changed winning local settings")
	}
	f.assertSetting("SSH_PORT", "2299")
	f.run(2, "config", "set", "SSH_PORT", "2301", "--scope", "host", "--git", "--yes")
	if !strings.Contains(f.stderr.String(), "local-only") {
		t.Fatalf("new Git SSH authoring lacks policy diagnostic: %s", f.stderr.String())
	}
}
