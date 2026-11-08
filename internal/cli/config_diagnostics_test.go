package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type diagnosticStatusPlatform struct {
	*appliedSelectionPlatformFixture
}

func TestConfigContentDifferenceKeepsSensitiveValuesOutOfRows(t *testing.T) {
	for _, test := range []struct {
		name             string
		file, desiredSet bool
		before, after    string
		kind             domain.ConfigurationDifferenceKind
	}{
		{"removed scalar", false, false, "installed-digest", "", domain.ConfigurationSensitive},
		{"removed file", true, false, "installed-digest", "", domain.ConfigurationSensitive},
		{"changed scalar", false, true, "installed-digest", "desired-digest", domain.ConfigurationSensitive},
		{"unreadable file", true, true, "installed-digest", "", domain.ConfigurationContentUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := configContentDifference(domain.ConfigurationDifference{
				Name: "SECRET", Installed: "private-before", Desired: "private-after", InstalledSet: true, DesiredSet: test.desiredSet,
			}, config.SettingDefinition{Sensitive: true}, test.file, test.before, test.after)
			if row.Kind != test.kind || row.Installed != "" || row.Desired != "" {
				t.Fatalf("sensitive row: %#v", row)
			}
			if test.kind == domain.ConfigurationSensitive && row.Value() != "changed" {
				t.Fatalf("known sensitive difference = %q", row.Value())
			}
		})
	}
}

func (*diagnosticStatusPlatform) ObserveProfileRuntimeDiagnostics(context.Context) (map[string]ports.RuntimeObservation, error) {
	diagnostics := ports.RuntimeDiagnostics{{Parameter: "sample.service", Installed: "inactive", Desired: "active"}}
	return map[string]ports.RuntimeObservation{"sample-runtime": {State: ports.RuntimeStateStale, Diagnostics: &diagnostics}}, nil
}

func TestConfigSettingDiagnosticUsesAppliedSnapshot(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	loaded.Environment["DEV_USER"] = "next"
	platform := &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment)}
	platform.settings["DEV_USER"] = "dev"
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Status: "Running"}}}
	cli := &CLI{options: Options{RepositoryRoot: root, InitPlatform: platform, Incus: incus}}
	diagnostic, err := cli.configInstalledDiagnostic(context.Background(), configTarget{Name: "default", Loaded: loaded}, "running")
	if err != nil {
		t.Fatal(err)
	}
	got := domain.FormatConfigurationDiagnostics([]domain.YardConfigurationDiagnostic{diagnostic})
	if !strings.Contains(got, "DEV_USER  dev -> next") {
		t.Fatalf("missing applied difference: %s", got)
	}
}

func TestConfigAssessmentKeepsIndependentDriftAfterSourceFailure(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	loaded.Environment["CODING_TOOL_INTEGRATIONS"] = "claude codex"
	loaded.Environment["AGENT_claude_CONFIG"] = t.TempDir()
	source := filepath.Join(t.TempDir(), "config")
	writeConfigCommandFile(t, source, "model = 'example'\n")
	loaded.Environment["AGENT_codex_CONFIG"] = source
	assets, err := effectiveConfigAssets(loaded)
	if err != nil {
		t.Fatal(err)
	}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Status: "Running"}}}
	for _, asset := range assets {
		if asset.Source == loaded.Environment["AGENT_claude_CONFIG"] {
			continue
		}
		output := []byte(strings.Repeat("0", 64) + "  " + asset.Destination + "\n")
		if asset.OwnedFormat != "" {
			output = []byte(`{"converged":false,"fingerprint":"` + strings.Repeat("0", 64) + `"}`)
		}
		incus.ExecSteps = append(incus.ExecSteps, testkit.IncusExecStep{Result: ports.InstanceExecResult{Stdout: output}})
	}
	var stdout, stderr bytes.Buffer
	platform := &diagnosticStatusPlatform{&appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment), selected: loaded.Integrations.Effective}}
	platform.settings["DEV_USER"] = "before"
	platform.settings["AGENT_codex_COMMAND"] = "before-command"
	program := &CLI{options: Options{RepositoryRoot: root, Incus: incus, Executor: incus, InitPlatform: platform, Stdout: &stdout, Stderr: &stderr}}
	err = program.configStatus(context.Background(), []configTarget{{Name: "default", Loaded: loaded}}, true)
	if err == nil || !strings.Contains(err.Error(), "claude.config") {
		t.Fatalf("missing read failure: %v", err)
	}
	if !strings.Contains(stdout.String(), "AGENT_codex_CONFIG") || !strings.Contains(stdout.String(), "DEV_USER  before ->") {
		t.Fatalf("independent drift missing: %s", stdout.String())
	}
}

