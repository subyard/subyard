package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func activationSelectionRepairFixture(t *testing.T, extraAssets ...map[string]string) (string, string, string, []string) {
	t.Helper()
	_, home, configHome, environment := configCommandFixture(t)
	runtimeRoot := aliasRepairRuntimeFixture(t, home)
	root := filepath.Join(runtimeRoot, "releases", "1.2.3-owner")
	capabilities := releasetransition.ActivationRecoveryLifecycleCapabilities()
	reply, err := releasetransition.MarshalRecoveryProcessResponse(releasetransition.RecoveryProcessResponse{SchemaVersion: 2, Capabilities: &capabilities})
	if err != nil {
		t.Fatal(err)
	}
	engine := []byte("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.3\\n';; _release-transition) cat >/dev/null; printf '%s' '" + string(reply) + "';; *) exit 99;; esac\n")
	testkit.WriteFile(t, filepath.Join(root, "bin/yard-engine"), engine, 0o700)
	manifestPath := filepath.Join(root, "runtime-files.sha256")
	manifest := mustReadConfigFixture(t, manifestPath)
	end := bytes.IndexByte(manifest, '\n')
	manifest = append([]byte(fmt.Sprintf("%x  ./bin/yard-engine\n", sha256.Sum256(engine))), manifest[end+1:]...)
	for _, assets := range extraAssets {
		for relative, content := range assets {
			mode := os.FileMode(0o600)
			if strings.HasSuffix(relative, ".sh") {
				mode = 0o700
			}
			writeProfileTestFile(t, filepath.Join(root, relative), []byte(content), mode)
			manifest = append(manifest, []byte(fmt.Sprintf("%x  ./%s\n", sha256.Sum256([]byte(content)), relative))...)
		}
	}
	testkit.WriteFile(t, manifestPath, manifest, 0o600)
	registryPayload := mustReadConfigFixture(t, filepath.Join(root, "config/release-transition.json"))
	registry, _, err := releasetransition.ParseRegistryV2(registryPayload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(registry)
	for _, migration := range registry.Migrations {
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	payload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(configHome, "release-transition/v2/ledger.json"), payload, 0o600)
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: runtimeRoot, ConfigHome: configHome, Yard: "default", Target: "1.2.3-owner",
		Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest)))}
	actor := &processDriftReconciler{failOnce: true}
	invoke := func() releasetransition.ProcessResponse {
		response, err := executeReleaseTransitionRequest(context.Background(), root, request, func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }, []releasetransition.V2ActivationReconciler{actor}, releaseTransitionOwnerFixture{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	inspection := invoke()
	if inspection.Inspection == nil {
		t.Fatal("missing activation inspection")
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: inspection.Inspection.Plan, Authorization: "fixture-grant"}
	result := invoke()
	if result.Outcome == nil || result.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("missing interrupted activation: %#v", result)
	}
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
	return root, configHome, filepath.Join(configHome, "config.env"), environment
}

func activationProfileRepairAssets() map[string]string {
	return map[string]string{
		"config/profiles/fixture-selected/profile.json":  `{"schema_version":1,"selected_provision_only":true}`,
		"config/profiles/fixture-selected/provision.sh":  "#!/bin/sh\n# subyard-provision-check-v1\nexit 99\n",
		"config/profiles/fixture-legacy/provision.sh":    "#!/bin/sh\n# subyard-provision-check-v1\nexit 99\n",
		"config/profiles/fixture-dedicated/profile.conf": "PROVISION_SCOPE=dedicated\n",
		"config/profiles/fixture-dedicated/provision.sh": "#!/bin/sh\n# subyard-provision-check-v1\nexit 99\n",
		"config/profiles/fixture-neighbor/profile.json":  `{"schema_version":1,"selected_provision_only":true}`,
	}
}

func activationRepairHistory(t *testing.T, root, home string) map[string]string {
	t.Helper()
	state := map[string]string{}
	for _, path := range []string{filepath.Join(home, "release-transition/v2/journal.json"), filepath.Join(home, "release-transition/v2/ledger.json"),
		filepath.Join(filepath.Dir(filepath.Dir(root)), "current"), filepath.Join(filepath.Dir(filepath.Dir(root)), "previous")} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			state[path] = "absent"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		value := ""
		if info.Mode()&os.ModeSymlink != 0 {
			value, err = os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			value = string(mustReadConfigFixture(t, path))
		}
		state[path] = fmt.Sprintf("%v:%s", info.Mode(), value)
	}
	return state
}

