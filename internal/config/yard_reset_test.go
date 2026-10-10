package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestYardResetSuppressesFallbackAndKeepsNewLocalSettings(t *testing.T) {
	for _, name := range []string{"named", "default"} {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.Abs("../..")
			if err != nil {
				t.Fatal(err)
			}
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, "config")
			git := filepath.Join(configHome, GitSettingsRelativePath)
			write := func(path, value string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte(value), 0600)
			}
			gitYard := filepath.Join(git, "yards", name, "config.env")
			write(gitYard, "YARD_IMAGE=old:image\nSSH_PORT=3555\n")
			write(filepath.Join(git, "overrides", "shared", "config.env"), "DEV_USER=shareddev\n")
			write(filepath.Join(configHome, "config.env"), "SSH_PORT=2223\n")
			asset := filepath.Join(git, "yards", name, "overrides", "agents", "codex", "rules", "repo.rules")
			write(asset, "old yard rule\n")
			options := LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: name, DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, "data")}}
			before, err := Load(options)
			if err != nil {
				t.Fatal(err)
			}
			if before.Environment["YARD_IMAGE"] != "old:image" {
				t.Fatal("fixture did not use Git fallback")
			}
			noOp := func() error { return nil }
			if err := ResetYardConfiguration(configHome, name, noOp, noOp, noOp); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(YardResetMarkerPath(configHome, name))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("marker permissions: %v %v", info, err)
			}
			if name != "default" {
				if _, err := Load(options); !errors.Is(err, ErrUnknownYard) {
					t.Fatalf("reset fallback registered yard: %v", err)
				}
				names, err := YardNames(filepath.Join(home, "shipped"), configHome)
				if err != nil || !reflect.DeepEqual(names, []string{"default"}) {
					t.Fatalf("reset inventory: %v %v", names, err)
				}
			}
			local := filepath.Join(configHome, "yards", name, "config.env")
			write(local, "YARD_IMAGE=new:image\n")
			loaded, err := Load(options)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Environment["YARD_IMAGE"] != "new:image" || loaded.Context.SSHPort != 2223 || loaded.Context.DevUser != "shareddev" || loaded.Environment["AGENT_codex_RULES"] == asset {
				t.Fatalf("reset inherited old yard or lost host/shared layers: %s %d %s", loaded.Environment["YARD_IMAGE"], loaded.Context.SSHPort, loaded.Context.DevUser)
			}
			options.LayerPaths = &LayerPaths{GitYardSettings: map[string]string{name: gitYard}, YardSettings: map[string]string{name: local}, GitSharedSettings: filepath.Join(git, "overrides", "shared", "config.env"), HostSettings: filepath.Join(configHome, "config.env"), GitYardAssets: map[string]string{name: filepath.Dir(filepath.Dir(filepath.Dir(asset)))}}
			prospective, err := Load(options)
			if err != nil || prospective.Context.SSHPort != 2223 || prospective.Environment["AGENT_codex_RULES"] == asset {
				t.Fatalf("prospective resolution: %v", err)
			}
			if _, err := os.Stat(gitYard); err != nil {
				t.Fatal("reset deleted Git source")
			}
		})
	}
}

func TestYardResetKeepsLegacySourceAndAllowsLocalShadow(t *testing.T) {
	root := testkit.TempDir(t)
	configHome := filepath.Join(root, "local")
	configDir := filepath.Join(root, "config")
	private := filepath.Join(root, "private", "yards", "named.env")
	if err := os.MkdirAll(filepath.Dir(private), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, private, []byte("SSH_PORT=3333\n"), 0600)
	noOp := func() error { return nil }
	if err := ResetYardConfiguration(configHome, "named", noOp, noOp, noOp); err != nil {
		t.Fatal(err)
	}
	if _, err := FindYardRegistrationFile(configDir, configHome, "named"); !errors.Is(err, ErrUnknownYard) {
		t.Fatalf("legacy source reactivated: %v", err)
	}
	flat := filepath.Join(configHome, "yards", "named.env")
	if err := os.MkdirAll(filepath.Dir(flat), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, flat, []byte("SSH_PORT=4444\n"), 0600)
	path, err := FindYardRegistrationFile(configDir, configHome, "named")
	if err != nil || path != flat {
		t.Fatalf("new flat registration: %s %v", path, err)
	}
	if _, err := os.Stat(private); err != nil {
		t.Fatal("legacy source removed")
	}
}

func TestYardResetRejectsUnsafeMarkers(t *testing.T) {
	for _, kind := range []string{"malformed", "permissions", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := YardResetMarkerPath(root, "named")
			content := yardResetMarkerContent
			mode := os.FileMode(0600)
			if kind == "malformed" {
				content = "unknown\n"
			}
			if kind == "permissions" {
				mode = 0644
			}
			if kind == "symlink" {
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			} else {
				testkit.WriteFile(t, path, []byte(content), mode)
			}
			if kind == "hardlink" {
				if err := os.Link(path, path+".alias"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := YardFallbackReset(root, "named"); err == nil {
				t.Fatal("unsafe ownership marker accepted")
			}
		})
	}
}

func TestYardResetMarkerModeDoesNotDependOnUmask(t *testing.T) {
	root := testkit.TempDir(t)
	previous := unix.Umask(0o777)
	defer unix.Umask(previous)
	noOp := func() error { return nil }
	if err := ResetYardConfiguration(root, "named", noOp, noOp, noOp); err != nil {
		t.Fatal(err)
	}
	reset, err := YardFallbackReset(root, "named")
	if err != nil || !reset {
		t.Fatalf("exact marker mode did not survive restrictive umask: %v", err)
	}
}
