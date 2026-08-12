package sshidentity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyLocalIdentity(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "operator")
	dataHome := filepath.Join(root, "data")
	sshDir := filepath.Join(home, ".ssh")
	keyDir := filepath.Join(dataHome, "ssh")
	for _, path := range []string{sshDir, keyDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	identity := filepath.Join(keyDir, "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", identity).CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, output)
	}
	if err := os.Chmod(identity, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(identity+".pub", 0o644); err != nil {
		t.Fatal(err)
	}

	writeSnippet := func(name, configuredIdentity string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sshDir, name), []byte(
			"Host yard\n    IdentityFile \""+configuredIdentity+"\"\n    IdentitiesOnly yes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeSnippet("subyard.config", identity)
	if got := Classify(home, dataHome, "default"); got != Dedicated {
		t.Fatalf("valid canonical identity classified as %q", got)
	}
	writeSnippet("subyard-shared.config", identity)
	if got := Classify(home, dataHome, "shared"); got != Dedicated {
		t.Fatalf("named yard sharing the canonical identity classified as %q", got)
	}

	writeSnippet("subyard-demo.config", filepath.Join(home, ".ssh", "id_ed25519"))
	if got := Classify(home, dataHome, "demo"); got != Legacy {
		t.Fatalf("personal identity classified as %q", got)
	}

	writeSnippet("subyard-unsafe.config", identity)
	if err := os.Chmod(filepath.Join(sshDir, "subyard-unsafe.config"), 0o666); err != nil {
		t.Fatal(err)
	}
	if got := Classify(home, dataHome, "unsafe"); got != Drift {
		t.Fatalf("unsafe snippet classified as %q", got)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "subyard-multiple.config"), []byte(
		"Host yard-multiple\n    IdentityFile \""+identity+"\"\n"+
			"    IdentityFile /tmp/other\n    IdentitiesOnly yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Classify(home, dataHome, "multiple"); got != Drift {
		t.Fatalf("multiple IdentityFile directives classified as %q", got)
	}

	if err := os.Chmod(identity, 0o640); err != nil {
		t.Fatal(err)
	}
	if got := Classify(home, dataHome, "default"); got != Drift {
		t.Fatalf("unsafe canonical identity classified as %q", got)
	}
	if got := Classify(home, dataHome, "missing"); got != Drift {
		t.Fatalf("missing snippet classified as %q", got)
	}
	if err := os.Chmod(identity, 0o600); err != nil {
		t.Fatal(err)
	}
	percentDataHome := filepath.Join(root, "data%slot")
	if err := os.Rename(dataHome, percentDataHome); err != nil {
		t.Fatal(err)
	}
	percentIdentity := filepath.Join(percentDataHome, "ssh", "id_ed25519")
	writeSnippet("subyard-percent.config", strings.ReplaceAll(percentIdentity, "%", "%%"))
	if got := Classify(home, percentDataHome, "percent"); got != Dedicated {
		t.Fatalf("percent-escaped canonical identity classified as %q", got)
	}
}

func TestCanonicalPairRejectsMismatchedPublicKey(t *testing.T) {
	dataHome := t.TempDir()
	keyDir := filepath.Join(dataHome, "ssh")
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(keyDir, "id_ed25519")
	second := filepath.Join(dataHome, "other")
	for _, path := range []string{first, second} {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("generate fixture: %v: %s", err, output)
		}
	}
	contents, err := os.ReadFile(second + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first+".pub", contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first+".pub", 0o644); err != nil {
		t.Fatal(err)
	}
	if ValidCanonicalPair(dataHome) {
		t.Fatal("mismatched canonical key pair was accepted")
	}
}