func TestActivationProfileSelectionRepairRejectsLegacyFallbackExpansion(t *testing.T) {
	assets := activationProfileRepairAssets()
	for _, arguments := range [][]string{
		{"config", "reduce-selection", "ENVIRONMENT_PROFILES", "", "--scope", "host", "--local", "--yes"},
		{"config", "set", "ENVIRONMENT_PROFILES", "", "--scope", "host", "--yes"},
		{"profile", "disable", "fixture-selected", "--yes"},
	} {
		t.Run(strings.Join(arguments[:2], "-"), func(t *testing.T) {
			root, configHome, path, environment := activationSelectionRepairFixture(t, assets)
			before := []byte("# preserve unrelated records\nENVIRONMENT_PROFILES='fixture-selected'\nSSH_PORT=2398\n")
			testkit.WriteFile(t, path, before, 0o640)
			journalPath := filepath.Join(configHome, "release-transition/v2/journal.json")
			journal := mustReadConfigFixture(t, journalPath)
			program, err := New(Options{RepositoryRoot: root, Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			selected, err := program.prepareProvisionExecution(loaded, nil, nil)
			if err != nil || !slices.Equal(selected.profiles, []string{"fixture-selected"}) {
				t.Fatalf("fixture selection is not isolated: %#v %v", selected, err)
			}
			loaded.Environment["ENVIRONMENT_PROFILES"] = ""
			fallback, err := program.prepareProvisionExecution(loaded, nil, nil)
			if err != nil || !slices.Contains(fallback.profiles, "fixture-legacy") || slices.Contains(fallback.profiles, "fixture-dedicated") {
				t.Fatalf("fixture fallback or dedicated role guard missing: %#v %v", fallback, err)
			}
			var stdout, stderr bytes.Buffer
			code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: arguments, Stdout: &stdout, Stderr: &stderr})
			if code == 0 {
				t.Fatalf("empty profile selection accepted despite expanding legacy provision fallback: %s %s", stdout.String(), stderr.String())
			}
			if !bytes.Equal(before, mustReadConfigFixture(t, path)) || !bytes.Equal(journal, mustReadConfigFixture(t, journalPath)) {
				t.Fatal("rejected profile expansion changed persistent selection or activation history")
			}
			if _, err := os.Lstat(filepath.Join(configHome, "yards/default/config.env")); !os.IsNotExist(err) {
				t.Fatal("rejected profile expansion created a yard selection")
			}
		})
	}
}

func TestActivationProfileSelectionRepairRejectsNamedDescriptorOnlyExpansion(t *testing.T) {
	root, _, _, environment := activationSelectionRepairFixture(t, activationProfileRepairAssets())
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	before, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	before.Context.YardName = "neighbor"
	before.Environment = maps.Clone(before.Environment)
	delete(before.Environment, "ENVIRONMENT_PROFILES")
	after := before
	after.Environment = maps.Clone(before.Environment)
	after.Environment["ENVIRONMENT_PROFILES"] = "fixture-neighbor"
	provision, err := program.prepareProvisionExecution(after, nil, nil)
	if err != nil || len(provision.profiles) != 0 {
		t.Fatalf("fixture is not descriptor-only: %#v %v", provision, err)
	}
	if err := program.checkSelectionReduction(before, after, "ENVIRONMENT_PROFILES"); err == nil || !strings.Contains(err.Error(), "yard neighbor:") {
		t.Fatalf("named descriptor-only expansion was accepted: %v", err)
	}
}

func TestActivationProfileSelectionRepairPreservesConsentAndHistory(t *testing.T) {
	for _, kind := range []string{"reduce", "disable", "set"} {
		t.Run(kind, func(t *testing.T) {
			root, home, path, environment := activationSelectionRepairFixture(t, activationProfileRepairAssets())
			before := []byte("# preserve unrelated records\nENVIRONMENT_PROFILES='fixture-selected fixture-neighbor'\nSSH_PORT=2398\n")
			testkit.WriteFile(t, path, before, 0o640)
			history := activationRepairHistory(t, root, home)
			arguments := []string{"config", "reduce-selection", "ENVIRONMENT_PROFILES", "fixture-selected", "--scope", "host", "--local"}
			target, mode := path, os.FileMode(0o640)
			stages := []string{"preview", "decline", "apply"}
			if kind != "reduce" {
				target, mode = filepath.Join(home, "yards/default/config.env"), 0o600
				stages = stages[1:]
				arguments = []string{"profile", "disable", "fixture-neighbor"}
				if kind == "set" {
					arguments = []string{"config", "set", "ENVIRONMENT_PROFILES", "fixture-selected", "--scope", "yard"}
				}
			}
			for _, stage := range stages {
				var stdout, stderr bytes.Buffer
				args := slices.Clone(arguments)
				if stage == "preview" {
					args = append(args, "--check")
				}
				prompt := &testkit.Prompt{Answers: []bool{stage == "apply"}}
				code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: args, Prompt: prompt, Stdout: &stdout, Stderr: &stderr})
				if code != 0 && stage != "decline" || stage == "decline" && code != 1 {
					t.Fatalf("%s code=%d %s %s", stage, code, stdout.String(), stderr.String())
				}
				if !reflect.DeepEqual(history, activationRepairHistory(t, root, home)) {
					t.Fatal("profile source reduction changed journal, ledger, release links or their modes")
				}
				if stage != "apply" {
					if !bytes.Equal(before, mustReadConfigFixture(t, path)) {
						t.Fatal("unconsented profile reduction changed host settings")
					}
					if target != path {
						if _, err := os.Lstat(target); !os.IsNotExist(err) {
							t.Fatal("unconsented typed profile reduction created yard settings")
						}
					}
					continue
				}
				if len(prompt.Requests) != 1 || !strings.Contains(stdout.String(), "activation remains pending") {
					t.Fatal("profile source consent or separate activation boundary missing")
				}
				want := "ENVIRONMENT_PROFILES='fixture-selected'\n"
				if target == path {
					want = "# preserve unrelated records\n" + want + "SSH_PORT=2398\n"
				} else if !bytes.Equal(before, mustReadConfigFixture(t, path)) {
					t.Fatal("yard profile reduction changed inherited host settings")
				}
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm() != mode || string(mustReadConfigFixture(t, target)) != want {
					t.Fatal("profile reduction changed unrelated settings or exact source mode")
				}
			}
		})
	}
}

