package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLoadNamedContext(t *testing.T) {
	root := testkit.TempDir(t)
	operatorHome := filepath.Join(root, "home")
	configHome := filepath.Join(root, "config-home")
	shipped := filepath.Join(root, "config")
	yardDir := filepath.Join(configHome, "yards")
	for _, directory := range []string{operatorHome, shipped, yardDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(shipped, "incus.project.env"), `: "${INCUS_PROJECT:=subyard}"
: "${RESTRICTED_DISK_PATHS:=/srv/subyard}"`)
	writeFixture(t, filepath.Join(shipped, "subyard.env"), `: "${YARD_INSTANCE_NAME:=yard}"
: "${YARD_KIND:=container}"
: "${SHIFT_MODE:=shift}"
: "${FORWARD_SSH_AGENT:=0}"
: "${DEV_SUDO:=no}"
: "${DEV_UID:=1000}"
: "${SSH_HOST:=yard}"
: "${SSH_PORT:=2222}"`)
	writeFixture(t, filepath.Join(shipped, "host.env"), `: "${SUBYARD_CONFIG_HOME:=$SUBYARD_OPERATOR_HOME/.config/subyard}"
: "${SUBYARD_HOME:=$SUBYARD_OPERATOR_HOME/.subyard}"
: "${STORAGE_PATH:=$SUBYARD_HOME/incus/storage}"
: "${HOST_BASE:=${RESTRICTED_DISK_PATHS:-/srv/subyard}}"`)
	writeFixture(t, filepath.Join(shipped, "yards", "profiles", "test-vms.env"), "NESTED_E2E_VMS=1\nE2E_VM_CPU=2\nFORWARD_SSH_AGENT=0\n")
	writeFixture(t, filepath.Join(yardDir, "named.env"), "YARD_TEMPLATE=test-vms\nSSH_PORT=3333\nINSTANCE_NAME=fixture-yard\nE2E_VM_CPU=1\nE2E_DISK_BUDGET=3GiB\nE2E_CACHE_BUDGET=3GiB\nE2E_DISK_RESERVE=3GiB\nE2E_MEMORY_RESERVE=3GiB\nE2E_VM_OVERHEAD=3GiB\nHOST_BASE="+root+"/host/../host\nRESTRICTED_DISK_PATHS="+root+"/host\n")

	loaded, err := Load(LoadOptions{
		RepositoryRoot: root,
		OperatorHome:   operatorHome,
		YardName:       "named",
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": operatorHome,
			"SUBYARD_CONFIG_HOME":   configHome,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := loaded.Context
	if ctx.YardInstanceName != "fixture-yard" || ctx.IncusProject != "subyard-named" || ctx.SSHPort != 3333 {
		t.Fatalf("named context mismatch: %#v", ctx)
	}
	if !ctx.NestedE2EVMs || ctx.ForwardSSHAgent {
		t.Fatalf("public yard template was not applied: %#v", ctx)
	}
	if !ctx.DevSudo {
		t.Fatal("retained shipped DEV_SUDO=no replaced the catalog default")
	}
	assertEffectiveSetting(t, loaded.Settings["DEV_SUDO"], "prompt", "default", "shipped defaults", "built-in resolver default")
	if loaded.Environment["E2E_VM_CPU"] != "1" {
		t.Fatalf("yard settings did not override the selected template: %#v", loaded.Environment)
	}
	for _, name := range []string{"E2E_DISK_BUDGET", "E2E_CACHE_BUDGET", "E2E_DISK_RESERVE", "E2E_MEMORY_RESERVE", "E2E_VM_OVERHEAD"} {
		if loaded.Environment[name] != "3GiB" {
			t.Fatalf("budget override lost: %s", name)
		}
		if err := ValidateSetting(ScopeYard, name, "3GiB", false); err != nil {
			t.Fatal(err)
		}
	}
	if ctx.Paths.HostBase != filepath.Join(root, "host") {
		t.Fatalf("host base was not normalized: %s", ctx.Paths.HostBase)
	}
	hostBaseTrace := loaded.Settings["HOST_BASE"]
	assertEffectiveSetting(
		t, hostBaseTrace, filepath.Join(root, "host"), "yard", "scalar settings",
		filepath.Join(yardDir, "named.env"),
	)
	if !strings.Contains(
		effectiveSettingDetail(hostBaseTrace), "normalized path",
	) {
		t.Fatalf("normalized setting provenance omitted its transformation: %#v", hostBaseTrace)
	}
	if ctx.AccessKind != domain.AccessLocal {
		t.Fatalf("unexpected yard type: %s", ctx.AccessKind)
	}
}

func TestLoadRejectsBroadDataHome(t *testing.T) {
	operatorHome := testkit.TempDir(t)
	_, err := Load(LoadOptions{
		RepositoryRoot: filepath.Clean(filepath.Join("..", "..")),
		OperatorHome:   operatorHome,
		Environment: map[string]string{
			"HOME":                  operatorHome,
			"SUBYARD_OPERATOR_HOME": operatorHome,
			"SUBYARD_CONFIG_HOME":   filepath.Join(operatorHome, ".config", "subyard"),
			"SUBYARD_HOME":          "/",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "data home is too broad") {
		t.Fatalf("Load accepted broad data home: %v", err)
	}
}

func TestLoadExactYardSettingsFile(t *testing.T) {
	root := testkit.TempDir(t)
	operatorHome := filepath.Join(root, "home")
	configHome := filepath.Join(root, "config-home")
	shipped := filepath.Join(root, "config")
	preset := filepath.Join(shipped, "profiles", "sample-service", "yard.env")
	for _, directory := range []string{operatorHome, shipped, configHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(shipped, "incus.project.env"), `: "${INCUS_PROJECT:=subyard}"
: "${RESTRICTED_DISK_PATHS:=/srv/subyard}"`)
	writeFixture(t, filepath.Join(shipped, "subyard.env"), `: "${YARD_INSTANCE_NAME:=yard}"
: "${YARD_KIND:=container}"
: "${SHIFT_MODE:=shift}"
: "${FORWARD_SSH_AGENT:=0}"
DEV_SUDO=no
: "${DEV_UID:=1000}"
: "${SSH_HOST:=yard}"
: "${SSH_PORT:=2222}"`)
	writeFixture(t, filepath.Join(shipped, "host.env"), `: "${SUBYARD_CONFIG_HOME:=$SUBYARD_OPERATOR_HOME/.config/subyard}"
: "${SUBYARD_HOME:=$SUBYARD_OPERATOR_HOME/.subyard}"
: "${STORAGE_PATH:=$SUBYARD_HOME/incus/storage}"
: "${HOST_BASE:=${RESTRICTED_DISK_PATHS:-/srv/subyard}}"`)
	writeFixture(t, filepath.Join(shipped, "agents.env"), "AGENT_codex_COMMAND=codex\n")
	writeFixture(t, preset, "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=3333\n")

	loaded, err := Load(LoadOptions{
		RepositoryRoot:   root,
		OperatorHome:     operatorHome,
		YardName:         "custom-name",
		YardSettingsFile: preset,
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": operatorHome,
			"SUBYARD_CONFIG_HOME":   configHome,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.YardName != "custom-name" || loaded.Context.SSHPort != 3333 ||
		loaded.Environment["ENVIRONMENT_PROFILES"] != "sample-service" || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
		t.Fatalf("exact yard settings were not loaded: %#v %#v", loaded.Context, loaded.Environment)
	}
	if !loaded.Context.DevSudo {
		t.Fatal("literal DEV_SUDO=no in shipped settings replaced the catalog default")
	}
	assertEffectiveSetting(t, loaded.Settings["SSH_PORT"], "3333", "yard", "scalar settings", preset)
}

func TestRetiredE2EVMTemplateReportsMigrationAndTeardown(t *testing.T) {
	root := testkit.TempDir(t)
	configDir := filepath.Join(root, "config")
	yardFile := filepath.Join(root, "yards", "e2e-yard.env")
	writeFixture(t, yardFile, "YARD_TEMPLATE=e2e-vms\nSSH_PORT=3333\n")
	// Even if a stale file is present, the retired name must never act as an alias.
	writeFixture(t, filepath.Join(configDir, "yards", "profiles", "e2e-vms.env"), "NESTED_E2E_VMS=1\n")

	err := applyYardTemplateTracked(configDir, "e2e-yard", "e2e-vms", yardFile, environment{}, nil)
	if err == nil {
		t.Fatal("retired e2e-vms template was accepted")
	}
	diagnostic := err.Error()
	for _, expected := range []string{
		yardFile,
		"YARD_TEMPLATE=test-vms",
		"yard -Y e2e-yard check --host",
		"yard -Y e2e-yard status",
		"yard -Y e2e-yard test-vms status",
		"yard -Y e2e-yard teardown",
	} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("retired-template diagnostic omitted %q: %s", expected, diagnostic)
		}
	}
}

func TestLoadHostSettingsAndMigratedPrivateAssets(t *testing.T) {
	root := testkit.TempDir(t)
	operatorHome := filepath.Join(root, "home")
	configHome := filepath.Join(operatorHome, ".config", "subyard")
	shipped := filepath.Join(root, "config")
	hostAgents := filepath.Join(configHome, "overrides", "host", "agents")
	for _, directory := range []string{
		operatorHome, shipped, filepath.Join(configHome, "yards", "named"),
		filepath.Join(hostAgents, "codex"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(shipped, "incus.project.env"), `: "${INCUS_PROJECT:=subyard}"
: "${RESTRICTED_DISK_PATHS:=/srv/subyard}"`)
	writeFixture(t, filepath.Join(shipped, "subyard.env"), `: "${YARD_INSTANCE_NAME:=yard}"
: "${YARD_KIND:=container}"
: "${SHIFT_MODE:=shift}"
: "${FORWARD_SSH_AGENT:=0}"
: "${DEV_SUDO:=no}"
: "${DEV_UID:=1000}"
: "${SSH_HOST:=yard}"
: "${SSH_PORT:=2222}"`)
	writeFixture(t, filepath.Join(shipped, "host.env"), `: "${SUBYARD_CONFIG_HOME:=$SUBYARD_OPERATOR_HOME/.config/subyard}"
: "${SUBYARD_HOME:=$SUBYARD_OPERATOR_HOME/.subyard}"
: "${STORAGE_PATH:=$SUBYARD_HOME/incus/storage}"
: "${HOST_BASE:=${RESTRICTED_DISK_PATHS:-/srv/subyard}}"`)
	writeFixture(t, filepath.Join(shipped, "agents.env"),
		`AGENT_codex_RULES="$SUBYARD_CONFIG_DIR/agents/codex/rules/repo.rules"`)
	writeFixture(t, filepath.Join(configHome, "config.env"),
		"DEV_SUDO=yes\nAGENT_codex_RULES="+filepath.Join(root, "private/agents/codex/repo.rules")+"\n")
	writeFixture(t, filepath.Join(hostAgents, "codex", "repo.rules"), "fixture\n")
	writeFixture(t, filepath.Join(configHome, "yards", "named", "config.env"), "SSH_PORT=3333\n")

	loaded, err := Load(LoadOptions{
		RepositoryRoot: root,
		OperatorHome:   operatorHome,
		YardName:       "named",
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": operatorHome,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Context.DevSudo {
		t.Fatal("host-wide scalar settings were not loaded")
	}
	wantRules := filepath.Join(hostAgents, "codex", "repo.rules")
	if loaded.Environment["AGENT_codex_RULES"] != wantRules {
		t.Fatalf("migrated private asset path = %q, want %q",
			loaded.Environment["AGENT_codex_RULES"], wantRules)
	}
	file, err := os.Open(loaded.Environment["AGENT_codex_RULES"])
	if err != nil {
		t.Fatalf("production agent asset path is not openable: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(loaded.Environment["AGENT_codex_RULES"]); err != nil ||
		!info.Mode().IsRegular() {
		t.Fatalf("production agent asset path is not a regular file: %v", err)
	}
	if loaded.Environment["SUBYARD_CONFIG_HOME"] != configHome ||
		loaded.Environment["SUBYARD_CONFIG_HOST_DIR"] != filepath.Join(configHome, "overrides", "host") ||
		loaded.Environment["SUBYARD_CONFIG_GENERATED_DIR"] != filepath.Join(configHome, "generated") ||
		loaded.Environment["SUBYARD_KEYS_CONSUMER_ROOT"] != filepath.Join(configHome, "generated") {
		t.Fatalf("persistent configuration root is not canonical: %#v", loaded.Environment)
	}
	if loaded.Environment["SUBYARD_CONFIG_DIR"] != shipped {
		t.Fatalf("shipped-defaults root leaked from migration input: %q",
			loaded.Environment["SUBYARD_CONFIG_DIR"])
	}
}

func TestGitApprovalDefaultAndExplicitPromptScopes(t *testing.T) {
	const setting = "CODING_TOOL_GIT_APPROVAL"
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, scope := range []string{"default", "shared", "host", "yard", "command"} {
		t.Run(scope, func(t *testing.T) {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, ".config", "subyard")
			options := LoadOptions{
				RepositoryRoot: root, OperatorHome: home, YardName: "named",
				Environment: map[string]string{"HOME": home, "SUBYARD_CONFIG_HOME": configHome},
			}
			yardConfig := filepath.Join(configHome, "yards", "named", "config.env")
			writeFixture(t, yardConfig, "")
			want := "prompt"
			switch scope {
			case "default":
				want = "off"
			case "shared":
				writeFixture(t, filepath.Join(configHome, "overrides", "shared", "config.env"), setting+"=prompt\n")
			case "host":
				writeFixture(t, filepath.Join(configHome, "config.env"), setting+"=prompt\n")
			case "yard":
				writeFixture(t, yardConfig, setting+"=prompt\n")
			case "command":
				options.Environment[setting] = "prompt"
			}
			loaded, err := Load(options)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Environment[setting]; got != want {
				t.Fatalf("%s = %q, want %q", setting, got, want)
			}
		})
	}
}

func TestDevSudoDefaultAndScopePrecedence(t *testing.T) {
	definition, ok := LookupSetting("DEV_SUDO")
	if !ok || !definition.HasDefault || definition.Default != "prompt" {
		t.Fatalf("DEV_SUDO catalog default = %#v", definition)
	}
	for _, test := range []struct {
		name, gitHost, gitYard, shared, host, yard, command string
		want, scope, role                                   string
	}{
		{name: "shipped default", want: "prompt", scope: "default", role: "shipped defaults"},
		{name: "host keeps passwordless sudo without prompts", host: "yes", want: "yes", scope: "host", role: "scalar settings"},
		{name: "Git host turns off", gitHost: "no", want: "no", scope: "host", role: "Git scalar settings"},
		{name: "Git yard prompts", gitYard: "prompt", want: "prompt", scope: "yard", role: "Git scalar settings"},
		{name: "shared turns off", shared: "no", want: "no", scope: "shared", role: "scalar settings"},
		{name: "shared prompts", shared: "prompt", want: "prompt", scope: "shared", role: "scalar settings"},
		{name: "host overrides shared", shared: "yes", host: "no", want: "no", scope: "host", role: "scalar settings"},
		{name: "yard overrides shared", shared: "no", yard: "yes", want: "yes", scope: "yard", role: "scalar settings"},
		{name: "yard turns off", host: "prompt", yard: "no", want: "no", scope: "yard", role: "scalar settings"},
		{name: "command overrides yard", shared: "no", yard: "yes", command: "no", want: "no", scope: "command", role: "command override"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, "config")
			for path, value := range map[string]string{
				GitSettingsRelativePath + "/config.env":               test.gitHost,
				GitSettingsRelativePath + "/yards/default/config.env": test.gitYard,
				"overrides/shared/config.env":                         test.shared,
				"config.env":                                          test.host,
				"yards/default/config.env":                            test.yard,
			} {
				if value != "" {
					writeFixture(t, filepath.Join(configHome, path), "DEV_SUDO="+value+"\n")
				}
			}
			environment := map[string]string{"SUBYARD_CONFIG_HOME": configHome}
			if test.command != "" {
				environment["DEV_SUDO"] = test.command
			}
			loaded, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Context.DevSudo != (test.want != "no") {
				t.Fatalf("context DevSudo = %v, want DEV_SUDO=%s", loaded.Context.DevSudo, test.want)
			}
			assertEffectiveSetting(t, loaded.Settings["DEV_SUDO"], test.want, test.scope, test.role, "")
		})
	}
}

// 0 and 1 are deprecated aliases: the loader reads them as no and yes and reports
// each assignment with its source line and replacement.
func TestDevSudoReadsDeprecatedAliases(t *testing.T) {
	for _, test := range []struct{ value, want, scope string }{
		{"0", "no", "host"}, {"1", "yes", "host"}, {"0", "no", "yard"}, {"1", "yes", "yard"}, {"0", "no", "command"}, {"1", "yes", "command"},
	} {
		home := testkit.TempDir(t)
		configHome := filepath.Join(home, "config")
		environment := map[string]string{"SUBYARD_CONFIG_HOME": configHome}
		path := ""
		switch test.scope {
		case "host":
			path = filepath.Join(configHome, "config.env")
		case "yard":
			path = filepath.Join(configHome, "yards", "default", "config.env")
		default:
			environment["DEV_SUDO"] = test.value
		}
		if path != "" {
			writeFixture(t, path, "# fixture\nDEV_SUDO="+test.value+"\n")
		}
		loaded, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, Environment: environment})
		if err != nil {
			t.Fatalf("%s DEV_SUDO=%s: %v", test.scope, test.value, err)
		}
		if loaded.Environment["DEV_SUDO"] != test.want || loaded.Settings["DEV_SUDO"].EffectiveValue != test.want ||
			loaded.Context.DevSudo != (test.want == "yes") {
			t.Fatalf("%s DEV_SUDO=%s: environment %q, effective %q, grant %v", test.scope, test.value,
				loaded.Environment["DEV_SUDO"], loaded.Settings["DEV_SUDO"].EffectiveValue, loaded.Context.DevSudo)
		}
		deprecated := loaded.DeprecatedAssignments()
		if len(deprecated) != 1 || deprecated[0].Name != "DEV_SUDO" || deprecated[0].Value != test.value ||
			deprecated[0].Replacement != test.want || deprecated[0].Scope != test.scope {
			t.Fatalf("%s DEV_SUDO=%s: deprecated %+v", test.scope, test.value, deprecated)
		}
		note := deprecated[0].String()
		switch test.scope {
		case "command":
			if note != "DEV_SUDO="+test.value+" in the command environment is deprecated and read as "+test.want+"; use DEV_SUDO="+test.want {
				t.Fatalf("command note: %s", note)
			}
		default:
			repair := "yard -Y default config set DEV_SUDO " + test.want + " --scope " + test.scope + " --local"
			if deprecated[0].Path != path || deprecated[0].Line != 2 || deprecated[0].Repair() != repair ||
				note != "DEV_SUDO="+test.value+" in "+path+":2 is deprecated and read as "+test.want+"; "+repair {
				t.Fatalf("%s note: %s", test.scope, note)
			}
		}
	}
}

// A file the config commands do not write gets the line to edit as its repair.
func TestDeprecatedAliasInUnauthoredFileNamesTheLine(t *testing.T) {
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, "config")
	path := filepath.Join(configHome, "yards", "default.env")
	writeFixture(t, path, "DEV_SUDO=0\n")
	loaded, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, YardName: "default", Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome}})
	if err != nil {
		t.Fatal(err)
	}
	deprecated := loaded.DeprecatedAssignments()
	if len(deprecated) != 1 || deprecated[0].Path != path {
		t.Fatalf("deprecated %+v", deprecated)
	}
	deprecated[0].authored = false
	if deprecated[0].Repair() != "replace line 1 of "+path+" with DEV_SUDO=no" {
		t.Fatalf("repair %q", deprecated[0].Repair())
	}
}

