package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLoadLocalFirstGitFallback(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, "config")
	gitRoot := filepath.Join(configHome, GitSettingsRelativePath)
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0o600)
	}
	gitYard := filepath.Join(gitRoot, "yards", "cached", "config.env")
	localShared := filepath.Join(configHome, "overrides", "shared", "config.env")
	write(gitYard, "YARD_IMAGE=git:image\nCODING_TOOL_INTEGRATIONS=codex\nYARD_INSTANCE_NAME=yard\n")
	write(filepath.Join(configHome, "config.env"), "INCUS_PROJECT=subyard\n")
	write(localShared, "YARD_IMAGE=local:image\nCODING_TOOL_INTEGRATIONS=\"\"\n")
	gitAsset := filepath.Join(gitRoot, "yards", "cached", "overrides", "agents", "codex", "rules", "repo.rules")
	localAsset := filepath.Join(configHome, "overrides", "shared", "agents", "codex", "rules", "repo.rules")
	write(gitAsset, "git\n")
	write(localAsset, "local\n")
	options := LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "cached", DisablePrivate: true,
		Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, "data")}}
	loaded, err := Load(options)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.IncusProject != "subyard-cached" {
		t.Fatalf("host generic values lost named derivations: project=%s instance=%s", loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	}
	if loaded.Context.YardInstanceName != "yard" {
		t.Fatalf("explicit Git yard generic value changed: %s", loaded.Context.YardInstanceName)
	}
	assertEffectiveSetting(t, loaded.Settings["YARD_IMAGE"], "local:image", "shared", "scalar settings", localShared)
	assertEffectiveSetting(t, loaded.Settings["CODING_TOOL_INTEGRATIONS"], "", "shared", "scalar settings", localShared)
	assertEffectiveSetting(t, loaded.Settings["AGENT_codex_RULES"], localAsset, "shared", "file settings", localAsset)
	candidate := options
	candidate.LayerPaths = &LayerPaths{
		SharedSettings: localShared, SharedAssets: filepath.Dir(filepath.Dir(filepath.Dir(localAsset))),
		GitYardSettings:    map[string]string{"cached": gitYard},
		GitYardAssets:      map[string]string{"cached": filepath.Dir(filepath.Dir(filepath.Dir(gitAsset)))},
		ExcludedLocalPaths: map[string]bool{localShared: true, localAsset: true},
	}
	candidateLoaded, err := Load(candidate)
	if err != nil {
		t.Fatal(err)
	}
	assertEffectiveSetting(t, candidateLoaded.Settings["YARD_IMAGE"], "git:image", "yard", "Git scalar settings", gitYard)
	assertEffectiveSetting(t, candidateLoaded.Settings["AGENT_codex_RULES"], gitAsset, "yard", "Git file settings", gitAsset)
	if err := os.Remove(localShared); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(localAsset); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(options)
	if err != nil {
		t.Fatal(err)
	}
	assertEffectiveSetting(t, loaded.Settings["YARD_IMAGE"], "git:image", "yard", "Git scalar settings", gitYard)
	assertEffectiveSetting(t, loaded.Settings["AGENT_codex_RULES"], gitAsset, "yard", "Git file settings", gitAsset)
	names, err := YardNames(filepath.Join(home, "shipped"), configHome)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"default", "cached"}) {
		t.Fatalf("yard names: %v", names)
	}
	localYard := filepath.Join(configHome, "yards", "cached", "config.env")
	write(localYard, "YARD_IMAGE=yard:image\n")
	registration, err := FindYardRegistrationFile(filepath.Join(root, "config"), configHome, "cached")
	if err != nil || registration != localYard {
		t.Fatalf("registration = %q, %v", registration, err)
	}
}

func TestLoadGitTemplateResolvedOnceWithLocalOverride(t *testing.T) {
	root := testkit.TempDir(t)
	home := filepath.Join(root, "home")
	configHome := filepath.Join(home, "config")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0o600)
	}
	write(filepath.Join(root, "config", "host.env"), "HOST_BASE="+filepath.Join(home, "host")+"\nSTORAGE_PATH="+filepath.Join(home, "storage")+"\nRESTRICTED_DISK_PATHS="+filepath.Join(home, "host")+"\n")
	write(filepath.Join(root, "config", "subyard.env"), "DEV_UID=1000\nDEV_USER=dev\nSSH_PORT=2222\nYARD_KIND=container\nSHIFT_MODE=shift\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\n")
	write(filepath.Join(root, "config", "yards", "profiles", "first.env"), "YARD_IMAGE=first:image\nSSH_PORT=3001\n")
	write(filepath.Join(root, "config", "yards", "profiles", "second.env"), "YARD_IMAGE=second:image\nSSH_PORT=3002\n")
	write(filepath.Join(configHome, GitSettingsRelativePath, "yards", "named", "config.env"), "YARD_TEMPLATE=first\nYARD_IMAGE=git:image\n")
	localYard := filepath.Join(configHome, "yards", "named", "config.env")
	write(localYard, "YARD_TEMPLATE=second\n")
	loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "named", DisablePrivate: true,
		Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, "data")}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["YARD_IMAGE"] != "git:image" || loaded.Context.SSHPort != 3002 {
		t.Fatalf("template resolution: image=%s port=%d", loaded.Environment["YARD_IMAGE"], loaded.Context.SSHPort)
	}
}
