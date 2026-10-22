package profile

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBoundedSourceDigestRejectsGrowthWithoutReadingPastBound(t *testing.T) {
	for _, size := range []int{0, 64 << 10, 8 << 20} {
		for _, delta := range []int{-1, 0, 2} {
			if size+delta < 0 {
				continue
			}
			content := strings.Repeat("x", size+delta)
			reader := strings.NewReader(content)
			digest, err := boundedSourceDigest(reader, int64(size))
			if delta == 0 {
				if err != nil || digest != fmt.Sprintf("%x", sha256.Sum256([]byte(content))) {
					t.Fatalf("size=%d digest=%q err=%v", size, digest, err)
				}
			} else if err == nil || digest != "" {
				t.Fatalf("changed size accepted: size=%d delta=%d digest=%q err=%v", size, delta, digest, err)
			}
			if consumed := len(content) - reader.Len(); consumed > size+1 {
				t.Fatalf("hash read %d bytes beyond captured size %d", consumed, size)
			}
		}
	}
}

func TestPresetSourcesEnforceExactBoundAndWritablePolicy(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "yard.env")
	for _, mode := range []os.FileMode{0o644, 0o664, 0o646} {
		for _, size := range []int{8 << 20, (8 << 20) + 1} {
			testkit.WriteFile(t, path, []byte(strings.Repeat("x", size)), mode)
			source, err := packageSource(root, "yard.env")
			if mode == 0o644 && size == 8<<20 {
				data, readErr := source.Read()
				if err != nil || readErr != nil || len(data) != size {
					t.Fatalf("bounded preset refused: source=%v read=%v size=%d", err, readErr, len(data))
				}
			} else if err == nil || err.Error() != "profile preset must be bounded and not writable by other users" {
				t.Fatalf("unsafe preset accepted: mode=%o size=%d err=%v", mode, size, err)
			}
		}
	}
}

func TestProvisionBundleKeepsPolicySpecificToPackageSources(t *testing.T) {
	root := testkit.TempDir(t)
	dir := filepath.Join(root, "config/profiles/sample")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "profile.conf"), []byte("safe\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(dir, "provision.sh"), []byte("#!/bin/sh\n# subyard-provision-check-v1\ntrue\n"), 0o755)
	testkit.WriteFile(t, filepath.Join(dir, "assets/profile.json"), []byte(strings.Repeat("x", (64<<10)+1)), 0o644)
	testkit.WriteFile(t, filepath.Join(dir, "assets/yard.env"), nil, 0o666)
	pkg, err := ResolvePackage(root, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pkg.BindProvision(); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionBindingAppliesSourcePolicyBeforeHashingChangedFiles(t *testing.T) {
	for _, name := range []string{"profile.json", "yard.env"} {
		for _, change := range []string{"expand", "replace", "writable"} {
			if change == "writable" && name != "yard.env" {
				continue
			}
			t.Run(name+"/"+change, func(t *testing.T) {
				root := testkit.TempDir(t)
				dir := filepath.Join(root, "config/profiles/sample")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(dir, "profile.conf"), []byte("safe\n"), 0o644)
				testkit.WriteFile(t, filepath.Join(dir, "provision.sh"), []byte("#!/bin/sh\n# subyard-provision-check-v1\ntrue\n"), 0o755)
				path := filepath.Join(dir, name)
				testkit.WriteFile(t, path, nil, 0o644)
				pkg, err := ResolvePackage(root, "sample")
				if err != nil {
					t.Fatal(err)
				}
				binding, err := pkg.BindProvision()
				if err != nil {
					t.Fatal(err)
				}
				if change == "replace" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, path, nil, 0o644)
				}
				want, bound := "shipped declaration must be bounded", int64(64<<10)
				if name == "yard.env" {
					want, bound = "profile preset must be bounded and not writable by other users", 8<<20
				}
				if change == "writable" {
					if err := os.Chmod(path, 0o666); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Truncate(path, bound+1); err != nil {
					t.Fatal(err)
				}
				if _, err := pkg.BindProvision(); err == nil || err.Error() != want {
					t.Fatalf("binding missed pre-hash policy: %v", err)
				}
				if err := binding.Check(); err == nil || err.Error() != want {
					t.Fatalf("binding check missed pre-hash policy: %v", err)
				}
			})
		}
	}
}

