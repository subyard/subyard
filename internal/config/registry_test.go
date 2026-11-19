package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestHostListenerCollisionsAcrossLocalYards(t *testing.T) {
	yard := func(name, ssh, preview string) Loaded {
		return Loaded{Context: domain.Context{YardName: name}, Environment: map[string]string{"SSH_PORT": ssh, "WEB_PREVIEW_HOST_PORT": preview}}
	}
	a, b := yard("a", "2222", "32222"), yard("b", "2222", "32222")
	err := HostListenerCollisions([]Loaded{b, a})
	if err == nil || !strings.Contains(err.Error(), "yards a SSH_PORT and b SSH_PORT reserve the same host port 2222") ||
		!strings.Contains(err.Error(), "yards a WEB_PREVIEW_HOST_PORT and b WEB_PREVIEW_HOST_PORT") {
		t.Fatalf("collisions=%v", err)
	}
	b.Context.AccessKind = domain.AccessRemote
	if err := HostListenerCollisions([]Loaded{a, b}); err != nil {
		t.Fatalf("remote reservation collided locally: %v", err)
	}
	b.Context.AccessKind = domain.AccessLocal
	b.Environment["SSH_PORT"], b.Environment["WEB_PREVIEW_HOST_PORT"] = "2223", "32223"
	if err := HostListenerCollisions([]Loaded{a, b}); err != nil {
		t.Fatal(err)
	}
	b.Environment["SSH_PORT"] = "02222"
	if err := HostListenerCollisions([]Loaded{a, b}); err == nil || !strings.Contains(err.Error(), "SSH_PORT reserve the same host port 2222") {
		t.Fatalf("equivalent port spelling evaded collision: %v", err)
	}
	collisions := newPortCollisions()
	addHostListenerCollisions(map[string]string{"SSH_PORT": "02222", "WEB_PREVIEW_HOST_PORT": "2222"}, Catalog{}, collisions)
	if len(collisions.refusals) == 0 {
		t.Fatal("equivalent port spelling evaded the within-yard collision")
	}
}

func TestYardRegistryReadsNestedAndLegacyLayouts(t *testing.T) {
	configHome := t.TempDir()
	root := filepath.Join(configHome, "yards")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "nested", "config.env"),
		filepath.Join(root, "legacy.env"),
	} {
		if err := os.WriteFile(path, []byte("SSH_PORT=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := YardNames(configHome)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "legacy", "nested"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("YardNames() = %v, want %v", got, want)
	}
}

func TestYardRegistryIgnoresCheckoutOverlay(t *testing.T) {
	repositoryRoot := t.TempDir()
	configHome := filepath.Join(t.TempDir(), "config")
	for _, path := range []string{
		filepath.Join(repositoryRoot, "private", "yards", "private-flat.env"),
		filepath.Join(repositoryRoot, "private", "yards", "private-nested", "config.env"),
		filepath.Join(repositoryRoot, "private", "yards", "duplicate.env"),
		filepath.Join(configHome, "yards", "installed-flat.env"),
		filepath.Join(configHome, "yards", "installed-nested", "config.env"),
		filepath.Join(configHome, "yards", "duplicate", "config.env"),
		filepath.Join(configHome, "yards", "default.env"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("SSH_PORT=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := YardNames(configHome)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "duplicate", "installed-flat", "installed-nested"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("YardNames() = %v, want only loadable layouts %v", got, want)
	}
	for _, name := range want[1:] {
		if _, err := FindYardRegistrationFile(configHome, name); err != nil {
			t.Errorf("listed yard %q is not loadable: %v", name, err)
		}
	}
	if got, err := FindYardRegistrationFile(configHome, "duplicate"); err != nil ||
		got != filepath.Join(configHome, "yards", "duplicate", "config.env") {
		t.Fatalf("duplicate precedence = %q, %v; want installed nested", got, err)
	}
}

func TestYardRegistryIgnoresUnsafeAndSymlinkEntries(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	yards := filepath.Join(configHome, "yards")
	if err := os.MkdirAll(yards, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.env")
	if err := os.WriteFile(target, []byte("SSH_PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(yards, "linked.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(yards, "bad name.env"), []byte("SSH_PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := YardNames(configHome)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("YardNames() = %v, want %v", got, want)
	}
}

func TestFindYardRegistrationFileRejectsSymlink(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	yardDir := filepath.Join(configHome, "yards", "demo")
	if err := os.MkdirAll(yardDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.env")
	if err := os.WriteFile(target, []byte("SSH_PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(yardDir, "config.env")); err != nil {
		t.Fatal(err)
	}

	if _, err := FindYardRegistrationFile(configHome, "demo"); !errors.Is(err, ErrUnknownYard) {
		t.Fatalf("FindYardRegistrationFile() error = %v, want unknown yard", err)
	}
	if _, err := Load(LoadOptions{RepositoryRoot: "../..", OperatorHome: configHome, YardName: "demo", NativeTargetOnly: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome}}); err == nil {
		t.Fatal("address-only load accepted symlink registration")
	}
}

func TestFindYardRegistrationFileRejectsUnsafeName(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	escaped := filepath.Join(configHome, "outside", "config.env")
	if err := os.MkdirAll(filepath.Dir(escaped), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escaped, []byte("SSH_PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := FindYardRegistrationFile(configHome, "../outside"); !errors.Is(err, ErrUnknownYard) {
		t.Fatalf("FindYardRegistrationFile() = %q, %v; want unknown yard", got, err)
	}
}

func TestFindYardRegistrationFileRejectsSyntheticDefault(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	registration := filepath.Join(configHome, "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(registration), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registration, []byte("SSH_PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := FindYardRegistrationFile(configHome, "default"); !errors.Is(err, ErrUnknownYard) {
		t.Fatalf("FindYardRegistrationFile() = %q, %v; want synthetic default", got, err)
	}
}

func TestYardRegistryFallsBackFromUnsafeHigherPrecedenceEntry(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	installedNested := filepath.Join(configHome, "yards", "demo", "config.env")
	git := filepath.Join(configHome, GitSettingsRelativePath, "yards", "demo", "config.env")
	target := filepath.Join(t.TempDir(), "target.env")
	for _, path := range []string{git, target} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("SSH_PORT=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(installedNested), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, installedNested); err != nil {
		t.Fatal(err)
	}

	names, err := YardNames(configHome)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default", "demo"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("YardNames() = %v, want %v", names, want)
	}
	if got, err := FindYardRegistrationFile(configHome, "demo"); err != nil || got != git {
		t.Fatalf("FindYardRegistrationFile() = %q, %v; want fallback %q", got, err, git)
	}
}

func TestLocalYardSettingsCandidatesPreferNestedLayout(t *testing.T) {
	want := [2]string{
		"/operator/config/yards/demo/config.env",
		"/operator/config/yards/demo.env",
	}
	if got := LocalYardSettingsCandidates("/operator/config/yards", "demo"); got != want {
		t.Fatalf("LocalYardSettingsCandidates() = %v, want %v", got, want)
	}
}