// Only 0 and 1 are aliases; every other value is still one refusal naming the accepted values.
func TestDevSudoRefusesUnsupportedValues(t *testing.T) {
	for _, value := range []string{"true", "false", "on", "off", "2", "Yes"} {
		for _, scope := range []string{"host", "command"} {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, "config")
			environment := map[string]string{"SUBYARD_CONFIG_HOME": configHome}
			if scope == "host" {
				writeFixture(t, filepath.Join(configHome, "config.env"), "DEV_SUDO="+value+"\n")
			} else {
				environment["DEV_SUDO"] = value
			}
			_, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, Environment: environment})
			if err == nil || strings.Count(err.Error(), "DEV_SUDO") != 1 ||
				!strings.Contains(err.Error(), "invalid value; must be one of yes, no, prompt") {
				t.Fatalf("%s DEV_SUDO=%s: want one refusal naming the accepted values: %v", scope, value, err)
			}
		}
	}
}

// A yard that reads 0 has the same init inputs and approval gates as one that
// reads no, so replacing the alias awaits no init.
func TestDevSudoAliasSnapshotsEqualCanonical(t *testing.T) {
	catalog, err := LoadCatalog(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for alias, canonical := range map[string]string{"0": "no", "1": "yes"} {
		home := testkit.TempDir(t)
		configHome := filepath.Join(home, "config")
		load := func(value string) Loaded {
			writeFixture(t, filepath.Join(configHome, "yards", "default", "config.env"), "DEV_SUDO="+value+"\n")
			loaded, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome}})
			if err != nil {
				t.Fatal(err)
			}
			return loaded
		}
		old, current := load(alias), load(canonical)
		oldInputs, currentInputs := catalog.IntegrationInitSettings(old.Environment), catalog.IntegrationInitSettings(current.Environment)
		if !maps.Equal(oldInputs, currentInputs) || oldInputs["DEV_SUDO"] != canonical {
			t.Fatalf("DEV_SUDO=%s init inputs differ from %s: %v %v", alias, canonical, oldInputs, currentInputs)
		}
		if SudoApprovalPrompt(old.Environment["DEV_SUDO"]) != SudoApprovalPrompt(current.Environment["DEV_SUDO"]) ||
			GitApprovalOff(old.Environment["CODING_TOOL_GIT_APPROVAL"]) != GitApprovalOff(current.Environment["CODING_TOOL_GIT_APPROVAL"]) {
			t.Fatalf("DEV_SUDO=%s approval gates differ from %s", alias, canonical)
		}
	}
	if got := CanonicalSettingValue("DEV_SUDO", "prompt"); got != "prompt" {
		t.Fatalf("canonical value changed: %q", got)
	}
	if got := CanonicalSettingValue("FORWARD_SSH_AGENT", "0"); got != "0" {
		t.Fatalf("alias applied to another setting: %q", got)
	}
}