func TestRepositoryEntrypointAliasRejectsAppendedSymlinks(t *testing.T) {
	for _, relative := range []string{"config", "config/profiles", "config/profiles/sample"} {
		t.Run(relative, func(t *testing.T) {
			base := testkit.TempDir(t)
			root := filepath.Join(base, "repository")
			target := filepath.Join(base, "external")
			remaining, err := filepath.Rel(relative, "config/profiles/sample")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(target, remaining), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(target, remaining, "profile.conf"), []byte("safe\n"), 0o644)
			path := filepath.Join(root, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(base, "alias")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			for _, entrypoint := range []string{root, alias} {
				if _, err := Packages(entrypoint); err == nil {
					t.Fatal("inventory accepted appended symlink")
				}
				if _, err := ResolvePackage(entrypoint, "sample"); err == nil {
					t.Fatal("resolver accepted appended symlink")
				}
			}
		})
	}
}

func TestRepositoryEntrypointAliasPreservesPackageSources(t *testing.T) {
	base := testkit.TempDir(t)
	root := filepath.Join(base, "repository")
	path := filepath.Join(root, "config/profiles/sample/profile.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("safe\n"), 0o644)
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	packages, err := Packages(alias)
	if err != nil || len(packages) != 1 {
		t.Fatalf("inventory=%v err=%v", packages, err)
	}
	pkg, err := ResolvePackage(alias, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := pkg.Config.Read(); err != nil || string(data) != "safe\n" {
		t.Fatalf("source=%q err=%v", data, err)
	}
}

func TestPackagesBindDescriptorlessSourcesAndCompleteProvisionBundle(t *testing.T) {
	root := testkit.TempDir(t)
	dir := filepath.Join(root, "config/profiles/sample")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "config/profiles/resource-only/resources"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "profile.conf"), []byte("PROFILE_NAME=sample\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(dir, "provision.sh"), []byte("#!/bin/sh\n# subyard-provision-check-v1\ntrue\n"), 0o755)
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/resource-only/resources/service.res"), []byte("fixture"), 0o644)
	packages, err := Packages(root)
	if err != nil || len(packages) != 2 {
		t.Fatalf("packages=%+v err=%v", packages, err)
	}
	pkg, err := ResolvePackage(root, "sample")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := pkg.BindProvision()
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Check(); err != nil {
		t.Fatal(err)
	}
	// A previously absent file changes the approved archive, even with no hook edit.
	testkit.WriteFile(t, filepath.Join(dir, "provision-tmp-paths"), nil, 0o644)
	if err := binding.Check(); err == nil {
		t.Fatal("new bundle file accepted")
	}
	if err := os.Remove(filepath.Join(dir, "profile.conf")); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "profile.conf"), []byte("PROFILE_NAME=sample\n"), 0o644)
	if _, err := pkg.Config.Read(); err == nil {
		t.Fatal("same-byte replacement accepted")
	}
}

func TestPackageSourcesKeepRetainedRepositoryAndRejectParentReplacement(t *testing.T) {
	base := testkit.TempDir(t)
	root := filepath.Join(base, "repository")
	dir := filepath.Join(root, "config/profiles/sample")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "profile.conf"), []byte("safe\n"), 0o644)
	ordinary, err := ResolvePackage(root, "sample")
	if err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	pinned, err := ResolvePackage(fmt.Sprintf("/proc/self/fd/%d", fd.Fd()), "sample")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "profile.conf"), []byte("replacement\n"), 0o644)
	if data, err := pinned.Config.Read(); err != nil || string(data) != "safe\n" {
		t.Fatalf("pinned=%q err=%v", data, err)
	}
	if _, err := ordinary.Config.Read(); err == nil {
		t.Fatal("replaced repository accepted")
	}
}

func TestPackagesRejectUnsafeNativeReferences(t *testing.T) {
	for _, name := range []string{"profile.conf", "profile.json", "provision.sh", "yard.env", "resources/service.res"} {
		for _, kind := range []string{"symlink", "hardlink", "directory", "fifo"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				root := testkit.TempDir(t)
				path := filepath.Join(root, "config/profiles/sample", name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, "target")
				testkit.WriteFile(t, target, []byte("fixture"), 0o644)
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(target, path)
				case "hardlink":
					err = os.Link(target, path)
				case "directory":
					err = os.Mkdir(path, 0o700)
				case "fifo":
					err = syscall.Mkfifo(path, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				if name == "yard.env" {
					testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/profile.conf"), []byte("PROFILE_NAME=sample\n"), 0o644)
					testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/provision.sh"), []byte("#!/bin/sh\n# subyard-provision-check-v1\ntrue\n"), 0o755)
					packages, err := Packages(root)
					if err != nil || len(packages) != 1 || packages[0].Preset != nil || packages[0].PresetError == nil {
						t.Fatalf("unsafe preset availability=%v err=%v", packages, err)
					}
					if _, err := packages[0].BindProvision(); err == nil {
						t.Fatal("unsafe preset admitted to provision bundle")
					}
				} else if _, err := Packages(root); err == nil {
					t.Fatal("unsafe reference accepted")
				}
			})
		}
	}
}

