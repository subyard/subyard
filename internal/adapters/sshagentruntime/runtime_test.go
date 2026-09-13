package sshagentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Runtime, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "sy-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, dir := range []string{"data", "data/ssh", "runtime", "bin"} {
		if err = os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"id_ed25519", "known_hosts"} {
		if err = os.WriteFile(filepath.Join(root, "data", "ssh", name), []byte("synthetic fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ssh", "ssh-agent", "ssh-add", "systemd-run", "systemctl", "loginctl"} {
		text := "#!/bin/sh\nexit 0\n"
		if name == "systemctl" {
			text = "#!/bin/sh\ncase \"$*\" in *LoadState*) printf 'LoadState=not-found\\nActiveState=inactive\\n';; *) echo 255;; esac\n"
		}
		if name == "loginctl" {
			text = "#!/bin/sh\necho yes\n"
		}
		if err = os.WriteFile(filepath.Join(root, "bin", name), []byte(text), 0700); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := New(Config{StateRoot: filepath.Join(root, "data", "ssh-agent"), Yard: "test", DataHome: filepath.Join(root, "data"), OperatorHome: root, Dispatcher: "/fixture/yard", DevUser: "dev", SSHPort: 2222,
		Environment: []string{"PATH=" + filepath.Join(root, "bin"), "XDG_RUNTIME_DIR=" + filepath.Join(root, "runtime")}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "selected-key")
	if err = os.WriteFile(key, []byte("synthetic selected key"), 0600); err != nil {
		t.Fatal(err)
	}
	return runtime, key
}

func TestPrepareValidatesWithoutReadingKeyOrCreatingState(t *testing.T) {
	r, key := fixture(t)
	prepared, err := r.Prepare(context.Background(), []string{"start", "--key", key, "--ttl", "14d"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Action != "ssh-agent.start" || !prepared.Changed || prepared.request.ttl != 14*24*time.Hour {
		t.Fatalf("bad assessment: %#v", prepared)
	}
	if _, err = os.Lstat(r.cfg.StateRoot); !os.IsNotExist(err) {
		t.Fatalf("prepare created state: %v", err)
	}
	// Invalid key contents above are deliberately accepted by metadata-only prepare.
	for _, mode := range []os.FileMode{0644, 0660} {
		if err = os.Chmod(key, mode); err != nil {
			t.Fatal(err)
		}
		if _, err = r.Prepare(context.Background(), []string{"start", "--key", key, "--ttl", "14d"}); err == nil {
			t.Fatal("accepted exposed key")
		}
	}
	if err = os.Chmod(key, 0600); err != nil {
		t.Fatal(err)
	}
	link := key + "-link"
	if err = os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Prepare(context.Background(), []string{"start", "--key", link, "--ttl", "14d"}); err == nil {
		t.Fatal("accepted symlink key")
	}
}

func TestKeyReplacementAfterConsentIsRejected(t *testing.T) {
	r, key := fixture(t)
	p, err := r.Prepare(context.Background(), []string{"start", "--key", key, "--ttl", "10m"})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the original inode alive, so this test cannot accidentally reuse it.
	if err = os.Rename(key, key+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(key, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = p.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "changed after assessment") {
		t.Fatalf("replacement: %v", err)
	}
	if _, err = os.Stat(filepath.Join(r.directory, "session.json")); !os.IsNotExist(err) {
		t.Fatal("replacement launched a session")
	}
}

func TestTTLAndVerbValidation(t *testing.T) {
	for _, args := range [][]string{
		{"start", "--key", "key"}, {"start", "--key", "key", "--ttl", "0s"},
		{"start", "--key", "key", "--ttl", "-1h"}, {"start", "--key", "key", "--ttl", "31d"},
		{"start", "--key", "key", "--ttl", "999999999999999999d"},
		{"start", "--key", "key", "--ttl", "1h", "--ttl", "2h"},
		{"status", "--ttl", "0s"}, {"stop", "--json"}, {"unexpected"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	if req, err := parse([]string{"start", "--key", "key", "--ttl", "30d"}); err != nil || req.ttl != maxTTL {
		t.Fatalf("30d: %#v %v", req, err)
	}
}

func TestStatusDoesNotCreateOrExposePrivateState(t *testing.T) {
	r, _ := fixture(t)
	p, err := r.Prepare(context.Background(), []string{"status", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(r.cfg.StateRoot); !os.IsNotExist(err) {
		t.Fatal("read-only status created state")
	}
	output := r.cfg.Stdout.(*bytes.Buffer)
	if strings.Contains(output.String(), r.cfg.StateRoot) {
		t.Fatal("status exposed private paths")
	}
	lock, err := r.lock()
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	deadline := time.Now().Add(-time.Second)
	if err = writeJSON(filepath.Join(r.directory, "session.json"), session{Yard: r.cfg.Yard, RuntimeDir: r.runtimeDir, ExpiresAt: deadline, Token: strings.Repeat("a", 32)}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err = p.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status Status
	if err = json.Unmarshal(output.Bytes(), &status); err != nil || status.State != "expired" {
		t.Fatalf("status %s: %v", output, err)
	}
	if strings.Contains(output.String(), "token") || strings.Contains(output.String(), "runtime_dir") {
		t.Fatal("status exposed internal state")
	}
}

func TestPrivateStateRejectsSymlinksAndConcurrentOperations(t *testing.T) {
	r, _ := fixture(t)
	if err := os.Symlink(r.cfg.OperatorHome, r.cfg.StateRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Prepare(context.Background(), []string{"status"}); err == nil {
		t.Fatal("accepted symlink state root")
	}
	if err := os.Remove(r.cfg.StateRoot); err != nil {
		t.Fatal(err)
	}
	lock, err := r.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := r.lock(); err == nil {
		other.Close()
		t.Fatal("allowed concurrent state operation")
	}
}

func TestSSHUsesDedicatedLoginAndForwardedAgentSeparately(t *testing.T) {
	s := session{RuntimeDir: "/run/user/1000/fixture", DataHome: "/fixture/data with space", DevUser: "dev", SSHPort: 2222, Token: strings.Repeat("a", 32)}
	args := sshArguments(s)
	joined := strings.Join(args, "\n")
	for _, want := range []string{"IdentitiesOnly=yes", `ForwardAgent="/run/user/1000/fixture/agent.sock"`, "StrictHostKeyChecking=yes", "/fixture/data with space/ssh/id_ed25519", "subyard-ssh-agent-ready"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, arg := range args {
		if arg == "-N" || arg == "-R" || arg == "IdentityAgent=none" {
			t.Fatalf("forwarding lost SSH agent session semantics: %s", arg)
		}
	}
}

func TestStopCannotClaimRevocationWhenManagerIsUnavailable(t *testing.T) {
	r, _ := fixture(t)
	lock, err := r.lock()
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	path := filepath.Join(r.directory, "session.json")
	s := session{Yard: r.cfg.Yard, RuntimeDir: r.runtimeDir, ExpiresAt: time.Now().Add(time.Hour)}
	if err = writeJSON(path, s); err != nil {
		t.Fatal(err)
	}
	program, err := r.program("systemctl")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(program, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = r.Revoke(context.Background()); err == nil {
		t.Fatal("claimed revoke without user manager")
	}
	var after session
	if err = readJSON(path, &after); err != nil {
		t.Fatal(err)
	}
	if after.Stopped {
		t.Fatal("persisted a false stopped state")
	}
}