func TestSettingsPrecedenceAndYardFileOverride(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	temp := testkit.TempDir(t)
	home := filepath.Join(temp, "home")
	configHome := filepath.Join(home, ".config", "subyard")
	shared := filepath.Join(configHome, "overrides", "shared", "agents", "codex", "rules", "repo.rules")
	host := filepath.Join(configHome, "overrides", "host", "agents", "codex", "rules", "repo.rules")
	yard := filepath.Join(configHome, "yards", "named", "overrides", "agents", "codex", "rules", "repo.rules")
	external := filepath.Join(temp, "external.rules")
	for path, value := range map[string]string{
		shared: "shared\n", host: "host\n", yard: "yard\n", external: "external\n",
		filepath.Join(configHome, "overrides", "shared", "config.env"): "YARD_IMAGE=shared:image\nDEV_SUDO=no\n",
		filepath.Join(configHome, "config.env"): "DEV_SUDO=yes\nBASE_IMAGE=host:image\n" +
			"AGENT_codex_RULES=" + external + "\n",
		filepath.Join(configHome, "yards", "named", "config.env"): "DEV_SUDO=no\nSSH_PORT=3333\nBASE_IMAGE=yard:image\n",
	} {
		writeFixture(t, path, value)
	}
	base := map[string]string{
		"HOME": home, "SUBYARD_OPERATOR_HOME": home,
		"SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, ".subyard"),
	}
	defaultLoaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: home, Environment: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if defaultLoaded.Environment["AGENT_codex_RULES"] != host {
		t.Fatalf("host asset did not override shared/config.env: %s",
			defaultLoaded.Environment["AGENT_codex_RULES"])
	}
	if !defaultLoaded.Context.DevSudo {
		t.Fatal("host DEV_SUDO=yes did not override local shared DEV_SUDO=no")
	}
	defaultAsset := filepath.Join(configHome, "yards", "default", "overrides", "agents", "codex", "rules", "repo.rules")
	writeFixture(t, defaultAsset, "default yard rules\n")
	defaultLoaded, err = Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: home, Environment: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if defaultLoaded.Environment["AGENT_codex_RULES"] != defaultAsset || defaultLoaded.Environment["SUBYARD_CONFIG_YARD_DIR"] != filepath.Join(configHome, "yards", "default") {
		t.Fatalf("default yard file layer missing: %q", defaultLoaded.Environment["AGENT_codex_RULES"])
	}
	assertEffectiveSetting(t, defaultLoaded.Settings["AGENT_codex_RULES"], defaultAsset, "yard", "file settings", defaultAsset)
	namedLoaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: home, YardName: "named",
		Environment: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if namedLoaded.Environment["AGENT_codex_RULES"] != yard || namedLoaded.Context.DevSudo {
		t.Fatalf("yard layer did not win: asset=%s sudo=%v",
			namedLoaded.Environment["AGENT_codex_RULES"], namedLoaded.Context.DevSudo)
	}
	withEnvironment := cloneStringMap(base)
	withEnvironment["AGENT_codex_RULES"] = external
	withEnvironment["DEV_SUDO"] = "yes"
	withEnvironment["YARD_IMAGE"] = "command:image"
	withEnvironment["SECRET_TOKEN"] = "must-not-be-traced"
	environmentLoaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: home, YardName: "named",
		Environment: withEnvironment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if environmentLoaded.Environment["AGENT_codex_RULES"] != external ||
		!environmentLoaded.Context.DevSudo {
		t.Fatalf("command environment did not win: asset=%s sudo=%v",
			environmentLoaded.Environment["AGENT_codex_RULES"], environmentLoaded.Context.DevSudo)
	}
	if _, ok := environmentLoaded.Settings["SECRET_TOKEN"]; ok {
		t.Fatal("unknown environment variable was exposed as a Subyard setting")
	}
	assertEffectiveSetting(
		t, environmentLoaded.Settings["DEV_SUDO"], "yes", "command", "command override", "environment",
	)
	assertEffectiveSetting(
		t, environmentLoaded.Settings["YARD_IMAGE"], "command:image",
		"command", "command override", "environment",
	)
	assertEffectiveSetting(
		t, namedLoaded.Settings["AGENT_codex_RULES"], yard, "yard", "file settings", yard,
	)
	assertEffectiveSetting(
		t, namedLoaded.Settings["YARD_INSTANCE_NAME"], "yard-named", "yard", "derived", "",
	)
	if !settingTraceContains(
		environmentLoaded.Settings["DEV_SUDO"], "shared", "scalar settings",
		filepath.Join(configHome, "overrides", "shared", "config.env"), 2, "overridden",
	) || !settingTraceContains(
		environmentLoaded.Settings["DEV_SUDO"], "host", "scalar settings",
		filepath.Join(configHome, "config.env"), 1, "overridden",
	) || !settingTraceContains(
		environmentLoaded.Settings["DEV_SUDO"], "yard", "scalar settings",
		filepath.Join(configHome, "yards", "named", "config.env"), 1, "overridden",
	) {
		t.Fatalf("scalar provenance omitted shared, host or yard assignments: %#v",
			environmentLoaded.Settings["DEV_SUDO"])
	}
	baseImage := environmentLoaded.Settings["YARD_IMAGE"]
	for _, expected := range []struct {
		scope, path string
	}{
		{"shared", filepath.Join(configHome, "overrides", "shared", "config.env")},
		{"host", filepath.Join(configHome, "config.env")},
		{"yard", filepath.Join(configHome, "yards", "named", "config.env")},
		{"command", "environment"},
	} {
		found := false
		for _, resolution := range baseImage.Resolutions {
			if resolution.Scope == expected.scope && resolution.Path == expected.path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("YARD_IMAGE trace omitted %s layer %s: %#v",
				expected.scope, expected.path, baseImage)
		}
	}
}

