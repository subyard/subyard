package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestInstallationActivationOwnsCanonicalScope(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-installation")
	if err := os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	c := &releaseOwnerComponents{cli: program, context: releaseowner.Context{RuntimeRoot: runtimeRoot, ConfigHome: environmentValue(environment, "SUBYARD_CONFIG_HOME"), DataHome: environmentValue(environment, "SUBYARD_HOME"), Target: "release-b", Session: strings.Repeat("a", 64), Direction: releasetransition.DirectionActivateTarget}, activation: map[string]releaseActivationPrepared{}}
	defer c.Close()
	ctx := context.Background()
	before, err := c.captureCoreActivation(ctx, "installation-runtime-v1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtimeRoot, uninstall.ManifestName)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("read-only capture wrote inventory")
	}
	if err := c.ApplyActivation(ctx, before[0]); err != nil {
		t.Fatal(err)
	}
	after, err := c.captureCoreActivation(ctx, "installation-runtime-v1")
	if err != nil {
		t.Fatal(err)
	}
	if after[0].State != "current" || before[0].Desired != after[0].Desired || before[0].Source == after[0].Source {
		t.Fatal("enrollment changed recovery scope or lost native binding")
	}
	if _, err := os.Lstat(filepath.Join(c.context.DataHome, uninstall.ManifestName)); !os.IsNotExist(err) {
		t.Fatal("runtime enrollment acquired full bootstrap scope")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, content, 0600)
	replacement, _ := os.Lstat(path)
	if err := c.ApplyActivation(ctx, after[0]); err == nil {
		t.Fatal("same-content concurrent replacement admitted")
	}
	current, _ := os.Lstat(path)
	if !os.SameFile(replacement, current) {
		t.Fatal("stale enrollment changed concurrent record")
	}
	fresh, err := c.captureCoreActivation(ctx, "installation-runtime-v1")
	if err != nil {
		t.Fatal(err)
	}
	if fresh[0].Source == after[0].Source || fresh[0].Desired != after[0].Desired {
		t.Fatal("replacement not bound independently of desired scope")
	}
}
