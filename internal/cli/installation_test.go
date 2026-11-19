package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestInstallationCacheEnrollmentRejectsVerifiedReplacement(t *testing.T) {
	home := testkit.TempDir(t)
	cache := filepath.Join(home, "cache")
	runtime := filepath.Join(home, "runtime")
	for _, path := range []string{cache, runtime} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{filepath.Join(cache, "bundle"), filepath.Join(cache, "checksum")}
	var identities []uninstall.Entry
	for _, path := range paths {
		testkit.WriteFile(t, path, []byte("verified"), 0o600)
		tree, err := uninstall.CaptureRoot(path)
		if err != nil || len(tree.Entries) != 1 {
			t.Fatalf("capture cache artifact: %v", err)
		}
		entry := tree.Entries[0]
		entry.Path = path
		identities = append(identities, entry)
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte("verified"), 0o600)
	}
	input, err := json.Marshal(map[string][]uninstall.Entry{"VerifiedCacheInputs": identities})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: repositoryRoot(t), Stdin: bytes.NewReader(input), Stderr: &stderr,
		Environment: []string{"HOME=" + home, "YARD_RELEASE_CACHE=" + cache, "YARD_RUNTIME_ROOT=" + runtime}})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.runInstallation(context.Background(), []string{"runtime"}); code != 1 ||
		strings.Count(stderr.String(), "verified installation cache artifact changed") != len(paths) {
		t.Fatalf("cache identity refusal = %d, %s", code, stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(runtime, uninstall.ManifestName)); !os.IsNotExist(err) {
		t.Fatalf("refused cache enrollment published inventory: %v", err)
	}
}

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
