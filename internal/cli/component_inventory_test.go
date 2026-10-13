package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ownerapi"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileListCLIIncludesImplicitEmptyAndForbiddenCatalogEntries(t *testing.T) {
	for _, selection := range []string{"absent", "empty", "explicit"} {
		t.Run(selection, func(t *testing.T) {
			handler, runner, incus := ownerQueryFixture(t)
			root := handler.cli.options.RepositoryRoot
			manifest := filepath.Join(root, "config/commands.registry")
			data := mustReadConfigFixture(t, manifest)
			writeCLIFile(t, manifest, string(data)+"profile||@profile||deny|mutate|dynamic|public|lifecycle|profile|profile <command> [id]|profiles|--json --yes --help|list enable disable setup status\n", 0o600)
			home := handler.loaded.Context.Paths.ConfigHome
			if selection != "absent" {
				value := ""
				if selection == "explicit" {
					value = "selected resource-only"
				}
				writeConfigCommandFile(t, filepath.Join(home, "config.env"), "ENVIRONMENT_PROFILES='"+value+"'\n")
				writeConfigCommandFile(t, filepath.Join(home, config.GitSettingsRelativePath, "config.env"), "ENVIRONMENT_PROFILES=legacy\n")
			}
			for _, structured := range []bool{false, true} {
				var stdout, stderr bytes.Buffer
				arguments := []string{"profile", "list"}
				if structured {
					arguments = append(arguments, "--json")
				}
				program, err := New(Options{RepositoryRoot: root, Environment: handler.cli.options.Environment, Arguments: arguments, Incus: incus, AdapterRunner: runner, Stdout: &stdout, Stderr: &stderr})
				if err != nil {
					t.Fatal(err)
				}
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("CLI list: %d %s", code, stderr.String())
				}
				if len(runner.Requests) != 0 {
					t.Fatal("catalog query ran a profile hook")
				}
				if !structured {
					for _, name := range []string{"selected", "legacy", "dedicated", "resource-only"} {
						if !strings.Contains(stdout.String(), "  "+name+":") {
							t.Fatalf("human inventory omitted %s: %s", name, stdout.String())
						}
					}
					continue
				}
				var list struct {
					ownerapi.ProfileList
					Source     config.SettingResolution   `json:"source"`
					Overridden []config.SettingResolution `json:"overridden"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
					t.Fatal(err)
				}
				if len(list.Profiles) != 4 || list.Selection.Present != (selection != "absent") {
					t.Fatalf("full catalog/absence lost: %#v", list)
				}
				for _, entry := range list.Profiles {
					if entry.Name == "dedicated" && (entry.Eligible || entry.Effective || entry.Reason != "role-restricted") {
						t.Fatalf("forbidden entry missing reason: %#v", entry)
					}
					if selection == "empty" {
						if entry.Requested || entry.Effective != (entry.Name == "legacy") {
							t.Fatalf("explicit empty provision fallback differs from inventory: %#v", entry)
						}
						if entry.Name == "legacy" && entry.Reason != "legacy-provision-fallback" {
							t.Fatalf("legacy fallback unexplained: %#v", entry)
						}
					}
					if selection == "absent" && entry.Name == "legacy" && (!entry.Effective || entry.Reason != "implicit-provision-default") {
						t.Fatalf("implicit default unexplained: %#v", entry)
					}
				}
				if selection != "absent" && (list.Source.Path != filepath.Join(home, "config.env") || len(list.Overridden) == 0) {
					t.Fatal("winning/overridden provenance missing")
				}
				if selection == "empty" {
					fresh, err := New(Options{RepositoryRoot: root, Environment: handler.cli.options.Environment, Incus: incus, AdapterRunner: runner})
					if err != nil {
						t.Fatal(err)
					}
					loaded, err := fresh.loadContext("default")
					if err != nil {
						t.Fatal(err)
					}
					execution, err := fresh.prepareProvisionExecution(loaded, nil, nil)
					if err != nil || len(execution.profiles) != 1 || execution.profiles[0] != "legacy" {
						t.Fatalf("legacy fallback execution differs: %#v %v", execution, err)
					}
					if !strings.Contains(loaded.EnvironmentProfileSelectionExplanation(), "legacy provision fallback") {
						t.Fatal("empty-selection plan hides the legacy fallback")
					}
				}
			}
			if _, err := os.Lstat(filepath.Join(home, "migrations")); !os.IsNotExist(err) {
				t.Fatal("inventory created mutation bookkeeping")
			}
		})
	}
}

func TestIntegrationStatusCLIUsesDependencyRoleAndCommandSourceSnapshot(t *testing.T) {
	for _, scenario := range []string{"local", "command", "role"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, _, runtime, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=sample\n")
			root := fixture.options.RepositoryRoot
			testkit.WriteFile(t, filepath.Join(root, "config/agents.env"), []byte("AGENT_sample_COMMAND=sample\nAGENT_sample_DEPENDS=base\nAGENT_base_COMMAND=base\nAGENT_other_COMMAND=other\n"), 0o600)
			gitPath := filepath.Join(root, "state", config.GitSettingsRelativePath, "config.env")
			writeProfileTestFile(t, gitPath, []byte("CODING_TOOL_INTEGRATIONS=sample\n"), 0o600)
			options := fixture.options
			group, scope := "local overrides", "yard"
			sourcePath := path
			if scenario == "command" {
				options.Environment = append(slices.Clone(options.Environment), "CODING_TOOL_INTEGRATIONS=sample")
				group, scope = "command override", "command"
				sourcePath = "environment"
			}
			if scenario == "role" {
				writeProfileTestFile(t, filepath.Join(root, "config/yards/profiles/fixture-restricted.env"), []byte("ALLOWS_CODING_TOOLS=false\n"), 0o600)
				testkit.WriteFile(t, path, []byte("YARD_TEMPLATE=fixture-restricted\n"), 0o600)
				group, scope = "Git settings", "host"
				sourcePath = gitPath
			}
			before := mustReadConfigFixture(t, path)
			runtime.plan.Changed = false
			var snapshots []config.IntegrationSelection
			options.IntegrationRuntime = func(loaded config.Loaded) IntegrationRuntime {
				snapshots = append(snapshots, loaded.Integrations)
				return runtime
			}
			var status integrationStatus
			for _, structured := range []bool{true, false} {
				var stdout, stderr bytes.Buffer
				options.Arguments = []string{"integration", "status", "other"}
				if structured {
					options.Arguments = append(options.Arguments, "--json")
				}
				options.Stdout, options.Stderr = &stdout, &stderr
				program, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("CLI status code=%d %s", code, stderr.String())
				}
				if !structured {
					wantEffective := "base sample"
					if scenario == "role" {
						wantEffective = ""
					}
					if !strings.Contains(stdout.String(), "Requested: sample\nEffective: "+wantEffective+"\n") || !strings.Contains(stdout.String(), "Observed (yard): ready\n") {
						t.Fatalf("human desired/yard observation differs: %s", stdout.String())
					}
					for _, entry := range status.Inventory {
						want := fmt.Sprintf("  %s: requested=%t effective=%t eligible=%t reason=%s; %s (%s, %s:%d); runtime=not-inspected", entry.ID, entry.Requested, entry.Effective, entry.Eligible, entry.Reason, group, scope, entry.Source.Path, entry.Source.Line)
						if !strings.Contains(stdout.String(), want) {
							t.Fatalf("human catalog or provenance differs for %s: %s", entry.ID, stdout.String())
						}
					}
					continue
				}
				if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if len(status.Inventory) != 3 || !status.Selection.Present || !slices.Equal(status.Selection.Requested, []string{"sample"}) || status.Observed != "ready" || status.ObservedScope != "yard" || len(status.Health) != 0 {
					t.Fatalf("JSON lost catalog or desired/runtime separation: %#v", status)
				}
				if status.Selection.Provenance.Scope != scope || status.Selection.Provenance.Path != sourcePath || scenario == "role" && (!status.Selection.Suppressed || len(status.Selection.Effective) != 0) || scenario != "role" && !slices.Equal(status.Selection.Effective, []string{"base", "sample"}) {
					t.Fatalf("JSON resolver snapshot differs: %#v", status.Selection)
				}
				for _, entry := range status.Inventory {
					requested, effective, eligible, reason := entry.ID == "sample", entry.ID != "other", true, "explicit-selection"
					if entry.ID == "base" {
						reason = "dependency"
						if scenario != "role" && !slices.Equal(entry.Dependencies, []string{"sample"}) {
							t.Fatalf("dependency reason missing: %#v", entry)
						}
					}
					if entry.ID == "other" {
						reason = "excluded-by-replace-list"
					}
					if scenario == "role" {
						effective, eligible, reason = false, false, "role-restricted"
					}
					if entry.Requested != requested || entry.Effective != effective || entry.Eligible != eligible || entry.Reason != reason || entry.SourceGroup != group || !reflect.DeepEqual(entry.Source, status.Selection.Provenance) || len(entry.Overridden) == 0 || entry.Observed != "not-inspected" {
						t.Fatalf("JSON component selection/provenance differs: %#v", entry)
					}
					if scenario != "role" && !slices.ContainsFunc(entry.Overridden, func(source config.SettingResolution) bool { return source.Path == gitPath && source.Scope == "host" }) {
						t.Fatalf("overridden Git source missing: %#v", entry.Overridden)
					}
				}
			}
			if len(snapshots) != 2 || !reflect.DeepEqual(snapshots[0], snapshots[1]) || !reflect.DeepEqual(status.Selection, snapshots[0]) || runtime.applied != 0 || !bytes.Equal(before, mustReadConfigFixture(t, path)) {
				t.Fatal("human/JSON query and runtime did not share the same read-only selection")
			}
		})
	}
}

func TestIntegrationInventoryUsesEffectiveDependencyAndRoleSnapshot(t *testing.T) {
	cli, _, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=sample\n")
	writeCLIFile(t, filepath.Join(cli.options.RepositoryRoot, "config/agents.env"), "AGENT_sample_COMMAND=sample\nAGENT_sample_DEPENDS=base\nAGENT_base_COMMAND=base\nAGENT_other_COMMAND=other\n", 0o600)
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	entries := loaded.IntegrationInventory()
	if len(entries) != 3 {
		t.Fatalf("inventory size=%d", len(entries))
	}
	for _, entry := range entries {
		if entry.ID == "base" && (entry.Requested || !entry.Effective || entry.Reason != "dependency" || len(entry.Dependencies) != 1) {
			t.Fatalf("dependency unexplained: %#v", entry)
		}
	}
	loaded.Integrations.AllowsCodingTools = false
	loaded.Integrations.Effective = nil
	for _, entry := range loaded.IntegrationInventory() {
		if entry.Effective || entry.Eligible || entry.Reason != "role-restricted" {
			t.Fatalf("role suppression lost: %#v", entry)
		}
	}
}
