package uninstall

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInstallationIdentityPreservesEstablishedCacheRoot(t *testing.T) {
	root := testkit.TempDir(t)
	identity, err := InstallationIdentity(uint32(os.Getuid()), filepath.Join(root, "data"), filepath.Join(root, "config"), filepath.Join(root, "runtime"), filepath.Join(root, "established-cache"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(identity.RuntimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := MarshalInstallation(identity)
	testkit.WriteFile(t, filepath.Join(identity.RuntimeRoot, ManifestName), body, 0600)
	derived, err := InstallationIdentity(identity.OperatorUID, identity.DataHome, identity.ConfigHome, identity.RuntimeRoot, filepath.Join(root, "download-cache"), true)
	if err != nil || !derived.SameIdentity(identity) {
		t.Fatal("transient download cache changed installation ownership", err)
	}
	if err := os.Mkdir(identity.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	conflict := identity
	conflict.CacheRoot = filepath.Join(root, "foreign-cache")
	body, _ = MarshalInstallation(conflict)
	testkit.WriteFile(t, filepath.Join(identity.DataHome, ManifestName), body, 0600)
	if _, err := InstallationIdentity(identity.OperatorUID, identity.DataHome, identity.ConfigHome, identity.RuntimeRoot, identity.CacheRoot, true); err == nil {
		t.Fatal("conflicting protected scopes admitted")
	}
	if _, err := InstallationIdentity(identity.OperatorUID, identity.DataHome, identity.ConfigHome, identity.RuntimeRoot, identity.CacheRoot, false); err != nil {
		t.Fatal("private draft scope consumed bootstrap identity", err)
	}
	if err := os.Remove(filepath.Join(identity.RuntimeRoot, ManifestName)); err != nil {
		t.Fatal(err)
	}
	body, _ = MarshalInstallation(identity)
	testkit.WriteFile(t, filepath.Join(identity.DataHome, ManifestName), body, 0600)
	derived, err = InstallationIdentity(identity.OperatorUID, identity.DataHome, identity.ConfigHome, identity.RuntimeRoot, filepath.Join(root, "download-cache"), true)
	if err != nil || !derived.SameIdentity(identity) {
		t.Fatal("canonical staging failed to recover established full-scope cache identity", err)
	}
}

func TestInstallationIdentityRefusesAliasedScopes(t *testing.T) {
	root := testkit.TempDir(t)
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{data, filepath.Join(root, "runtime-alias")} {
		if runtime != data {
			if err := os.Symlink(data, runtime); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := InstallationIdentity(uint32(os.Getuid()), data, filepath.Join(root, "config"), runtime, filepath.Join(root, "cache"), true); err == nil {
			t.Fatal("runtime and bootstrap scopes aliased")
		}
	}
}

func TestInstallationEnrollmentExactScopeAndCAS(t *testing.T) {
	for _, scenario := range []string{"initial", "matching", "conflicting", "uid", "hardlink", "symlink", "mode", "special mode", "malformed", "oversized", "concurrent creation", "replacement", "root replacement", "epoch", "pending uninstall", "full claims"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			identity, err := InstallationIdentity(uint32(os.Getuid()), filepath.Join(root, "data"), filepath.Join(root, "config"), filepath.Join(root, "runtime"), filepath.Join(root, "cache"), true)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(identity.RuntimeRoot, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(identity.RuntimeRoot, ManifestName)
			body, _ := json.Marshal(identity)
			write := func() { testkit.WriteFile(t, path, body, 0600) }
			switch scenario {
			case "matching", "replacement":
				write()
			case "conflicting":
				value := identity
				value.ConfigHome = filepath.Join(root, "foreign")
				body, _ = json.Marshal(value)
				write()
			case "uid":
				value := identity
				value.OperatorUID++
				body, _ = json.Marshal(value)
				write()
			case "full claims":
				value := identity
				value.BinDir = filepath.Join(root, "bin")
				body, _ = json.Marshal(value)
				write()
			case "hardlink":
				write()
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				testkit.WriteFile(t, path+".target", body, 0600)
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			case "mode":
				testkit.WriteFile(t, path, body, 0644)
			case "special mode":
				testkit.WriteFile(t, path, body, 0600|os.ModeSetuid)
			case "malformed":
				testkit.WriteFile(t, path, []byte("{"), 0600)
			case "oversized":
				testkit.WriteFile(t, path, append(body, []byte(strings.Repeat(" ", 65536))...), 0600)
			}
			before, err := InspectInstallation(identity.RuntimeRoot, identity)
			if scenario == "conflicting" || scenario == "uid" || scenario == "hardlink" || scenario == "symlink" || scenario == "mode" || scenario == "special mode" || scenario == "malformed" || scenario == "oversized" || scenario == "full claims" {
				if err == nil {
					before.Close()
					t.Fatal("unsafe inventory admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer before.Close()
			if _, err := os.Lstat(path); scenario != "matching" && scenario != "replacement" && !os.IsNotExist(err) {
				t.Fatal("inspection wrote inventory")
			}
			var original os.FileInfo
			if scenario == "matching" {
				original, _ = os.Lstat(path)
			}
			_, release, err := OwnerLock(context.Background(), identity.DataHome, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			switch scenario {
			case "concurrent creation":
				write()
			case "replacement":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				write()
			case "root replacement":
				if err := os.Rename(identity.RuntimeRoot, identity.RuntimeRoot+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(identity.RuntimeRoot, 0700); err != nil {
					t.Fatal(err)
				}
			case "epoch":
				testkit.WriteFile(t, OwnerEpochPath(identity.DataHome), []byte("1\n"), 0600)
			case "pending uninstall":
				if err := os.Mkdir(identity.DataHome+".uninstall", 0700); err != nil {
					t.Fatal(err)
				}
			}
			err = before.Publish(identity)
			if scenario == "concurrent creation" || scenario == "replacement" || scenario == "root replacement" || scenario == "epoch" || scenario == "pending uninstall" {
				if err == nil {
					t.Fatal("stale publication succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := ReadInstallationFile(path, identity.DataHome, identity.OperatorUID)
			if err != nil || !value.SameIdentity(identity) || value.BinDir != "" || len(value.ShellFiles) != 0 {
				t.Fatal("wrong scoped inventory", err)
			}
			if original != nil {
				current, _ := os.Lstat(path)
				if !os.SameFile(original, current) || original.ModTime() != current.ModTime() {
					t.Fatal("matching inventory rewritten")
				}
			}
		})
	}
}