func TestActivationProfileSelectionRepairRejectsExpansionOverridesAndConsentDrift(t *testing.T) {
	for _, scenario := range []string{"expansion", "override", "drift", "source-drift"} {
		t.Run(scenario, func(t *testing.T) {
			root, home, path, environment := activationSelectionRepairFixture(t, activationProfileRepairAssets())
			before := []byte("ENVIRONMENT_PROFILES='fixture-selected fixture-neighbor'\nSSH_PORT=2398\n")
			testkit.WriteFile(t, path, before, 0o640)
			history := activationRepairHistory(t, root, home)
			value := "fixture-selected"
			if scenario == "expansion" {
				value = "fixture-legacy"
			}
			if scenario == "override" {
				environment = append(environment, "ENVIRONMENT_PROFILES=fixture-selected")
			}
			prompt := &callbackPrompt{callback: func() {
				if scenario == "source-drift" {
					testkit.WriteFile(t, path, append(slices.Clone(before), []byte("# independent update\n")...), 0o640)
				} else {
					testkit.WriteFile(t, filepath.Join(home, "release-transition/v2/.journal.json.pending"), []byte("{}\n"), 0o600)
				}
			}}
			var stderr bytes.Buffer
			code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment,
				Arguments: []string{"config", "reduce-selection", "ENVIRONMENT_PROFILES", value, "--scope", "host", "--local"}, Prompt: prompt, Stdout: &bytes.Buffer{}, Stderr: &stderr})
			info, err := os.Stat(path)
			want := before
			if scenario == "source-drift" {
				want = append(slices.Clone(before), []byte("# independent update\n")...)
			}
			if code == 0 || err != nil || info.Mode().Perm() != 0o640 || !bytes.Equal(want, mustReadConfigFixture(t, path)) || !reflect.DeepEqual(history, activationRepairHistory(t, root, home)) {
				t.Fatalf("unsafe profile reduction accepted: code=%d %s", code, stderr.String())
			}
			if (scenario == "drift" || scenario == "source-drift") && (len(prompt.requests) != 1 || !strings.Contains(stderr.String(), "stale")) {
				t.Fatalf("profile source consent did not bind history: %s", stderr.String())
			}
		})
	}
}

func TestActivationSelectionRepairPreservesHistoryAndRequiresSourceConsent(t *testing.T) {
	root, configHome, path, environment := activationSelectionRepairFixture(t)
	before := []byte("# preserve unrelated records\nCODING_TOOL_INTEGRATIONS='codex claude'\nSSH_PORT=2398\n")
	testkit.WriteFile(t, path, before, 0o640)
	journalPath := filepath.Join(configHome, "release-transition/v2/journal.json")
	journal := mustReadConfigFixture(t, journalPath)
	for _, stage := range []string{"preview", "decline", "apply"} {
		var stdout, stderr bytes.Buffer
		arguments := []string{"config", "reduce-selection", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--local"}
		prompt := &testkit.Prompt{Answers: []bool{stage == "apply"}}
		if stage == "preview" {
			arguments = append(arguments, "--check")
		}
		code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: arguments, Prompt: prompt, Stdout: &stdout, Stderr: &stderr})
		if stage == "decline" {
			if code != 1 {
				t.Fatalf("decline code=%d %s", code, stderr.String())
			}
		} else if code != 0 {
			t.Fatalf("%s code=%d %s %s", stage, code, stdout.String(), stderr.String())
		}
		if !bytes.Equal(journal, mustReadConfigFixture(t, journalPath)) {
			t.Fatal("source repair rewrote interrupted history")
		}
		if stage != "apply" && !bytes.Equal(before, mustReadConfigFixture(t, path)) {
			t.Fatal("unconsented source repair wrote desired settings")
		}
		if stage == "apply" {
			if len(prompt.Requests) != 1 || !strings.Contains(stdout.String(), "activation remains pending") {
				t.Fatal("source consent or separate activation recovery boundary missing")
			}
			if string(mustReadConfigFixture(t, path)) != "# preserve unrelated records\nCODING_TOOL_INTEGRATIONS='codex'\nSSH_PORT=2398\n" {
				t.Fatal("source repair changed unrelated settings")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatal("source mode changed")
			}
		}
	}
}

func TestActivationSelectionRepairRejectsExpansionOverridesAndConsentDrift(t *testing.T) {
	for _, scenario := range []string{"expansion", "override", "drift"} {
		t.Run(scenario, func(t *testing.T) {
			root, configHome, path, environment := activationSelectionRepairFixture(t)
			before := []byte("CODING_TOOL_INTEGRATIONS='codex'\n")
			testkit.WriteFile(t, path, before, 0o600)
			value := ""
			if scenario == "expansion" {
				value = "codex claude"
			}
			if scenario == "override" {
				environment = append(environment, "CODING_TOOL_INTEGRATIONS=claude")
			}
			prompt := &callbackPrompt{callback: func() {
				testkit.WriteFile(t, filepath.Join(configHome, "release-transition/v2/.journal.json.pending"), []byte("{}\n"), 0o600)
			}}
			var stderr bytes.Buffer
			code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"config", "reduce-selection", "CODING_TOOL_INTEGRATIONS", value, "--scope", "host", "--local"}, Prompt: prompt, Stdout: &bytes.Buffer{}, Stderr: &stderr})
			if code == 0 || !bytes.Equal(before, mustReadConfigFixture(t, path)) {
				t.Fatalf("unsafe repair accepted: %d %s", code, stderr.String())
			}
			if scenario == "drift" && (len(prompt.requests) != 1 || !strings.Contains(stderr.String(), "stale")) {
				t.Fatalf("changed history not rebound: %s", stderr.String())
			}
		})
	}
}