func TestResolveIntegrationSelection(t *testing.T) {
	tests := []struct {
		name    string
		agents  string
		values  environment
		want    string
		wantErr string
	}{
		{
			name:   "dependency before dependent",
			agents: "relay",
			values: environment{"AGENT_relay_DEPENDS": "codex", "AGENT_codex_COMMAND": "codex"},
			want:   "codex relay",
		},
		{
			name:   "stable independent order and transitive closure",
			agents: "pi relay claude",
			values: environment{
				"AGENT_relay_DEPENDS": "bridge", "AGENT_bridge_COMMAND": "bridge",
				"AGENT_bridge_DEPENDS": "codex", "AGENT_codex_COMMAND": "codex",
				"AGENT_pi_COMMAND": "pi", "AGENT_claude_COMMAND": "claude",
			},
			want: "pi codex bridge relay claude",
		},
		{
			name:   "agent id containing underscore",
			agents: "relay",
			values: environment{
				"AGENT_relay_DEPENDS": "codex_bridge", "AGENT_codex_bridge_COMMAND": "bridge",
			},
			want: "codex_bridge relay",
		},
		{name: "unknown root", agents: "missing", wantErr: "unknown integration"},
		{name: "canonical none is unknown", agents: "none", wantErr: "unknown integration"},
		{name: "duplicate input", agents: "relay relay", wantErr: "duplicate agent"},
		{
			name: "unknown dependency", agents: "relay",
			values: environment{"AGENT_relay_DEPENDS": "missing"}, wantErr: "unknown dependency",
		},
		{
			name: "self dependency", agents: "relay",
			values: environment{"AGENT_relay_DEPENDS": "relay"}, wantErr: "dependency cycle",
		},
		{
			name: "dependency cycle", agents: "relay",
			values: environment{
				"AGENT_relay_DEPENDS": "codex", "AGENT_codex_DEPENDS": "relay",
			},
			wantErr: "dependency cycle",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := cloneEnvironment(test.values)
			selection, err := ResolveIntegrationSelection(values, strings.Fields(test.agents))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("resolve error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if got := strings.Join(selection.Effective, " "); err != nil || got != test.want {
				t.Fatalf("resolve = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestPersistentSettingsValidationFailsClosed(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, test := range []struct {
		name, relative, content, want string
	}{
		{"unknown secret-like", "config.env", "FUTURE_SETTING=password=hidden\n", "secret material"},
		{"unknown name too long", "config.env", strings.Repeat("F", 129) + "=1\n", "name is too long"},
		{"wrong shared scope", "overrides/shared/config.env", "DEV_UID=1000\n", "not allowed in shared scope"},
		{"wrong type", "config.env", "SSH_PORT=70000\n", "must be in range"},
		{"bad mount", "config.env", "HOST_MOUNTS=../escape:/mnt/cache:rw:0755\n", "invalid mount"},
		{"secret-like", "config.env", "YARD_IMAGE=password=hidden\n", "secret material"},
	} {
		t.Run(test.name, func(t *testing.T) {
			temp := testkit.TempDir(t)
			home := filepath.Join(temp, "home")
			configHome := filepath.Join(home, ".config", "subyard")
			writeFixture(t, filepath.Join(configHome, test.relative), test.content)
			_, err := Load(LoadOptions{
				RepositoryRoot: root, OperatorHome: home,
				Environment: map[string]string{
					"SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome,
					"SUBYARD_HOME": filepath.Join(home, ".subyard"),
				},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("wanted %q rejection, got %v", test.want, err)
			}
		})
	}
}

func TestProductionLoaderRejectsPendingConfigurationTransaction(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	temporary := testkit.TempDir(t)
	home := filepath.Join(temporary, "home")
	configHome := filepath.Join(home, ".config", "subyard")
	writeFixture(t, filepath.Join(configHome, ".sync", "transaction.json"), "{}\n")
	options := LoadOptions{
		RepositoryRoot: root, OperatorHome: home,
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome,
			"SUBYARD_HOME": filepath.Join(home, ".subyard"),
		},
	}
	if _, err := Load(options); err == nil ||
		!strings.Contains(err.Error(), "interrupted configuration transaction") {
		t.Fatalf("production resolver accepted a pending transaction: %v", err)
	}
}

func assertEffectiveSetting(
	t *testing.T,
	trace SettingTrace,
	value, scope, role, path string,
) {
	t.Helper()
	if trace.EffectiveValue != value {
		t.Fatalf("%s effective value = %q, want %q: %#v", trace.Name, trace.EffectiveValue, value, trace)
	}
	for _, resolution := range trace.Resolutions {
		if resolution.Status == "effective" && resolution.Scope == scope &&
			resolution.Role == role && (path == "" || resolution.Path == path) {
			return
		}
	}
	t.Fatalf("%s has no effective %s/%s source %q: %#v", trace.Name, scope, role, path, trace)
}

func settingTraceContains(
	trace SettingTrace,
	scope, role, path string,
	line int,
	status string,
) bool {
	for _, resolution := range trace.Resolutions {
		if resolution.Scope == scope && resolution.Role == role && resolution.Path == path &&
			resolution.Line == line && resolution.Status == status {
			return true
		}
	}
	return false
}

func effectiveSettingDetail(trace SettingTrace) string {
	for _, resolution := range trace.Resolutions {
		if resolution.Status == "effective" {
			return resolution.Detail
		}
	}
	return ""
}

func cloneStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}

func TestResolveE2EVMCPU(t *testing.T) {
	for _, test := range []struct {
		value    string
		hostCPUs int
		want     string
	}{
		{value: "auto", hostCPUs: 1, want: "1"},
		{value: "auto", hostCPUs: 2, want: "1"},
		{value: "auto", hostCPUs: 3, want: "2"},
		{value: "auto", hostCPUs: 4, want: "2"},
		{value: "auto", hostCPUs: 5, want: "3"},
		{value: "auto", hostCPUs: 6, want: "4"},
		{value: "auto", hostCPUs: 64, want: "4"},
		{value: "7", hostCPUs: 3, want: "7"},
	} {
		got, err := resolveE2EVMCPU(test.value, test.hostCPUs)
		if err != nil || got != test.want {
			t.Fatalf("resolveE2EVMCPU(%q, %d) = %q, %v; want %q",
				test.value, test.hostCPUs, got, err, test.want)
		}
	}
	for _, value := range []string{"0", "-1", "invalid"} {
		if _, err := resolveE2EVMCPU(value, 8); err == nil {
			t.Fatalf("invalid E2E_VM_CPU %q was accepted", value)
		}
	}
	if _, err := resolveE2EVMCPU("auto", 0); err == nil {
		t.Fatal("automatic E2E_VM_CPU accepted an unavailable host CPU count")
	}
}

func TestEnvFileRejectsCommands(t *testing.T) {
	if _, err := EvaluateAssignments(strings.NewReader("VALUE=$(id)\n"), "unsafe.env", nil); err == nil {
		t.Fatal("command substitution was accepted")
	}
}

func TestSingleQuotedValueIsLiteral(t *testing.T) {
	values, err := EvaluateAssignments(strings.NewReader("VALUE='$HOME'\n"), "literal.env", map[string]string{"HOME": "/operator"})
	if err != nil {
		t.Fatal(err)
	}
	if values["VALUE"] != "$HOME" {
		t.Fatalf("single-quoted value expanded: %q", values["VALUE"])
	}
}

func TestNormalizeAgentPersistLinksUsesExactSelection(t *testing.T) {
	tracker := newSettingTracker()
	defaults := tracker.addLayer("default", "shipped defaults", "agents.env", true, settingAny)
	tracker.record(defaults, "HOST_LINKS", "all-agent-links", "agents.env", 1, "")
	values := environment{
		"CODING_TOOL_INTEGRATIONS": "codex",
		"HOST_LINKS":               "all-agent-links",
		"AGENT_claude_PERSIST":     "claude-link\n",
		"AGENT_codex_PERSIST":      "codex-link\n",
		"AGENT_opencode_PERSIST":   "opencode-link\n",
	}
	normalizeAgentPersistLinks(values, tracker, defaults)
	if values["HOST_LINKS"] != "codex-link\n" {
		t.Fatalf("selected-agent links = %q, want Codex only", values["HOST_LINKS"])
	}

	explicit := tracker.addLayer("yard", "scalar settings", "sample-service.env", true, settingScalar)
	tracker.record(explicit, "HOST_LINKS", "", "sample-service.env", 1, "")
	values["HOST_LINKS"] = ""
	normalizeAgentPersistLinks(values, tracker, defaults)
	if values["HOST_LINKS"] != "" {
		t.Fatalf("explicit empty HOST_LINKS was replaced with %q", values["HOST_LINKS"])
	}
}

func TestReadAssignmentsOverPreservesExplicitProfileOverrides(t *testing.T) {
	values, err := EvaluateAssignments(strings.NewReader("IMAGE_DOCKERFILE=\"${IMAGE_DOCKERFILE:-}\"\nPROJECT_ENV_BASE_IMAGE=ubuntu:24.04\n"), "profile.conf", map[string]string{"IMAGE_DOCKERFILE": "docker/dev.Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	if values["IMAGE_DOCKERFILE"] != "docker/dev.Dockerfile" || values["PROJECT_ENV_BASE_IMAGE"] != "ubuntu:24.04" {
		t.Fatalf("profile overlay changed precedence: %#v", values)
	}
}

func TestLegacyYardSettingsNormalizeAtLayerBoundary(t *testing.T) {
	values := environment{"ACCESS_KIND": "local", "YARD_KIND": "container"}
	if err := (Catalog{}).applyEnvReaderValidated(strings.NewReader("YARD_TYPE=remote\nINSTANCE_TYPE=vm\nINSTANCE_NAME=yard-demo\nREMOTE_DEST=dev@owner.example\nREMOTE_YARD=demo\nBASE_IMAGE=images:debian/13\nYARD_PROFILES=sample-device\nAGENTS=codex\n"), "config.env", values, ScopeHost, false, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ACCESS_KIND": "remote", "YARD_KIND": "vm", "YARD_INSTANCE_NAME": "yard-demo",
		"OWNER_ENDPOINT": "dev@owner.example", "OWNER_YARD_NAME": "demo",
		"YARD_IMAGE": "images:debian/13", "ENVIRONMENT_PROFILES": "sample-device",
		"CODING_TOOL_INTEGRATIONS": "codex",
	}
	for name, expected := range want {
		if values[name] != expected {
			t.Fatalf("normalized %s = %q, want %q; values=%#v", name, values[name], expected, values)
		}
	}
	for _, legacy := range []string{
		"YARD_TYPE", "INSTANCE_TYPE", "INSTANCE_NAME", "REMOTE_DEST", "REMOTE_YARD",
		"BASE_IMAGE", "YARD_PROFILES", "AGENTS",
	} {
		if _, exists := values[legacy]; exists {
			t.Fatalf("legacy setting %s escaped input boundary: %#v", legacy, values)
		}
	}
}

func TestLegacyAndCanonicalSettingConflictFailsClosed(t *testing.T) {
	if err := (Catalog{}).applyEnvReaderValidated(strings.NewReader("ACCESS_KIND=local\nYARD_TYPE=remote\n"), "config.env", environment{}, ScopeHost, false, nil); err == nil ||
		!strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting canonical and legacy settings were accepted: %v", err)
	}
}

func TestLegacyEmptyCommandSelection(t *testing.T) {
	values := environment{"AGENTS": "none", "CODING_TOOL_INTEGRATIONS": ""}
	if err := normalizeLegacyEnvironment(values); err != nil {
		t.Fatal(err)
	}
	if value, present := values["CODING_TOOL_INTEGRATIONS"]; !present || value != "" {
		t.Fatalf("legacy none lost explicit empty: %#v", values)
	}
	if err := normalizeLegacyEnvironment(environment{"AGENTS": "none", "CODING_TOOL_INTEGRATIONS": "codex"}); err == nil {
		t.Fatal("conflicting empty command selection accepted")
	}
}

func TestLoadIgnoresRetiredCodexReleasePins(t *testing.T) {
	for _, yard := range []string{"default", "named"} {
		t.Run(yard, func(t *testing.T) {
			operatorHome := testkit.TempDir(t)
			configHome := filepath.Join(operatorHome, ".config", "subyard")
			hostFile := filepath.Join(configHome, "config.env")
			yardFile := filepath.Join(configHome, "yards", yard, "config.env")
			hostContent := "CODING_TOOL_INTEGRATIONS=codex\nCODEX_VERSION=0.147.0\nCODEX_SHA256_AMD64=" + strings.Repeat("a", 64) + "\nCODEX_SHA256_ARM64=" + strings.Repeat("b", 64) + "\n"
			yardContent := "SSH_PORT=2231\nCODEX_VERSION=latest\nCODEX_SHA256_AMD64=\nCODEX_SHA256_ARM64=\n"
			writeFixture(t, hostFile, hostContent)
			writeFixture(t, yardFile, yardContent)
			loaded, err := Load(LoadOptions{
				RepositoryRoot: filepath.Join("..", ".."), OperatorHome: operatorHome,
				YardName: yard,
				Environment: map[string]string{
					"SUBYARD_CONFIG_HOME": configHome,
					"CODEX_VERSION":       "not-a-version", "CODEX_SHA256_AMD64": "not-a-checksum", "CODEX_SHA256_ARM64": "",
				},
			})
			if err != nil {
				t.Fatalf("legacy persisted Codex pins blocked load: %v", err)
			}
			if loaded.Context.SSHPort != 2231 || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
				t.Fatalf("ordinary host and yard settings did not load: %#v", loaded.Context)
			}
			for _, name := range []string{"CODEX_VERSION", "CODEX_SHA256_AMD64", "CODEX_SHA256_ARM64"} {
				if _, exists := loaded.Environment[name]; exists {
					t.Errorf("retired %s remains effective", name)
				}
				if _, exists := loaded.Settings[name]; exists {
					t.Errorf("retired %s remains visible in setting provenance", name)
				}
				if err := ValidateSetting(ScopeHost, name, "0.147.0", false); err == nil {
					t.Errorf("new typed write to retired %s was accepted", name)
				}
			}
			for path, want := range map[string]string{hostFile: hostContent, yardFile: yardContent} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("loading rewrote persisted configuration %s: %v", path, err)
				}
			}
		})
	}
}

