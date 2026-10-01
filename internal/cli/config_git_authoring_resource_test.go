package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigGitAuthoringRetryRequiresShutdownBeforeApplyingCommittedProfileChange(t *testing.T) {
	f := newConfigGitAuthoringFixture(t)
	root, nativeEnvironment, _ := nativeFixture(t)
	writeConfigCommandFile(t, filepath.Join(root, "config/subyard.env"), "DEV_UID=1000\nDEV_USER=dev\nSSH_PORT=2222\nYARD_KIND=container\nSHIFT_MODE=shift\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nSTORAGE_PATH=$SUBYARD_HOME/storage\nHOST_BASE=/srv/subyard\nRESTRICTED_DISK_PATHS=/srv/subyard\n", 0o600)
	resourceDir := filepath.Join(root, "config/profiles/sample/resources")
	writeConfigCommandFile(t, filepath.Join(resourceDir, "relay.res"), `COMMAND=relay
HANDLER=resources/relay/handler.sh
TITLE="Sample relay"
PROXY="sample-relay RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT RESOURCE_RELAY_INTERFACE udp:guest:41999 owner-metadata-v1 owner-ipv4-udp"
ACTION="up up public-ingress-change reversible"
ACTION="down down public-ingress-change reversible"
BRINGUP=up
SHUTDOWN=down
`, 0o600)
	writeConfigCommandFile(t, filepath.Join(resourceDir, "relay/handler.sh"), `#!/bin/sh
set -eu
[ "${SUBYARD_RESOURCE_MODE:-}" = prepare ] && [ "$1" = down ] || exit 71
printf '{"schema":"yard.resource-action-assessment.v1","action":"down","changed":true,"consequences":["close sample relay"]}\n'
`, 0o700)
	environment := append(nativeEnvironment, withoutCommandSetting(f.environment, "SUBYARD_CONFIG_DIR")...)
	environment = append(environment, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
	checkout := filepath.Join(f.home, ".local/share/subyard-config")
	source := filepath.Join(checkout, "hosts/owner-a/yards/demo/config.env")
	old := "YARD_KIND=vm\nSSH_PORT=2300\nENVIRONMENT_PROFILES=sample\n"
	writeConfigCommandFile(t, source, old, 0o600)
	commitConfigSource(t, checkout, "Select sample profile")
	plan, err := configsync.BuildPlan(configsync.Options{
		SourceRoot: checkout, ConfigHome: f.configHome, RepositoryRoot: root,
		OperatorHome: f.home, Environment: environmentMap(environment), Adopt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := configsync.Apply(plan); err != nil {
		t.Fatal(err)
	}
	// The requested change already exists in Git, while an earlier failed apply
	// left the active resource selected in the cached effective settings.
	writeConfigCommandFile(t, source, "YARD_KIND=vm\nSSH_PORT=2300\nENVIRONMENT_PROFILES=''\n", 0o600)
	commitConfigSource(t, checkout, "Deselect sample profile")
	cache := filepath.Join(f.configHome, config.GitSettingsRelativePath, "yards/demo/config.env")
	cacheBefore, err := os.ReadFile(cache)
	if err != nil || string(cacheBefore) != old {
		t.Fatalf("retry fixture lost the previous cached selection: %q, %v", cacheBefore, err)
	}
	manifestBefore, err := os.ReadFile(configsync.ManifestPath(f.configHome))
	if err != nil {
		t.Fatal(err)
	}
	checkoutBefore := snapshotConfigCheckoutRaw(t, checkout)
	remoteBefore := strings.TrimSpace(configSourceGitOutput(t, f.remote, "rev-parse", "HEAD"))
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		WorkingDir: root, Prompt: prompt, Stdout: &bytes.Buffer{}, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("demo")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["ENVIRONMENT_PROFILES"] != "sample" {
		t.Fatalf("effective resource profile=%q", loaded.Environment["ENVIRONMENT_PROFILES"])
	}
	request := configAuthoringRequest{action: "set", name: "ENVIRONMENT_PROFILES", scope: config.ScopeYard, git: true}
	prepared, err := program.prepareConfigSyncPush(context.Background(), loaded, configSyncPushOptions{authoring: &request})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.createdCommit || !prepared.preview.NeedsApply() {
		t.Fatalf("retry fixture must need cache application without another commit: created=%t apply=%t", prepared.createdCommit, prepared.preview.NeedsApply())
	}
	prepared.cleanup(program, context.Background())
	code := program.runConfigAuthoring(context.Background(), loaded, "set",
		[]string{"ENVIRONMENT_PROFILES", "", "--scope", "yard", "--git"}, false)
	if code != 1 || !strings.Contains(stderr.String(), "run relay down before changing ENVIRONMENT_PROFILES") {
		t.Fatalf("unsafe committed-profile retry: code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("unsafe retry reached confirmation: %#v", prompt.Requests)
	}
	if after, err := os.ReadFile(cache); err != nil || !bytes.Equal(after, cacheBefore) {
		t.Fatalf("unsafe retry changed the cache: %q, %v", after, err)
	}
	if after, err := os.ReadFile(configsync.ManifestPath(f.configHome)); err != nil || !bytes.Equal(after, manifestBefore) {
		t.Fatalf("unsafe retry changed the manifest: %v", err)
	}
	checkoutAfter := snapshotConfigCheckoutRaw(t, checkout)
	if !reflect.DeepEqual(checkoutAfter, checkoutBefore) {
		t.Fatal("unsafe retry changed the registered checkout")
	}
	if after := strings.TrimSpace(configSourceGitOutput(t, f.remote, "rev-parse", "HEAD")); after != remoteBefore {
		t.Fatal("unsafe retry pushed the pending profile change")
	}
}
