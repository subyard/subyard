package integrationassets

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDigestUsesTrackedIntegrationInventory(t *testing.T) {
	root := testkit.TempDir(t)
	copyTrackedConfig(t, root)
	artifact := "config/agents/widget/tests/template.toml"
	writeAsset(t, filepath.Join(root, "config/agents/widget/integration.json"), []byte(`{"schema_version":2,"artifacts":[{"id":"config","kind":"config","source":"tests/template.toml","destination":".widget/config.toml","format":"raw"}]}`), 0o644)
	writeAsset(t, filepath.Join(root, artifact), []byte("key = 1\n"), 0o644)
	writeAsset(t, filepath.Join(root, "config/agents/widget/tests/unused"), []byte("not installed"), 0o644)
	tracked, err := TrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(root, tracked)
	if err != nil {
		t.Fatal(err)
	}
	contains := func(path string) bool {
		for _, source := range before.Sources {
			if source == path {
				return true
			}
		}
		return false
	}
	if !contains("config/projects-changed.sh") || !contains(artifact) {
		t.Fatalf("installed sources outside config/agents escaped: %v", before.Sources)
	}
	writeAsset(t, filepath.Join(root, "config/agents/widget/tests/unused"), []byte("changed unreferenced fixture"), 0o644)
	writeAsset(t, filepath.Join(root, "config/agents/scratch/integration.json"), []byte("invalid untracked metadata"), 0o644)
	after, err := Inspect(root, tracked)
	if err != nil || after.Digest != before.Digest {
		t.Fatalf("untracked scratch or unreferenced fixture affected inventory: %s %s %v", before.Digest, after.Digest, err)
	}
	for _, path := range []string{"config/projects-changed.sh", artifact} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		writeAsset(t, filepath.Join(root, path), append(data, []byte("\n# changed\n")...), info.Mode().Perm())
		changed, err := Inspect(root, tracked)
		if err != nil || changed.Digest == before.Digest {
			t.Fatalf("installed input %s escaped digest: %v", path, err)
		}
		writeAsset(t, filepath.Join(root, path), data, info.Mode().Perm())
	}
}

func TestDigestFilesOrdersRecordsAndIncludesReferencedTestAssets(t *testing.T) {
	root := testkit.TempDir(t)
	for _, path := range []string{"a.b", "a/asset", "tests/shipped"} {
		writeAsset(t, filepath.Join(root, path), []byte(path), 0o644)
	}
	first, err := DigestFiles(root, []string{"a/asset", "tests/shipped", "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := DigestFiles(root, []string{"tests/shipped", "a.b", "a/asset"})
	if err != nil || first != second {
		t.Fatalf("ordering changed digest: %s %s %v", first, second, err)
	}
	writeAsset(t, filepath.Join(root, "tests/shipped"), []byte("changed"), 0o644)
	third, err := DigestFiles(root, []string{"tests/shipped", "a.b", "a/asset"})
	if err != nil || third == first {
		t.Fatalf("referenced tests/ asset excluded: %v", err)
	}
}

func writeAsset(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, data, mode)
}

func TestInventoryUsesTheRuntimeProfileLoader(t *testing.T) {
	root := testkit.TempDir(t)
	copyTrackedConfig(t, root)
	// Nested test data resembles a descriptor but is not a shipped profile.
	writeAsset(t, filepath.Join(root, "config/profiles/fixture/profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"fixture-runtime","handler":"runtime.sh","format":2}}`), 0o644)
	writeAsset(t, filepath.Join(root, "config/profiles/fixture/runtime.sh"), []byte("#!/bin/sh\n"), 0o755)
	writeAsset(t, filepath.Join(root, "config/profiles/fixture/tests/profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"not-a-runtime","handler":"runtime.sh"}}`), 0o644)
	writeAsset(t, filepath.Join(root, "config/projects-changed.sh"), nil, 0o755)
	tracked, err := TrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	writeAsset(t, filepath.Join(root, "config/profiles/scratch/profile.json"), []byte("invalid untracked descriptor"), 0o644)
	inventory, err := Inspect(root, tracked)
	if err != nil {
		t.Fatal(err)
	}
	if _, nested := inventory.Profiles["not-a-runtime"]; nested || inventory.Profiles["fixture-runtime"].Format != 2 {
		t.Fatalf("runtime profile discovery differs: %#v", inventory.Profiles)
	}
	// A raw JSON scan would accept the declared runtime despite an invalid handler.
	writeAsset(t, filepath.Join(root, "config/profiles/fixture/runtime.sh"), []byte("not executable"), 0o644)
	if _, err := Inspect(root, tracked); err == nil {
		t.Fatal("profile package validation bypassed")
	}
}

func copyTrackedConfig(t *testing.T, root string) {
	t.Helper()
	checkout := filepath.Join("..", "..", "..")
	paths, err := TrackedFiles(checkout)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "config/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(checkout, path))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(checkout, path))
		if err != nil {
			t.Fatal(err)
		}
		writeAsset(t, filepath.Join(root, path), data, info.Mode().Perm())
	}
}

// A release snapshot has no Git metadata: it is either outside any repository
// or inside an ignored directory of the checkout it was prepared from.
func TestTrackedFilesMatchesGitCheckoutAndReleaseSnapshots(t *testing.T) {
	checkout := tempOutsideTestTemp(t)
	copyTrackedConfig(t, checkout)
	writeAsset(t, filepath.Join(checkout, ".gitignore"), []byte("/.build/\n/scratch\n"), 0o644)
	writeAsset(t, filepath.Join(checkout, "scratch"), []byte("ignored"), 0o644)
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--all"}} {
		if output, err := exec.Command("git", append([]string{"-C", checkout}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v: %s", err, output)
		}
	}
	listed, err := exec.Command("git", "-C", checkout, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimSuffix(string(listed), "\x00"), "\x00")
	slices.Sort(want)
	outside := tempOutsideTestTemp(t)
	if err := exec.Command("git", "-C", outside, "rev-parse", "--git-dir").Run(); err == nil {
		t.Fatalf("%s is inside a Git repository", outside)
	}
	var digest string
	for _, root := range []string{checkout, outside, filepath.Join(checkout, ".build", "source")} {
		if root != checkout {
			for _, path := range want {
				data, err := os.ReadFile(filepath.Join(checkout, path))
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(filepath.Join(checkout, path))
				if err != nil {
					t.Fatal(err)
				}
				writeAsset(t, filepath.Join(root, path), data, info.Mode().Perm())
			}
		}
		got, err := TrackedFiles(root)
		if err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("%s lists %d files, the Git checkout tracks %d", root, len(got), len(want))
		}
		inventory, err := Inspect(root, got)
		if err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		if digest == "" {
			digest = inventory.Digest
		} else if inventory.Digest != digest {
			t.Fatalf("%s digest %s differs from the checkout's %s", root, inventory.Digest, digest)
		}
	}
	if files, err := TrackedFiles(tempOutsideTestTemp(t)); err == nil {
		t.Fatalf("empty source tree listed %q", files)
	}
}

// TMPDIR can point into a checkout, so these roots are created under /tmp.
func tempOutsideTestTemp(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "subyard-integration-sources-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}
