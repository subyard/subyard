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

func TestBrokerFixtureSeparatesDefaultAndNamedHostNamespaces(t *testing.T) {
	root := testkit.TempDir(t)
	repository, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(repository, "config/profiles/github/tests/e2e/acceptance.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "write_config() {\n")
	if start < 0 {
		t.Fatal("broker configuration helper missing")
	}
	end := strings.Index(string(source)[start:], "\n}\n")
	if end < 0 {
		t.Fatal("broker configuration helper boundary missing")
	}
	home, configHome := filepath.Join(root, "home"), filepath.Join(root, "config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"HOME": home, "SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome,
		"SUBYARD_HOME": filepath.Join(root, "data"), "STORAGE_PATH": filepath.Join(root, "storage"),
		"SUBYARD_KEYS_CONSUMER_ROOT": filepath.Join(root, "keys"),
	}
	command := exec.Command("bash", "-ceu", string(source)[start:start+end+3]+"\nwrite_config\n")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "STATE=" + root, "MARKER=fixture", "SSH_PORT=39999", "PROJECT=github-fixture", "INSTANCE=github-e2e"}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("write broker configuration: %v: %s", err, output)
	}
	defaultPath := filepath.Join(configHome, "yards/default/config.env")
	hostBefore, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	defaultBefore, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	preset := filepath.Join(repository, "config/profiles/hermes/yard.env")
	presetBefore, err := os.ReadFile(preset)
	if err != nil {
		t.Fatal(err)
	}
	load := func(name, settings string) config.Loaded {
		t.Helper()
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: repository, OperatorHome: home, DisablePrivate: true,
			Environment: environment, YardName: name, YardSettingsFile: settings})
		if err != nil {
			t.Fatal(err)
		}
		return loaded
	}
	current, named := load("default", ""), load("hermes-fixture", preset)
	if current.Context.Paths.HostBase != filepath.Join(root, "host") || current.Context.Paths.HostBase == named.Context.Paths.HostBase {
		t.Fatal("broker fixture shares the default yard host namespace with the new Hermes yard")
	}
	if current.Context.IncusProject != "github-fixture" || current.Context.YardInstanceName != "github-e2e" || current.Context.SSHPort != 39999 ||
		named.Context.IncusProject != "subyard-hermes-fixture" || named.Context.SSHPort != 2224 {
		t.Fatal("fixture yard identities or preset endpoint changed")
	}
	for path, before := range map[string][]byte{filepath.Join(configHome, "config.env"): hostBefore, defaultPath: defaultBefore, preset: presetBefore} {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("configuration assessment changed %s: %v", path, err)
		}
		if path != preset {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("fixture configuration mode changed: %v", err)
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(configHome, "yards/hermes-fixture/config.env")); !os.IsNotExist(err) {
		t.Fatal("assessment prematurely registered the named yard", err)
	}
}