func TestActivationSelectionRepairTypedDisableAndSetShareSourceOnlyBoundary(t *testing.T) {
	t.Run("dependency-only-disable", func(t *testing.T) {
		root, home, path, environment := activationSelectionRepairFixture(t, map[string]string{
			"config/agents/wrapper/integration.json": `{"schema_version":1,"command":"synthetic-cli","depends":["base"]}`,
			"config/agents/base/integration.json":    `{"schema_version":1,"command":"synthetic-cli"}`,
		})
		before := []byte("CODING_TOOL_INTEGRATIONS='wrapper'\n")
		testkit.WriteFile(t, path, before, 0o640)
		program, err := New(Options{RepositoryRoot: root, Environment: environment})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := program.loadSelectionRepair("default")
		if err != nil || !slices.Equal(loaded.Integrations.Requested, []string{"wrapper"}) || !slices.Contains(loaded.Integrations.Effective, "base") {
			t.Fatalf("dependency-only fixture: %#v %v", loaded.Integrations, err)
		}
		history := activationRepairHistory(t, root, home)
		prompt := &testkit.Prompt{Answers: []bool{true}}
		var stdout, stderr bytes.Buffer
		code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment,
			Arguments: []string{"integration", "disable", "base"}, Prompt: prompt, Stdout: &stdout, Stderr: &stderr})
		if code != 2 || len(prompt.Requests) != 0 || !strings.Contains(stderr.String(), "disable its dependents explicitly first") {
			t.Fatalf("dependency-only disable was not rejected before consent: code=%d prompts=%d %s %s", code, len(prompt.Requests), stdout.String(), stderr.String())
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o640 || !bytes.Equal(before, mustReadConfigFixture(t, path)) || !reflect.DeepEqual(history, activationRepairHistory(t, root, home)) {
			t.Fatal("dependency-only disable changed inherited selection, its mode or protected history")
		}
		if _, err := os.Lstat(filepath.Join(home, "yards/default/config.env")); !os.IsNotExist(err) {
			t.Fatal("dependency-only disable created a yard selection override")
		}
		loaded, err = program.loadSelectionRepair("default")
		if err != nil || !slices.Equal(loaded.Integrations.Requested, []string{"wrapper"}) || !slices.Contains(loaded.Integrations.Effective, "base") {
			t.Fatalf("dependency-only disable changed effective dependency: %#v %v", loaded.Integrations, err)
		}
	})
	for _, arguments := range [][]string{
		{"integration", "disable", "claude", "--yes"},
		{"config", "set", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "yard", "--yes"},
	} {
		t.Run(arguments[0], func(t *testing.T) {
			root, configHome, path, environment := activationSelectionRepairFixture(t)
			testkit.WriteFile(t, path, []byte("CODING_TOOL_INTEGRATIONS='codex claude'\n"), 0o600)
			journalPath := filepath.Join(configHome, "release-transition/v2/journal.json")
			journal := mustReadConfigFixture(t, journalPath)
			var stdout, stderr bytes.Buffer
			if code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: arguments, Stdout: &stdout, Stderr: &stderr}); code != 0 {
				t.Fatalf("typed source reduction: %d %s %s", code, stdout.String(), stderr.String())
			}
			local := mustReadConfigFixture(t, filepath.Join(configHome, "yards/default/config.env"))
			if string(local) != "CODING_TOOL_INTEGRATIONS='codex'\n" || !bytes.Equal(journal, mustReadConfigFixture(t, journalPath)) {
				t.Fatal("typed reduction did not preserve source-only boundary")
			}
		})
	}
}