// Host instruction copies are retired: older configuration keeps loading, the
// values are discarded, and each non-empty assignment gets its own notice.
func TestLoadIgnoresRetiredHostInstructionSettings(t *testing.T) {
	operatorHome := testkit.TempDir(t)
	configHome := filepath.Join(operatorHome, ".config", "subyard")
	hostFile := filepath.Join(configHome, "config.env")
	yardFile := filepath.Join(configHome, "yards", "default", "config.env")
	hostContent := "CODING_TOOL_INTEGRATIONS=\"claude codex opencode\"\nHOST_CLAUDE_MD=/home/operator/.claude/CLAUDE.md\nHOST_CODEX_AGENTS_MD=/home/operator/.codex/AGENTS.md\n"
	yardContent := "SSH_PORT=2231\nHOST_OPENCODE_AGENTS_MD=/home/operator/.config/opencode/AGENTS.md\nHOST_CLAUDE_MD=\n"
	writeFixture(t, hostFile, hostContent)
	writeFixture(t, yardFile, yardContent)
	loaded, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: operatorHome,
		Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome}})
	if err != nil {
		t.Fatalf("retired instruction settings blocked load: %v", err)
	}
	for _, name := range []string{"HOST_CLAUDE_MD", "HOST_CODEX_AGENTS_MD", "HOST_OPENCODE_AGENTS_MD"} {
		if _, exists := loaded.Environment[name]; exists {
			t.Errorf("retired %s remains effective", name)
		}
		if _, exists := loaded.Settings[name]; exists {
			t.Errorf("retired %s remains visible in setting provenance", name)
		}
		if err := ValidateSetting(ScopeHost, name, "/tmp/AGENTS.md", false); err == nil {
			t.Errorf("new typed write to retired %s was accepted", name)
		}
	}
	var notices []string
	for _, assignment := range loaded.RetiredAssignments() {
		if !strings.HasPrefix(assignment.Repair(), "remove line ") {
			t.Fatalf("retired assignment repair: %s", assignment)
		}
		notices = append(notices, fmt.Sprintf("%s:%s:%d", assignment.Name, assignment.Path, assignment.Line))
	}
	want := []string{"HOST_CLAUDE_MD:" + hostFile + ":2", "HOST_CODEX_AGENTS_MD:" + hostFile + ":3", "HOST_OPENCODE_AGENTS_MD:" + yardFile + ":2"}
	slices.Sort(notices)
	if !slices.Equal(notices, want) {
		t.Fatalf("notices %v, want %v", notices, want)
	}
}

