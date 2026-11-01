package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestInstallationActivationOwnsCanonicalScope(t *testing.T) {
	root := repositoryRoot(t)
	home := testkit.TempDir(t)
	environment := []string{"HOME=" + home, "SUBYARD_HOME=" + filepath.Join(home, "data"), "SUBYARD_CONFIG_HOME=" + filepath.Join(home, "config")}
	runtimeRoot := filepath.Join(testkit.TempDir(t), "runtime-installation")
	if err := os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	r := &installationActivationReconciler{cli: program, request: releasetransition.ProcessRequest{RuntimeRoot: runtimeRoot, ConfigHome: environmentValue(environment, "SUBYARD_CONFIG_HOME"), Target: "release-b"}}
	ctx := context.Background()
	before, err := r.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r.before != nil {
			r.before.Close()
		}
	}()
	path := filepath.Join(runtimeRoot, uninstall.ManifestName)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("read-only capture wrote inventory")
	}
	if err := r.Reconcile(ctx, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	after, err := r.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if !after.Converged || before.Desired != after.Desired || before.Binding == after.Binding {
		t.Fatal("enrollment changed recovery scope or lost native binding")
	}
	if _, err := os.Lstat(filepath.Join(home, "data", uninstall.ManifestName)); !os.IsNotExist(err) {
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
	if err := r.Reconcile(ctx, releasetransition.ReleaseLinks{}); err == nil {
		t.Fatal("same-content concurrent replacement admitted")
	}
	current, _ := os.Lstat(path)
	if !os.SameFile(replacement, current) {
		t.Fatal("stale enrollment changed concurrent record")
	}
	fresh, err := r.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Binding == after.Binding || fresh.Desired != after.Desired {
		t.Fatal("replacement not bound independently of desired scope")
	}
}