func TestDeclarationSourcesRejectOversizeBeforeBinding(t *testing.T) {
	for _, kind := range []string{"profiles", "agents", "retired-integrations"} {
		t.Run(kind, func(t *testing.T) {
			root := testkit.TempDir(t)
			name := "integration.json"
			if kind == "profiles" {
				name = "profile.json"
			}
			dir := filepath.Join(root, "config", kind, "sample")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{64 << 10, (64 << 10) + 1} {
				testkit.WriteFile(t, filepath.Join(dir, name), make([]byte, size), 0o644)
				_, sourceErr := packageSource(dir, name)
				declarations, loadErr := loadDeclarations(root, kind, name)
				if size > 64<<10 {
					if sourceErr == nil || sourceErr.Error() != "shipped declaration must be bounded" || loadErr == nil {
						t.Fatalf("oversized declaration reached source binding: source=%v load=%v", sourceErr, loadErr)
					}
				} else if sourceErr != nil || loadErr != nil || len(declarations) != 1 || len(declarations[0].data) != size {
					t.Fatalf("bounded declaration refused: source=%v load=%v declarations=%d", sourceErr, loadErr, len(declarations))
				}
			}
		})
	}
}

func TestReadBoundSourceRejectsWrongDescriptorAndUnapprovedBytes(t *testing.T) {
	root := testkit.TempDir(t)
	approved := filepath.Join(root, "approved")
	testkit.WriteFile(t, approved, []byte("approved\n"), 0o600)
	expected, err := sourceIdentity(approved, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"approved", "wrong descriptor", "same-byte replacement", "changed bytes"} {
		t.Run(scenario, func(t *testing.T) {
			path, identity := approved, expected
			if scenario != "approved" {
				path = filepath.Join(root, scenario)
				content := []byte("different\n")
				if scenario == "same-byte replacement" {
					content = []byte("approved\n")
				}
				testkit.WriteFile(t, path, content, 0o600)
				if scenario == "changed bytes" {
					identity, err = sourceIdentity(path, true)
					if err != nil {
						t.Fatal(err)
					}
					// Matching descriptor metadata cannot authorize different content.
					identity.Digest = expected.Digest
				}
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := readBoundSource(file, identity)
			if scenario == "approved" {
				if err != nil || string(data) != "approved\n" {
					t.Fatalf("approved=%q err=%v", data, err)
				}
			} else if err == nil || data != nil {
				t.Fatalf("unapproved descriptor returned bytes: %q err=%v", data, err)
			}
		})
	}
}

func TestPackageEntryAliasKeepsStrictChildrenAndRetainedRoot(t *testing.T) {
	root := testkit.TempDir(t)
	profiles := filepath.Join(root, "config/profiles")
	if err := os.MkdirAll(filepath.Join(profiles, "sample"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(profiles, "sample/profile.conf"), []byte("safe\n"), 0o644)
	alias := filepath.Join(root, "profiles-alias")
	if err := os.Symlink(profiles, alias); err != nil {
		t.Fatal(err)
	}
	packages, err := PackagesIn(alias)
	if err != nil || len(packages) != 1 {
		t.Fatalf("alias inventory=%v err=%v", packages, err)
	}
	fd, err := os.Open(profiles)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	pinned, err := ResolvePackageIn(fmt.Sprintf("/proc/self/fd/%d", fd.Fd()), "sample")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(profiles, profiles+"-old"); err != nil {
		t.Fatal(err)
	}
	if data, err := pinned.Config.Read(); err != nil || string(data) != "safe\n" {
		t.Fatalf("pinned=%q err=%v", data, err)
	}
	if err := os.Mkdir(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(profiles+"-old", "sample"), filepath.Join(profiles, "sample")); err != nil {
		t.Fatal(err)
	}
	if _, err := PackagesIn(alias); err == nil {
		t.Fatal("entry alias normalized unsafe package child")
	}
	if _, err := ResolvePackageIn(alias, "sample"); err == nil {
		t.Fatal("entry alias resolved unsafe package child")
	}
}