func completedSelectionAuthoringFixture(t *testing.T, migrateSSH ...bool) (*CLI, string, string) {
	t.Helper()
	f := newConfigApplyRepairFixture(t, false)
	oldRoot := f.cli.options.RepositoryRoot
	runtimeRoot := f.cli.env["YARD_RUNTIME_ROOT"]
	root := filepath.Join(runtimeRoot, "releases", "1.2.3-aaaaaaaaaaaa")
	commandsPath := filepath.Join(oldRoot, "config/commands.registry")
	commands := mustReadConfigFixture(t, commandsPath)
	for _, line := range strings.Split(string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config/commands.registry"))), "\n") {
		if strings.HasPrefix(line, "integration|") || strings.HasPrefix(line, "profile|") || strings.HasPrefix(line, "config|") {
			commands = append(commands, []byte(line+"\n")...)
		}
	}
	testkit.WriteFile(t, commandsPath, commands, 0o600)
	agentsPath := filepath.Join(oldRoot, "config/agents.env")
	agents := append(mustReadConfigFixture(t, agentsPath), []byte("AGENT_claude_COMMAND=synthetic-cli\nAGENT_claude_PROVISION=${SUBYARD_CONFIG_DIR}/synthetic-integration-provision.sh\n")...)
	testkit.WriteFile(t, agentsPath, agents, 0o600)
	writeProfileTestFile(t, filepath.Join(oldRoot, "config/synthetic-integration-provision.sh"), []byte("#!/bin/sh\nexit 99\n"), 0o700)
	writeProfileTestFile(t, filepath.Join(oldRoot, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	writeProfileTestFile(t, filepath.Join(oldRoot, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 99\n"), 0o700)
	writeProfileTestFile(t, filepath.Join(oldRoot, "config/release-checkpoint.json"), mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config/release-checkpoint.json")), 0o600)

	for _, tree := range []string{"config", "scripts"} {
		err := filepath.WalkDir(filepath.Join(oldRoot, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(oldRoot, path)
			if err != nil {
				return err
			}
			target := filepath.Join(root, relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			testkit.WriteFile(t, target, mustReadConfigFixture(t, path), info.Mode().Perm())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for relative, content := range activationProfileRepairAssets() {
		mode := os.FileMode(0o600)
		if strings.HasSuffix(relative, ".sh") {
			mode = 0o700
		}
		writeProfileTestFile(t, filepath.Join(root, relative), []byte(content), mode)
	}
	manifest := ""
	for _, tree := range []string{"bin", "config", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			manifest += fmt.Sprintf("%x  ./%s\n", sha256.Sum256(mustReadConfigFixture(t, path)), filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest), 0o600)
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, f.journalPath))
	if err != nil {
		t.Fatal(err)
	}
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256([]byte(manifest))))
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, f.journalPath, payload, 0o600)
	home := f.cli.env["SUBYARD_CONFIG_HOME"]
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := releasetransition.ParseRegistryV2(mustReadConfigFixture(t, filepath.Join(root, "config/release-transition.json")), releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if len(migrateSSH) != 0 && migrateSSH[0] {
		// Generate actual SSH pin and ledger evidence under compact authority,
		// with the compatibility projection retained at settings epoch two.
		if err := os.Remove(f.journalPath); err != nil {
			t.Fatal(err)
		}
		before := releasetransition.BaselineLedgerV2(registry)
		for _, migration := range registry.Migrations {
			if migration.Kind == "local-ssh-port-settings-v2-to-v3" {
				break
			}
			before, err = before.Advance(registry, migration)
			if err != nil {
				t.Fatal(err)
			}
		}
		payload, _, err := releasetransition.MarshalLedgerV2(before, registry)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(home, "release-transition/v2/ledger.json"), payload, 0o600)
		writeProfileTestFile(t, filepath.Join(home, config.GitSettingsRelativePath, "config.env"), []byte("SSH_PORT=2240\n"), 0o600)
		writeProfileTestFile(t, filepath.Join(home, config.GitSettingsRelativePath, "yards/named/config.env"), []byte("SSH_PORT=2241\n"), 0o600)
	}
	ledger, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(ledger, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	if len(migrateSSH) != 0 && migrateSSH[0] {
		links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := links.Observe()
		if err != nil {
			t.Fatal(err)
		}
		actor := &processDriftReconciler{converged: true}
		transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
			ConfigHome: home, OperatorHome: f.cli.env["HOME"], CandidateConfigDir: filepath.Join(root, "config"), RegistryPayload: mustReadConfigFixture(t, filepath.Join(root, "config/release-transition.json")), ArtifactDigest: journal.ArtifactDigest,
			Releases: releasetransition.ReleasePair{From: actual.Active, Previous: actual.Previous, Target: actual.Active}, MigrationCheckpoint: true, SourceMigrationCheckpoint: true,
			NewTransactionID:  func() releasetransition.TransactionID { return journal.Transaction },
			OwnerRegistration: releaseTransitionOwnerFixture{}, Reconcilers: []releasetransition.V2ActivationReconciler{actor},
			ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() }, ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
				return links.Activate(pair)
			},
			VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		})
		if err != nil {
			t.Fatal(err)
		}
		goal := releasetransition.Goal{Target: actual.Active, Direction: releasetransition.DirectionActivateTarget}
		inspection, err := transition.Inspect(context.Background(), goal)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "fixture-original-grant"})
		if err != nil || outcome.Status != releasetransition.StatusReady {
			t.Fatalf("actual SSH history: %#v %v", outcome, err)
		}
		journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, f.journalPath))
		if err != nil || journal.Checkpoint != releasetransition.JournalComplete || len(journal.Steps) != 3 {
			t.Fatalf("actual completed pin evidence: steps=%d err=%v", len(journal.Steps), err)
		}
		for _, step := range journal.Steps {
			if step.Checkpoint != releasetransition.StepVerified {
				t.Fatal("SSH step not verified")
			}
		}
		state, err := store.ReadMigrationLedger(registry, digest)
		if err != nil || !state.Checkpointed || state.Ledger.Domains["settings"].Epoch != 3 {
			t.Fatal("actual compact SSH authority missing")
		}
		legacy, _, err := releasetransition.ParseLedgerV2(state.ProjectionSnapshot.Payload, registry)
		if err != nil || legacy.Domains["settings"].Epoch != 2 {
			t.Fatal("compatibility projection not retained")
		}
		for yard, port := range map[string]string{"default": "2240", "named": "2241"} {
			path := filepath.Join(home, "yards", yard, "config.env")
			values, err := config.ReadAssignments(path)
			info, statErr := os.Stat(path)
			if err != nil || statErr != nil || values["SSH_PORT"] != port || info.Mode().Perm() != 0o600 {
				t.Fatalf("native pinCAS missing for %s", yard)
			}
		}
		fake := f.cli.options.Incus.(*testkit.Incus)
		fake.Instances["subyard-named/yard-named"] = ports.InstanceInfo{Project: "subyard-named", Name: "yard-named", Status: "Running"}
	}
	env := append(slices.Clone(f.cli.options.Environment), "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
	f.cli.options.RepositoryRoot, f.cli.options.Environment = root, env
	platform := newInitPlatformFixture()
	f.cli.options.InitPlatform = platform
	program, err := New(f.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	return program, f.readyMarker, home
}

func TestCompletedSelectionAuthoringPreservesHistoryAndDefersActivation(t *testing.T) {
	for _, arguments := range [][]string{
		{"config", "set", "CODING_TOOL_INTEGRATIONS", "", "--scope", "yard", "--local"},
		{"integration", "disable", "codex"},
		{"config", "set", "ENVIRONMENT_PROFILES", "fixture-neighbor", "--scope", "yard", "--local"},
		{"profile", "disable", "fixture-selected"},
	} {
		t.Run(strings.Join(arguments[:2], "-")+arguments[2], func(t *testing.T) {
			program, ready, home := completedSelectionAuthoringFixture(t)
			path := filepath.Join(home, "yards/default/config.env")
			original := []byte("# retain unrelated setting\nCODING_TOOL_INTEGRATIONS=claude\nENVIRONMENT_PROFILES=fixture-neighbor\nSSH_PORT=2222\n")
			if arguments[0] == "profile" || arguments[2] == "ENVIRONMENT_PROFILES" {
				original = bytes.Replace(original, []byte("CODING_TOOL_INTEGRATIONS=claude"), []byte("CODING_TOOL_INTEGRATIONS=codex"), 1)
			}
			writeProfileTestFile(t, path, original, 0o640)
			writeProfileTestFile(t, filepath.Join(home, config.GitSettingsRelativePath, "config.env"), []byte("CODING_TOOL_INTEGRATIONS='claude codex'\nENVIRONMENT_PROFILES='fixture-selected fixture-neighbor'\n"), 0o600)
			testkit.WriteFile(t, ready, []byte("ready\n"), 0o600)
			if _, err := program.loadSelectionRepair("default"); err != nil {
				t.Fatalf("initial source fixture: %#v", err)
			}
			history := activationRepairHistory(t, program.options.RepositoryRoot, home)
			checkpoint := mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/history-checkpoint.json"))
			run := func(args []string, consent bool) (int, string) {
				t.Helper()
				options := program.options
				var stdout, stderr bytes.Buffer
				options.Arguments = args
				options.Stdout = &stdout
				options.Stderr = &stderr
				options.Prompt = &testkit.Prompt{Answers: []bool{consent}}
				code := runAliasRepairFixture(t, options)
				return code, stdout.String() + stderr.String()
			}
			name := "CODING_TOOL_INTEGRATIONS"
			if arguments[0] == "profile" || arguments[2] == "ENVIRONMENT_PROFILES" {
				name = "ENVIRONMENT_PROFILES"
			}
			if code, output := run([]string{"config", "unset", name, "--scope", "yard", "--local", "--yes"}, true); code != 0 {
				t.Fatalf("ordinary unset: %d %s", code, output)
			}
			if err := os.Remove(ready); err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadSelectionRepair("default")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Environment[name] == "" {
				t.Fatal("unset did not restore inherited nonempty selection")
			}
			fake := program.options.Executor.(*testkit.Incus)
			fake.ExecSteps = nil
			for range 20 {
				appendMismatchedHashSteps(t, fake, loaded, "0")
			}
			before := mustReadConfigFixture(t, path)
			if code, output := run(arguments, false); code == 0 {
				t.Fatalf("declined reduction applied: %s", output)
			}
			if !bytes.Equal(before, mustReadConfigFixture(t, path)) {
				t.Fatal("decline changed persistent selection")
			}
			if code, output := run(append(slices.Clone(arguments), "--yes"), true); code != 0 {
				t.Fatalf("completed source reduction: %d %s", code, output)
			} else if !strings.Contains(output, "config.set") || strings.Contains(output, "reduce-selection") {
				t.Fatalf("not ordinary source authoring: %s", output)
			}
			after := mustReadConfigFixture(t, path)
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o640 || !bytes.Contains(after, []byte("# retain unrelated setting\n")) || !bytes.Contains(after, []byte("SSH_PORT=2222\n")) {
				t.Fatal("source authoring changed unrelated bytes or mode")
			}
			if !reflect.DeepEqual(history, activationRepairHistory(t, program.options.RepositoryRoot, home)) || !bytes.Equal(checkpoint, mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/history-checkpoint.json"))) {
				t.Fatal("ordinary source write changed release history or links")
			}
			if _, err := os.Stat(ready); !os.IsNotExist(err) {
				t.Fatal("source authoring materialized intermediate expansion")
			}
			platform := program.options.InitPlatform.(*initPlatformFixture)
			if len(platform.applied) != 0 || platform.projectHooks != 0 {
				t.Fatal("source authoring ran hooks/runtime reconciliation")
			}
			resolved, err := program.loadSelectionRepair("default")
			if err != nil {
				t.Fatal(err)
			}
			desired := ""
			if name == "ENVIRONMENT_PROFILES" {
				desired = "fixture-neighbor"
			} else if arguments[0] == "integration" {
				desired = "claude"
			}
			if resolved.Environment[name] != desired {
				t.Fatalf("desired source %q want %q", resolved.Environment[name], desired)
			}
		})
	}
}

func TestCompletedSelectionAuthoringRejectsExpansionAndStaleAuthority(t *testing.T) {
	for _, scenario := range []string{"expansion", "legacy fallback", "temporary", "permissions", "other activation", "unfinished", "source after consent", "history after consent", "checkpoint after consent", "tooling after consent"} {
		t.Run(scenario, func(t *testing.T) {
			program, _, home := completedSelectionAuthoringFixture(t)
			path := filepath.Join(home, "yards/default/config.env")
			before := []byte("CODING_TOOL_INTEGRATIONS=codex\nENVIRONMENT_PROFILES=fixture-selected\nSSH_PORT=2222\n")
			writeProfileTestFile(t, path, before, 0o640)
			loaded, err := program.loadSelectionRepair("default")
			if err != nil {
				t.Fatal(err)
			}
			options := program.options
			// Exercise the newly admitted current-file boundary without changing
			// any of the source, history or consent rejection cases.
			options.Executor = &selectionOnlyIntegrationExecutor{source: loaded.Environment["AGENT_codex_RULES"], serviceDrift: true}
			options.Arguments = []string{"config", "set", "CODING_TOOL_INTEGRATIONS", "", "--scope", "yard", "--local"}
			switch scenario {
			case "expansion":
				options.Arguments[3] = "codex claude"
			case "legacy fallback":
				options.Arguments[2] = "ENVIRONMENT_PROFILES"
			case "temporary":
				options.Environment = append(slices.Clone(options.Environment), "CODING_TOOL_INTEGRATIONS=codex")
			case "permissions":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "other activation":
				options.InitPlatform = &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &ports.RuntimeObservation{State: "stale", Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}}
			case "unfinished":
				journalPath := filepath.Join(home, "release-transition/v2/journal.json")
				journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, journalPath))
				if err != nil {
					t.Fatal(err)
				}
				journal.Checkpoint = releasetransition.JournalMigrating
				payload, err := releasetransition.MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, journalPath, payload, 0o600)
			}
			prompt := &callbackPrompt{callback: func() {
				switch scenario {
				case "source after consent":
					writeProfileTestFile(t, filepath.Join(home, config.GitSettingsRelativePath, "config.env"), []byte("CODING_TOOL_INTEGRATIONS='codex claude'\n"), 0o600)
				case "history after consent":
					journalPath := filepath.Join(home, "release-transition/v2/journal.json")
					testkit.WriteFile(t, journalPath, append(mustReadConfigFixture(t, journalPath), '\n'), 0o600)
				case "checkpoint after consent":
					checkpoint := filepath.Join(home, "release-transition/v2/history-checkpoint.json")
					testkit.WriteFile(t, checkpoint, append(mustReadConfigFixture(t, checkpoint), '\n'), 0o600)
				case "tooling after consent":
					if err := os.Chmod(filepath.Join(options.RepositoryRoot, "config/commands.registry"), 0o640); err != nil {
						t.Fatal(err)
					}
				}
			}}
			options.Prompt = prompt
			var stdout, stderr bytes.Buffer
			options.Stdout = &stdout
			options.Stderr = &stderr
			if code := runAliasRepairFixture(t, options); code == 0 {
				t.Fatalf("unsafe completed source write admitted: %s %s", stdout.String(), stderr.String())
			}
			if !bytes.Equal(before, mustReadConfigFixture(t, path)) {
				t.Fatal("rejected source write changed selection")
			}
			if strings.Contains(scenario, "after consent") && len(prompt.requests) != 1 {
				t.Fatalf("did not reach bound consent guard: prompts=%d %s", len(prompt.requests), stderr.String())
			}
			if platform, ok := options.InitPlatform.(*initPlatformFixture); ok && (len(platform.applied) != 0 || platform.projectHooks != 0) {
				t.Fatal("rejected source write ran activation")
			}
		})
	}
}

