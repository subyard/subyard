package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestYardRegistryReadsNestedAndLegacyLayouts(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
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
	got, err := YardNames(configDir, configHome)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "legacy", "nested"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("YardNames() = %v, want %v", got, want)
	}
}

func TestYardRegistryListsOnlyLoadableLayouts(t *testing.T) {
	repositoryRoot := t.TempDir()
	configDir := filepath.Join(repositoryRoot, "config")
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

	got, err := YardNames(configDir, configHome)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "duplicate", "installed-flat", "installed-nested", "private-flat"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("YardNames() = %v, want only loadable layouts %v", got, want)
	}
	for _, name := range want[1:] {
		if _, err := FindYardSettingsFile(repositoryRoot, name, configHome); err != nil {
			t.Errorf("listed yard %q is not loadable: %v", name, err)
		}
	}
	if got, err := FindYardSettingsFile(repositoryRoot, "duplicate", configHome); err != nil ||
		got != filepath.Join(configHome, "yards", "duplicate", "config.env") {
		t.Fatalf("duplicate precedence = %q, %v; want installed nested", got, err)
	}
}

func TestYardRegistryIgnoresUnsafeAndSymlinkEntries(t *testing.T) {
	repositoryRoot := t.TempDir()
	configDir := filepath.Join(repositoryRoot, "config")
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

	got, err := YardNames(configDir, configHome)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("YardNames() = %v, want %v", got, want)
	}
}

func TestFindYardSettingsFileRejectsSymlink(t *testing.T) {
	repositoryRoot := t.TempDir()
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

	if _, err := FindYardSettingsFile(repositoryRoot, "demo", configHome); !errors.Is(err, ErrUnknownYard) {
		t.Fatalf("FindYardSettingsFile() error = %v, want unknown yard", err)
	}
}

func TestFindYardRegistrationFileRejectsUnsafeName(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	configHome := filepath.Join(t.TempDir(), "config")
	escaped := filepath.Join(configHome, "outside", "config.env")
	if err := os.MkdirAll(filepath.Dir(escaped), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escaped, []byte("SSH_PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := FindYardRegistrationFile(configDir, configHome, "../outside"); !errors.Is(err, ErrUnknownYard) {
		t.Fatalf("FindYardRegistrationFile() = %q, %v; want unknown yard", got, err)
	}
}

func TestFindYardRegistrationFileRejectsSyntheticDefault(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	configHome := filepath.Join(t.TempDir(), "config")
	registration := filepath.Join(configHome, "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(registration), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registration, []byte("SSH_PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := FindYardRegistrationFile(configDir, configHome, "default"); !errors.Is(err, ErrUnknownYard) {
		t.Fatalf("FindYardRegistrationFile() = %q, %v; want synthetic default", got, err)
	}
}

func TestYardRegistryFallsBackFromUnsafeHigherPrecedenceEntry(t *testing.T) {
	repositoryRoot := t.TempDir()
	configDir := filepath.Join(repositoryRoot, "config")
	configHome := filepath.Join(t.TempDir(), "config")
	installedNested := filepath.Join(configHome, "yards", "demo", "config.env")
	privateFlat := filepath.Join(repositoryRoot, "private", "yards", "demo.env")
	target := filepath.Join(t.TempDir(), "target.env")
	for _, path := range []string{privateFlat, target} {
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

	names, err := YardNames(configDir, configHome)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default", "demo"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("YardNames() = %v, want %v", names, want)
	}
	if got, err := FindYardSettingsFile(repositoryRoot, "demo", configHome); err != nil || got != privateFlat {
		t.Fatalf("FindYardSettingsFile() = %q, %v; want fallback %q", got, err, privateFlat)
	}
}

func TestYardFileCandidatesPreferInstalledNestedLayout(t *testing.T) {
	configDir := "/runtime/config"
	configHome := "/operator/config"
	want := []string{
		"/operator/config/yards/demo/config.env",
		"/runtime/private/yards/demo.env",
		"/operator/config/yards/demo.env",
	}
	if got := YardFileCandidates(configDir, configHome, "demo"); !reflect.DeepEqual(got, want) {
		t.Fatalf("YardFileCandidates() = %v, want %v", got, want)
	}
}