func TestConfigInstalledDiagnosticShowsAppliedEnvironmentProfiles(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	loaded.EnvironmentProfiles.Effective = []string{"sample"}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Status: "Running"}}}
	program := &CLI{options: Options{Incus: incus, RepositoryRoot: root, InitPlatform: &appliedProfilesPlatformFixture{appliedSelectionPlatformFixture: &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment)}, known: true}}}
	diagnostic, err := program.configInstalledDiagnostic(context.Background(), configTarget{Name: "default", Loaded: loaded}, "running")
	if err != nil {
		t.Fatal(err)
	}
	message := domain.FormatConfigurationDiagnostics([]domain.YardConfigurationDiagnostic{diagnostic})
	if !strings.Contains(message, "ENVIRONMENT_PROFILES  (empty) -> sample") {
		t.Fatalf("profile selection missing: %s", message)
	}
}

func TestConfigStatusPrintsAppliedSettingDifferences(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	settings := loaded.Catalog.IntegrationInitSettings(loaded.Environment)
	settings["DEV_USER"] = "old"
	platform := &diagnosticStatusPlatform{&appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), settings: settings, selected: loaded.Integrations.Effective}}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running"}}}
	appendHashSteps(t, incus, loaded)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, Arguments: []string{"config", "status"}, InitPlatform: platform, Incus: incus, Executor: incus, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("status %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "yard default:") || !strings.Contains(stdout.String(), "DEV_USER  old -> "+loaded.Environment["DEV_USER"]) {
		t.Fatalf("status did not print setting difference: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "sample.service  inactive -> active") {
		t.Fatalf("status omitted profile diagnostic: %s", stdout.String())
	}
}

func TestMaterializationReportsIndependentSourceFailures(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	loaded.Environment["CODING_TOOL_INTEGRATIONS"] = "claude codex"
	loaded.Environment["AGENT_claude_CONFIG"] = "/unavailable-one"
	loaded.Environment["AGENT_codex_CONFIG"] = "/unavailable-two"
	_, _, _, err := materializationDesired(configTarget{Name: "default", Loaded: loaded}, true, func(asset config.MaterializedAsset) ([]byte, error) {
		return nil, fmt.Errorf("cannot read %s", asset.Source)
	})
	if err == nil || !strings.Contains(err.Error(), "claude.config") || !strings.Contains(err.Error(), "codex.config") {
		t.Fatalf("independent sources: %v", err)
	}
}

func TestConfigSettingDiagnosticReportsSamePathContentChange(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded.Environment["AGENT_sample_PROVISION"] = path
	loaded.Integrations.Effective = []string{"sample"}
	key := bytes.Repeat([]byte{7}, 32)
	applied := reconcileruntime.AppliedIntegrationConfiguration{Settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment), Diagnostics: &reconcileruntime.IntegrationDiagnosticEvidence{Key: hex.EncodeToString(key), Digests: loaded.Catalog.IntegrationDiagnosticDigests(loaded.Environment, key, nil)}}
	if err := os.WriteFile(path, []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}
	message := domain.FormatConfigurationDiagnostics([]domain.YardConfigurationDiagnostic{configContentDiagnostics(configTarget{Name: "default", Loaded: loaded}, applied)})
	if !strings.Contains(message, "AGENT_sample_PROVISION  content changed") || strings.Contains(message, "old content") || strings.Contains(message, "new content") {
		t.Fatalf("unexpected diagnostic %q", message)
	}
}

func TestConfigSettingDiagnosticReportsCommittedConfigSourceChange(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded.Environment["CODING_TOOL_INTEGRATIONS"] = "claude"
	loaded.Environment["AGENT_claude_CONFIG"] = path
	key := bytes.Repeat([]byte{8}, 32)
	definition, ok := loaded.Catalog.LookupSetting("AGENT_claude_CONFIG")
	if !ok {
		t.Fatal("missing setting")
	}
	applied := reconcileruntime.AppliedIntegrationConfiguration{Settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment), Diagnostics: &reconcileruntime.IntegrationDiagnosticEvidence{Key: hex.EncodeToString(key), Digests: loaded.Catalog.IntegrationDiagnosticDigests(loaded.Environment, key, nil), Files: map[string]reconcileruntime.IntegrationDiagnosticFile{"AGENT_claude_CONFIG": {Path: path, Digest: config.ConfigurationInputDigest("AGENT_claude_CONFIG", definition.Kind, key, []byte("original"))}}}}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	message := domain.FormatConfigurationDiagnostics([]domain.YardConfigurationDiagnostic{configContentDiagnostics(configTarget{Name: "default", Loaded: loaded}, applied)})
	if !strings.Contains(message, "AGENT_claude_CONFIG  content changed") {
		t.Fatalf("unexpected diagnostic %q", message)
	}
}