func TestRetiredCodexAssignmentsCannotAffectExportsOrOtherSettings(t *testing.T) {
	file := filepath.Join(testkit.TempDir(t), "config.env")
	writeFixture(t, file, "CODEX_VERSION=\"${SSH_PORT:=4444}\"\n: \"${CODEX_SHA256_AMD64:=${DEV_UID:=2000}}\"\nCODEX_SHA256_ARM64=\nCODING_TOOL_INTEGRATIONS=codex\n")
	values, err := ReadAssignmentsOver(file, map[string]string{"CODEX_VERSION": "0.147.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["CODING_TOOL_INTEGRATIONS"] != "codex" {
		t.Fatalf("retired assignments changed effective input: %#v", values)
	}
	names, err := AssignedSettingNames(file, []byte("CODEX_VERSION=0.147.0\nCODEX_SHA256_ARM64=\nCODING_TOOL_INTEGRATIONS=codex\n"))
	if err != nil || len(names) != 1 || names[0] != "CODING_TOOL_INTEGRATIONS" {
		t.Fatalf("retired assignments escaped enumeration: %v, %v", names, err)
	}
}

func TestE2EConfigValidation(t *testing.T) {
	valid := environment{
		"E2E_DISK_BUDGET":    "160GiB",
		"E2E_CACHE_BUDGET":   "24GiB",
		"E2E_DISK_RESERVE":   "5GiB",
		"E2E_MEMORY_RESERVE": "2GiB",
		"E2E_VM_OVERHEAD":    "512MiB",

		"E2E_VM_IMAGE": "images:debian/13/cloud", "E2E_VM_CPU": "2",
		"E2E_VM_MEMORY": "4GiB", "E2E_VM_DISK": "10GiB",
		"E2E_VM_SLOT_COUNT": "2", "E2E_VM_BOOT_TIMEOUT": "300",
	}
	if err := validateE2EConfig(valid, true); err != nil {
		t.Fatal(err)
	}
	valid["E2E_DISK_BUDGET"] = "0GiB"
	if err := validateE2EConfig(valid, true); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSetting(ScopeYard, "E2E_DISK_BUDGET", "0GiB", false); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"E2E_DISK_BUDGET":    "-1GiB",
		"E2E_CACHE_BUDGET":   "0GiB",
		"E2E_DISK_RESERVE":   "0GiB",
		"E2E_MEMORY_RESERVE": "0GiB",
		"E2E_VM_OVERHEAD":    "0GiB",

		"E2E_VM_IMAGE": "-unsafe", "E2E_VM_CPU": "0", "E2E_VM_MEMORY": "4GB",
		"E2E_VM_DISK": "9GiB", "E2E_VM_SLOT_COUNT": "0", "E2E_VM_BOOT_TIMEOUT": "29",
	} {
		values := make(environment, len(valid))
		for key, current := range valid {
			values[key] = current
		}
		values[name] = value
		if err := validateE2EConfig(values, true); err == nil {
			t.Errorf("%s=%q was accepted", name, value)
		}
	}
	invalid := maps.Clone(valid)
	invalid["E2E_VM_IMAGE"], invalid["E2E_VM_MEMORY"], invalid["E2E_VM_DISK"], invalid["E2E_VM_BOOT_TIMEOUT"] = "-SENTINEL", "4GB", "9GiB", "29"
	diagnostic := Diagnose("", validateE2EConfig(invalid, true))
	want := "E2E_VM_BOOT_TIMEOUT must be an integer from 30 to 1800; E2E_VM_DISK must be at least 10GiB; E2E_VM_IMAGE contains unsafe characters; E2E_VM_MEMORY must use a positive MiB or GiB value"
	if message := diagnostic.Message("generic"); message != want || diagnostic.Unclassified {
		t.Fatalf("E2E refusals = %q", message)
	}
}