// Retain the native integration observer; only physical guest reads are synthetic.
// Hashes are current unless the contrast explicitly requests file drift.
type selectionOnlyIntegrationExecutor struct {
	source                                   string
	staleFiles, serviceDrift, missingCommand bool
	mutations, serviceObservations           int
}

func (executor *selectionOnlyIntegrationExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := strings.Join(request.Command, "\x00")
	if len(request.Command) > 4 && request.Command[0] == "python3" && strings.Contains(request.Command[3], "Version 1 records observed ownership only") {
		if request.Command[4] != "observe" {
			executor.mutations++
			return ports.InstanceExecResult{}, errors.New("unexpected inventory mutation")
		}
		payload, _ := json.Marshal(map[string]any{"fingerprint": strings.Repeat("a", 64), "changed": false, "initial": false})
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if strings.Contains(command, "mode=$1") && strings.Contains(command, "changed=0") {
		if len(request.Command) > 5 && request.Command[5] == "apply" {
			executor.mutations++
			return ports.InstanceExecResult{}, errors.New("unexpected service apply")
		}
		executor.serviceObservations++
		value := "changed=0\n"
		if executor.serviceDrift {
			value = "changed=1\n"
		}
		return ports.InstanceExecResult{Stdout: []byte(value)}, nil
	}
	if strings.Contains(command, "exec timeout --kill-after=5s 120s") {
		executor.mutations++
		return ports.InstanceExecResult{}, errors.New("unexpected integration hook")
	}
	if len(request.Command) > 2 && request.Command[0] == "sha256sum" {
		content, err := os.ReadFile(executor.source)
		if err != nil {
			return ports.InstanceExecResult{}, err
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(content))
		if executor.staleFiles {
			hash = strings.Repeat("0", 64)
		}
		return ports.InstanceExecResult{Stdout: []byte(hash + "  " + request.Command[2] + "\n")}, nil
	}
	if executor.missingCommand && slices.Contains(request.Command, "synthetic-cli") {
		return ports.InstanceExecResult{ExitCode: 1}, errors.New("synthetic integration command is not installed")
	}
	return ports.InstanceExecResult{}, nil
}

func TestCompletedSelectionAuthoringAdmitsIntegrationOnlyDriftWithCurrentFiles(t *testing.T) {
	for _, history := range []string{"activation-only", "verified-ssh"} {
		for _, drift := range []string{"files", "service-only", "provision-only"} {
			for _, verb := range []string{"set", "disable"} {
				t.Run(history+"/"+drift+"/"+verb, func(t *testing.T) {
					program, ready, home := completedSelectionAuthoringFixture(t, history == "verified-ssh")
					path := filepath.Join(home, "yards/default/config.env")
					port := "2222"
					if history == "verified-ssh" {
						port = "2240"
					}
					before := []byte("# retain selection intent\nCODING_TOOL_INTEGRATIONS=codex\nSSH_PORT=" + port + "\n")
					writeProfileTestFile(t, path, before, 0o640)
					gitPath := filepath.Join(home, config.GitSettingsRelativePath, "config.env")
					writeProfileTestFile(t, gitPath, []byte("SSH_PORT="+port+"\nCODING_TOOL_INTEGRATIONS='codex claude'\n"), 0o600)
					testkit.WriteFile(t, ready, []byte("ready\n"), 0o600)
					options := program.options
					var output bytes.Buffer
					options.Stdout = &output
					options.Stderr = &output
					options.Arguments = []string{"config", "unset", "CODING_TOOL_INTEGRATIONS", "--scope", "yard", "--local", "--yes"}
					if code := runAliasRepairFixture(t, options); code != 0 {
						t.Fatalf("ordinary native unset: %d %s", code, output.String())
					}
					if err := os.Remove(ready); err != nil {
						t.Fatal(err)
					}
					loaded, err := program.loadSelectionRepair("default")
					if err != nil {
						t.Fatal(err)
					}
					fake := options.Incus.(*testkit.Incus)
					fake.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: fake.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]}
					physical := &selectionOnlyIntegrationExecutor{source: loaded.Environment["AGENT_codex_RULES"], staleFiles: drift == "files", serviceDrift: drift != "provision-only", missingCommand: drift == "provision-only"}
					native := *program
					native.options.Executor = physical
					native.options.InitPlatform = reconcileruntime.Runtime{RepositoryRoot: options.RepositoryRoot, Executor: physical, Incus: options.Incus, Yard: loaded.Context, Environment: environmentList(loaded.Environment, nil)}
					assessment, err := native.assessConfigTarget(context.Background(), configTarget{Name: "default", Loaded: loaded}, true)
					if err != nil || assessment.Changed != (drift == "files") {
						t.Fatalf("file contrast: %#v %v", assessment, err)
					}
					actor := &materializedConfigActivationReconciler{cli: &native, yard: "default", configHome: home, scopeResolved: true}
					observation, err := actor.Observe(context.Background(), releasetransition.ReleasePair{From: "1.2.3-aaaaaaaaaaaa", Target: "1.2.3-aaaaaaaaaaaa"}, releasetransition.ReleaseLinks{Active: "1.2.3-aaaaaaaaaaaa"})
					if err != nil || observation.Converged || !actor.integrationPlans["default"].Changed || physical.serviceObservations == 0 {
						t.Fatalf("native integration-only activation not pending: %#v %v", observation, err)
					}
					apply := *program
					apply.options.Executor = physical
					gate, err := apply.inspectMutationGate(context.Background(), "default")
					if err != nil || gate == nil {
						t.Fatalf("completed native mutation gate missing: %#v %v", gate, err)
					}
					permit, err := apply.prepareConfigApplyRepair(context.Background(), "default", true, *gate)
					if err != nil || (permit != nil) != (drift == "files") {
						t.Fatalf("ordinary config apply file-drift guard changed: admitted=%t err=%v", permit != nil, err)
					}
					if drift != "files" {
						blocked := options
						blocked.Executor = physical
						blocked.Config = &recordingConfigApplier{}
						blocked.Arguments = []string{"config", "apply", "--all-local", "--yes"}
						output.Reset()
						if code := runAliasRepairFixture(t, blocked); code == 0 || len(blocked.Config.(*recordingConfigApplier).yards) != 0 {
							t.Fatalf("ordinary config apply admitted current-file integration drift: %d %s", code, output.String())
						}
					}
					options.Executor = physical
					output.Reset()
					historyBefore := activationRepairHistory(t, options.RepositoryRoot, home)
					checkpoint := mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/history-checkpoint.json"))
					options.Arguments = []string{"config", "set", "CODING_TOOL_INTEGRATIONS", "", "--scope", "yard", "--local", "--yes"}
					desired := ""
					if verb == "disable" {
						options.Arguments = []string{"integration", "disable", "claude", "--yes"}
						desired = "codex"
					}
					if code := runAliasRepairFixture(t, options); code != 0 {
						t.Fatalf("completed source-only %s with %s: %d %s", verb, drift, code, output.String())
					}
					after, err := program.loadSelectionRepair("default")
					if err != nil || after.Environment["CODING_TOOL_INTEGRATIONS"] != desired {
						t.Fatalf("final desired selection: %q %v", after.Environment["CODING_TOOL_INTEGRATIONS"], err)
					}
					if !reflect.DeepEqual(historyBefore, activationRepairHistory(t, options.RepositoryRoot, home)) || !bytes.Equal(checkpoint, mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/history-checkpoint.json"))) || physical.mutations != 0 {
						t.Fatal("source-only reduction mutated history or native integrations")
					}
					info, err := os.Stat(path)
					content := mustReadConfigFixture(t, path)
					if err != nil || info.Mode().Perm() != 0o640 || !bytes.Contains(content, []byte("# retain selection intent\n")) || !bytes.Contains(content, []byte("SSH_PORT="+port+"\n")) {
						t.Fatal("source-only reduction changed unrelated state or modes")
					}
				})
			}
		}
	}
}
