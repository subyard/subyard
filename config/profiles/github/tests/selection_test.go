package github_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileDefaultsAndExplicitSelection(t *testing.T) {
	definitions, err := profile.Load("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	var github *profile.Definition
	for i := range definitions {
		if definitions[i].Name == "github" {
			github = &definitions[i]
			break
		}
	}
	if github == nil {
		t.Fatal("GitHub profile definition missing")
	}
	cases := []struct {
		name string
		yard string
		env  map[string]string
		want bool
	}{
		{name: "default yard", yard: "default", want: true},
		{name: "other yard", yard: "hermes", want: false},
		{name: "explicit enable", yard: "hermes", env: map[string]string{"ENVIRONMENT_PROFILES": "base github"}, want: true},
		{name: "explicit disable", yard: "default", env: map[string]string{"ENVIRONMENT_PROFILES": "base"}, want: false},
		{name: "nested vm disabled", yard: "default", env: map[string]string{"NESTED_E2E_VMS": "1", "ENVIRONMENT_PROFILES": "github"}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := github.Selected(test.yard, test.env); got != test.want {
				t.Fatalf("Selected() = %t, want %t", got, test.want)
			}
		})
	}
}

// The peer fixture also exercises the implicit default GitHub profile. Its
// consumer root must agree with installed release inspection, which reloads
// persisted settings instead of inheriting command-only catalog overrides.
func TestPeerFixturePersistsGitHubConsumerRoot(t *testing.T) {
	root := testkit.TempDir(t)
	peer := filepath.Join(root, "peer")
	if err := os.MkdirAll(peer, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(repository, "dev", "e2e", "p0-guest.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "peer_yard_start() {\n")
	if start < 0 {
		t.Fatal("peer yard setup missing")
	}
	end := strings.Index(string(source)[start:], "\npeer_info() {")
	if end < 0 {
		t.Fatal("peer yard setup boundary missing")
	}
	calls := filepath.Join(root, "calls")
	entry := filepath.Join(root, "yard")
	testkit.WriteFile(t, entry, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\n"), 0o700)
	script := string(source)[start:start+end] + `
cleanup_peer_snapshot_fixture() { :; }
incus() {
  case "$*" in
    'project show subyard') return 0 ;;
    'image info fixture --project default') return 1 ;;
    *) return 99 ;;
  esac
}
timeout() {
  [ "$1" = --foreground ] && [ "$2" = 1800 ] || return 99
  shift 2
  "$@"
}
umask 0022
peer_yard_start
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	consumer := filepath.Join(root, "consumer files")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "SUBYARD_E2E_VM=2", "PEER_ROOT=" + peer, "PEER_REAL_YARD_MARKER=" + filepath.Join(peer, "marker"), "MARKER=peer-fixture", "OWNER_BASE_IMAGE=fixture", "PEER_YARD_ENTRY=" + entry, "PEER_KEYS_CONSUMER_ROOT=" + consumer, "CALLS=" + calls}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("peer fixture: %v: %s", err, output)
	}
	content, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "init --yes\ninit --yes\ncheck\nstart --yes\n" {
		t.Fatalf("peer workflow changed: %q", content)
	}
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: repository, OperatorHome: filepath.Join(root, "home"), DisablePrivate: true, Environment: map[string]string{
		"HOME": filepath.Join(root, "home"), "SUBYARD_OPERATOR_HOME": filepath.Join(root, "home"), "SUBYARD_CONFIG_HOME": filepath.Join(peer, "config"), "SUBYARD_HOME": filepath.Join(root, "data"), "STORAGE_PATH": filepath.Join(root, "storage"), "HOST_BASE": filepath.Join(root, "host"), "RESTRICTED_DISK_PATHS": filepath.Join(root, "host"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["SUBYARD_KEYS_CONSUMER_ROOT"] != consumer {
		t.Fatal("peer consumer root is only a command override; installed readiness resolves a different GitHub key file")
	}
	if len(loaded.Integrations.Effective) != 0 {
		t.Fatal("peer coding integrations changed")
	}
	definitions, err := profile.Load(repository)
	if err != nil {
		t.Fatal(err)
	}
	selected := false
	for _, definition := range definitions {
		if definition.Name == "github" {
			selected = definition.Selected("default", loaded.Environment)
		}
	}
	if !selected {
		t.Fatal("peer fixture disabled implicit GitHub selection")
	}
	info, err := os.Stat(filepath.Join(peer, "config", "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("peer config mode=%o, want 0600", info.Mode().Perm())
	}
}