// The peer fixture also exercises the implicit default GitHub profile. Its
// wrappers must retain the account home used by the real user manager. Its
// consumer root must agree with installed release inspection, which reloads
// persisted settings instead of inheriting command-only catalog overrides.
func TestPeerFixturePersistsGitHubConsumerRoot(t *testing.T) {
	root := testkit.TempDir(t)
	home := filepath.Join(root, "account home")
	peer := filepath.Join(root, "peer")
	if err := os.MkdirAll(filepath.Join(peer, "bin"), 0o700); err != nil {
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
	wrapperStart := strings.Index(string(source), "install_peer_wrapper() {\n")
	wrapperEnd := strings.Index(string(source), "reexec_with_incus_group() {\n")
	if wrapperStart < 0 || wrapperEnd <= wrapperStart {
		t.Fatal("peer wrapper setup boundary missing")
	}
	cleanupStart := strings.Index(string(source), "remove_peer_ssh_include() {\n")
	cleanupEnd := strings.Index(string(source), "source_upgrade_project_inventory() {\n")
	if cleanupStart < 0 || cleanupEnd <= cleanupStart {
		t.Fatal("peer cleanup boundary missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	engine := filepath.Join(root, "yard-engine")
	build := exec.CommandContext(ctx, "go", "build", "-o", engine, "./cmd/yard")
	build.Dir = repository
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build peer CLI: %v: %s", err, output)
	}
	ssh := filepath.Join(home, ".ssh")
	registration := filepath.Join(peer, "config/yards/peer/config.env")
	for _, directory := range []string{ssh, filepath.Dir(registration)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	retained := "Host retained\n    HostName retained.example\n"
	testkit.WriteFile(t, filepath.Join(ssh, "config"), []byte("Include subyard-peer.config\nInclude "+filepath.Join(ssh, "subyard-peer.config")+"\n"+retained), 0o600)
	testkit.WriteFile(t, filepath.Join(ssh, "subyard-peer.config"), []byte("Host yard-peer\n"), 0o600)
	testkit.WriteFile(t, registration, []byte("ACCESS_KIND=remote\nOWNER_ENDPOINT=dev@192.0.2.1\nOWNER_YARD_NAME=default\nREMOTE_SSH_PORT=3222\nREMOTE_DEV_USER=dev\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(peer, "ssh-config-state"), []byte("file\t640\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(peer, "ssh-config-backup"), []byte(retained), 0o640)
	calls := filepath.Join(root, "calls")
	entry := filepath.Join(home, ".local/bin/yard")
	testkit.WriteFile(t, filepath.Join(peer, "bin/yard"), []byte(`#!/bin/sh
[ "$HOME" = "$EXPECTED_HOME" ] && [ "$SUBYARD_OPERATOR_HOME" = "$EXPECTED_HOME" ] || {
  printf 'peer command did not retain the account home\n' >&2
  exit 23
}
[ "$SUBYARD_CONFIG_HOME" = "$PEER_ROOT/config" ] && [ "$SUBYARD_HOME" = "$PEER_DATA_ROOT" ] || exit 24
printf '%s\n' "$*" >> "$CALLS"
case "$1" in remote) exec "$PEER_NATIVE_ENGINE" "$@" ;; esac
`), 0o700)
	script := string(source)[wrapperStart:wrapperEnd] + string(source)[start:start+end] + string(source)[cleanupStart:cleanupEnd] + `
cleanup_peer_snapshot_fixture() { :; }
remove_peer_authorization() { :; }
cleanup_peer_incus() { :; }
clean_tree() { :; }
clean_peer_data() {
  [ ! -e "$PEER_ROOT/config/yards/peer/config.env" ] && [ ! -e "$HOME/.ssh/subyard-peer.config" ] || {
    printf 'peer alias survived smoke cleanup\n' >&2
    return 1
  }
}
bash() {
  [ "$#" = 2 ] && [ "$1" = -lc ] && [ "$2" = 'command -v yard' ] || return 99
  printf '%s\n' "$PEER_YARD_ENTRY"
}
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
install_peer_wrapper
bootstrap_peer_keys
peer_yard_start
peer_clean
peer_clean
`
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	consumer := filepath.Join(root, "consumer files")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "EXPECTED_HOME=" + home,
		"SUBYARD_REPOSITORY_ROOT=" + repository, "SUBYARD_NO_AUDIT=1", "PEER_NATIVE_ENGINE=" + engine, "PEER_CONFIG_STATE=" + filepath.Join(peer, "ssh-config-state"), "PEER_CONFIG_BACKUP=" + filepath.Join(peer, "ssh-config-backup"),
		"SUBYARD_E2E_VM=2", "PEER_ROOT=" + peer, "PEER_REAL_YARD_MARKER=" + filepath.Join(peer, "marker"), "MARKER=peer-fixture", "OWNER_BASE_IMAGE=fixture", "PEER_YARD_ENTRY=" + entry, "PEER_KEYS_CONSUMER_ROOT=" + consumer, "CALLS=" + calls,
		"PEER_DATA_ROOT=" + filepath.Join(root, "data"), "PEER_HOST_BASE=" + filepath.Join(root, "host"), "PEER_KEYS_ROOT=" + filepath.Join(root, "keys"), "PEER_KEYS_TOOLS=" + filepath.Join(root, "tools"),
		"PEER_YARD_STATE=" + filepath.Join(peer, "yard-state"), "PEER_YARD_BACKUP=" + filepath.Join(peer, "yard-backup"), "PEER_PROFILE_STATE=" + filepath.Join(peer, "profile-state"), "PEER_PROFILE_BACKUP=" + filepath.Join(peer, "profile-backup")}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("peer fixture: %v: %s", err, output)
	}
	content, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "_keys-init\ninit --yes\ninit --yes\ncheck\nstart --yes\nteardown --yes\nremote remove peer --yes\n" {
		t.Fatalf("peer workflow changed: %q", content)
	}
	content, err = os.ReadFile(filepath.Join(ssh, "config"))
	if err != nil || string(content) != retained {
		t.Fatalf("peer cleanup did not restore unrelated SSH config: %q, %v", content, err)
	}
	info, err := os.Stat(filepath.Join(ssh, "config"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("peer cleanup did not restore SSH config mode: %v, %v", info, err)
	}
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: repository, OperatorHome: home, DisablePrivate: true, Environment: map[string]string{
		"HOME": home, "SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": filepath.Join(peer, "config"), "SUBYARD_HOME": filepath.Join(root, "data"), "STORAGE_PATH": filepath.Join(root, "storage"), "HOST_BASE": filepath.Join(root, "host"), "RESTRICTED_DISK_PATHS": filepath.Join(root, "host"),
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
	info, err = os.Stat(filepath.Join(peer, "config", "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("peer config mode=%o, want 0600", info.Mode().Perm())
	}
}

func TestPeerYardRestoresAccountSSHConfig(t *testing.T) {
	repository, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(repository, "dev/e2e/p0-guest.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var functions string
	for _, boundary := range [][2]string{
		{"install_peer_wrapper() {\n", "reexec_with_incus_group() {\n"},
		{"peer_yard_start() {\n", "peer_info() {\n"},
		{"remove_peer_ssh_include() {\n", "source_upgrade_project_inventory() {\n"},
	} {
		start, end := strings.Index(string(source), boundary[0]), strings.Index(string(source), boundary[1])
		if start < 0 || end <= start {
			t.Fatalf("peer function boundary missing: %q", boundary)
		}
		functions += string(source)[start:end]
	}
	for _, initial := range []string{"absent", "existing"} {
		t.Run(initial, func(t *testing.T) {
			root := testkit.TempDir(t)
			home, peer := filepath.Join(root, "account home"), filepath.Join(root, "peer")
			for _, directory := range []string{filepath.Join(home, ".ssh"), filepath.Join(peer, "bin")} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			config := filepath.Join(home, ".ssh/config")
			retained := "Host retained\n    HostName retained.example" // Preserve a missing final newline too.
			if initial == "existing" {
				testkit.WriteFile(t, config, []byte(retained), 0o640)
			}
			testkit.WriteFile(t, filepath.Join(peer, "bin/yard"), []byte(`#!/usr/bin/env bash
set -euo pipefail
. "$REPOSITORY/scripts/lib/ssh-config.sh"
case "$1" in
  init)
    touch "$HOME/.ssh/config"
    chmod 0600 "$HOME/.ssh/config"
    ssh_config_prepend_once "$HOME/.ssh/config" 'Include subyard.config'
    ;;
  teardown) ssh_config_remove_exact "$HOME/.ssh/config" 'Include subyard.config' ;;
  check|start) ;;
  *) exit 99 ;;
esac
`), 0o700)
			script := functions + `
PEER_ROOT=$FIXTURE_ROOT/peer
PEER_YARD_ENTRY=$HOME/.local/bin/yard
PEER_YARD_STATE=$PEER_ROOT/yard-state PEER_YARD_BACKUP=$PEER_ROOT/yard-backup
PEER_PROFILE_STATE=$PEER_ROOT/profile-state PEER_PROFILE_BACKUP=$PEER_ROOT/profile-backup
PEER_CONFIG_STATE=$PEER_ROOT/ssh-config-state PEER_CONFIG_BACKUP=$PEER_ROOT/ssh-config-backup
PEER_REAL_YARD_MARKER=$PEER_ROOT/yard-marker
PEER_DATA_ROOT=$PEER_ROOT/data PEER_HOST_BASE=$PEER_ROOT/host
PEER_KEYS_ROOT=$PEER_ROOT/keys PEER_KEYS_TOOLS=$PEER_ROOT/tools PEER_KEYS_CONSUMER_ROOT=$PEER_ROOT/consumer
SUBYARD_E2E_VM=2 MARKER=peer-fixture
die() { printf '%s\n' "$*" >&2; exit 2; }
bash() { printf '%s\n' "$PEER_YARD_ENTRY"; }
timeout() { shift 2; "$@"; }
cleanup_peer_snapshot_fixture() { :; }
remove_peer_authorization() { :; }
cleanup_peer_incus() { :; }
clean_peer_data() { :; }
clean_tree() { :; }
install_peer_wrapper
peer_yard_start
peer_clean
peer_clean
`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "FIXTURE_ROOT=" + root, "REPOSITORY=" + repository}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("peer yard lifecycle: %v: %s", err, output)
			}
			content, err := os.ReadFile(config)
			if initial == "absent" {
				if !os.IsNotExist(err) {
					t.Fatalf("peer yard retained a new SSH config: %q, %v", content, err)
				}
				return
			}
			if err != nil || string(content) != retained {
				t.Fatalf("peer yard changed original SSH config bytes: %q, %v", content, err)
			}
			info, err := os.Stat(config)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("peer yard changed original SSH config mode: %v, %v", info, err)
			}
		})
	}
}