func TestEngineReexecDoesNotLeakPriorYardContext(t *testing.T) {
	root := testkit.TempDir(t)
	operatorHome := filepath.Join(root, "home")
	configHome := filepath.Join(root, "config-home")
	for _, directory := range []string{filepath.Join(root, "config"), filepath.Join(configHome, "yards")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(root, "config", "incus.project.env"), `: "${INCUS_PROJECT:=subyard}"
: "${RESTRICTED_DISK_PATHS:=/srv/subyard}"`)
	writeFixture(t, filepath.Join(root, "config", "subyard.env"), `: "${YARD_INSTANCE_NAME:=yard}"
: "${YARD_KIND:=container}"
: "${SHIFT_MODE:=shift}"
: "${FORWARD_SSH_AGENT:=0}"
: "${DEV_SUDO:=no}"
: "${DEV_UID:=1000}"
: "${SSH_HOST:=yard}"
: "${SSH_PORT:=2222}"`)
	writeFixture(t, filepath.Join(root, "config", "host.env"), `: "${SUBYARD_CONFIG_HOME:=$SUBYARD_OPERATOR_HOME/.config/subyard}"
: "${SUBYARD_HOME:=$SUBYARD_OPERATOR_HOME/.subyard}"
: "${STORAGE_PATH:=$SUBYARD_HOME/incus/storage}"
: "${HOST_BASE:=${RESTRICTED_DISK_PATHS:-/srv/subyard}}"`)
	writeFixture(t, filepath.Join(configHome, "yards", "named.env"), "SSH_PORT=3333\n")

	loaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: operatorHome, YardName: "named",
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": operatorHome, "SUBYARD_CONFIG_HOME": configHome,
			"SUBYARD_ENGINE_CONTEXT": "1", "YARD_INSTANCE_NAME": "yard", "INCUS_PROJECT": "subyard",
			"SSH_HOST": "yard", "SSH_PORT": "2222", "RESTRICTED_DISK_PATHS": "/srv/subyard",
			"HOST_BASE": "/srv/subyard", "YARD_KIND": "container", "SHIFT_MODE": "shift",
			"FORWARD_SSH_AGENT": "0", "DEV_SUDO": "no", "DEV_UID": "1000",
			"YARD_TEMPLATE": "stale", "NESTED_E2E_VMS": "1",
			"E2E_DISK_BUDGET":    "999GiB",
			"E2E_CACHE_BUDGET":   "999GiB",
			"E2E_DISK_RESERVE":   "999GiB",
			"E2E_MEMORY_RESERVE": "999GiB",
			"E2E_VM_OVERHEAD":    "999GiB",

			"CODING_TOOL_INTEGRATIONS": "unknown-prior-tool", "ALLOWS_CODING_TOOLS": "false",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := loaded.Context
	if ctx.YardInstanceName != "yard-named" || ctx.IncusProject != "subyard-named" || ctx.SSHHost != "yard-named" {
		t.Fatalf("prior context leaked into named reload: %#v", ctx)
	}
	if loaded.Integrations.Present || !loaded.Integrations.AllowsCodingTools {
		t.Fatalf("prior integration context leaked: %#v", loaded.Integrations)
	}
	if ctx.NestedE2EVMs || loaded.Environment["YARD_TEMPLATE"] != "" {
		t.Fatalf("prior E2E context leaked into named reload: %#v", loaded.Environment)
	}
	for name, expected := range map[string]string{"E2E_DISK_BUDGET": "0GiB", "E2E_CACHE_BUDGET": "24GiB", "E2E_DISK_RESERVE": "5GiB", "E2E_MEMORY_RESERVE": "4GiB", "E2E_VM_OVERHEAD": "256MiB"} {
		if loaded.Environment[name] != expected {
			t.Fatalf("prior budget leaked: %s", name)
		}
	}

}

func TestMultilineAndNestedDefaults(t *testing.T) {
	values, err := EvaluateAssignments(strings.NewReader(`ROOT=/srv/test
A="${A:-
one
two
}"
B="${B:-${A}/three}"
`), "config.env", nil)
	if err != nil {
		t.Fatal(err)
	}
	if values["A"] != "\none\ntwo\n" || values["B"] != "\none\ntwo\n/three" {
		t.Fatalf("unexpected expansion: A=%q B=%q", values["A"], values["B"])
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content), 0o600)
}
