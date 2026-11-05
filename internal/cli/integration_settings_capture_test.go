package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type settingsCaptureFixture struct {
	calls int
	apply func() error
}

func (*settingsCaptureFixture) Binding() string { return strings.Repeat("a", 64) }
func (capture *settingsCaptureFixture) Apply(context.Context) error {
	capture.calls++
	if capture.apply != nil {
		return capture.apply()
	}
	return nil
}

type settingsCapturePlatformFixture struct {
	*initPlatformFixture
	capture  *settingsCaptureFixture
	prepares int
	err      error
}

func (platform *settingsCapturePlatformFixture) PrepareAppliedIntegrationSettings(context.Context) (ports.PreparedSettingsCapture, error) {
	platform.prepares++
	return platform.capture, platform.err
}

func TestLegacyIntegrationSettingsCapturedBeforeDesiredWrite(t *testing.T) {
	for _, scenario := range []string{"capture", "stale", "stopped", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			cli := fixture.cli
			incus := cli.options.Incus.(*testkit.Incus)
			loaded, err := cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "default", "config.env")
			writeConfigCommandFile(t, path, "CODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_COMMAND=old-command\n", 0o600)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			capture := &settingsCaptureFixture{apply: func() error {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, before) {
					t.Fatal("desired configuration changed before baseline capture")
				}
				if scenario == "stale" {
					return domain.ErrPlanStale
				}
				return nil
			}}
			platform := &settingsCapturePlatformFixture{initPlatformFixture: newInitPlatformFixture(), capture: capture}
			if scenario == "conflict" {
				platform.err = errors.New("owned artifact conflict")
			}
			cli.options.InitPlatform = platform
			if scenario == "stopped" {
				instance := incus.Instances["subyard/yard"]
				instance.Status = "Stopped"
				incus.Instances["subyard/yard"] = instance
			}
			loaded, err = cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition := nativeConfigCommandDefinition()
			prepared, err := cli.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{"set", "AGENT_codex_COMMAND", "new-command", "--scope", "yard"}})
			if scenario == "stopped" || scenario == "conflict" {
				if err == nil {
					prepared.Close()
					t.Fatal("unverifiable applied settings accepted")
				}
				if capture.calls != 0 {
					t.Fatal("refused authoring captured settings")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if capture.calls != 0 {
				t.Fatal("preparation wrote the applied baseline")
			}
			authorizePreparedFixture(prepared)
			_, err = prepared.Execute(context.Background(), nil, io.Discard)
			if scenario == "stale" {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("stale baseline authoring: %v", err)
				}
				actual, _ := os.ReadFile(path)
				if !bytes.Equal(actual, before) {
					t.Fatal("stale baseline changed desired config")
				}
				return
			}
			if err != nil || capture.calls != 1 {
				t.Fatalf("capture/write failed: %v calls=%d", err, capture.calls)
			}
			actual, _ := os.ReadFile(path)
			if !strings.Contains(string(actual), "new-command") {
				t.Fatal("new desired configuration was not written")
			}
		})
	}
}

func TestHostIntegrationSettingsCaptureSkipsUnaffectedStoppedYard(t *testing.T) {
	cli, incus, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "neighbor", "config.env")
	writeConfigCommandFile(t, path, "SSH_PORT=2224\nCODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_COMMAND=neighbor-command\n", 0o600)
	incus.Instances["subyard-neighbor/yard-neighbor"] = ports.InstanceInfo{Status: "Stopped"}
	platform := &settingsCapturePlatformFixture{initPlatformFixture: newInitPlatformFixture(), capture: &settingsCaptureFixture{}}
	cli.options.InitPlatform = platform
	request := configAuthoringRequest{name: "AGENT_codex_COMMAND", scope: config.ScopeHost}
	target, err := configScalarAuthoringPath(loaded, config.ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	prospective, err := cli.prepareConfigProspective(loaded, request, target, config.PersistentFileSnapshot{Exists: true, Content: []byte("AGENT_codex_COMMAND=new-command\n")})
	if err != nil {
		t.Fatal(err)
	}
	defer prospective.close()
	captures, err := cli.prepareIntegrationSettingsCaptures(context.Background(), loaded, request, prospective)
	if err != nil || len(captures) != 1 || captures[0].loaded.Context.YardName != "default" {
		t.Fatalf("affected yard capture: %#v %v", captures, err)
	}
}

func TestHostIntegrationSettingsCaptureReportsAllStoppedYards(t *testing.T) {
	cli, incus, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "neighbor", "config.env"), "SSH_PORT=2224\nCODING_TOOL_INTEGRATIONS=codex\n", 0o600)
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Stopped"
	incus.Instances["subyard/yard"] = instance
	incus.Instances["subyard-neighbor/yard-neighbor"] = ports.InstanceInfo{Status: "Stopped"}
	cli.options.InitPlatform = &settingsCapturePlatformFixture{initPlatformFixture: newInitPlatformFixture(), capture: &settingsCaptureFixture{}}
	request := configAuthoringRequest{name: "AGENT_codex_COMMAND", scope: config.ScopeHost}
	path, err := configScalarAuthoringPath(loaded, config.ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	prospective, err := cli.prepareConfigProspective(loaded, request, path, config.PersistentFileSnapshot{Exists: true, Content: []byte("AGENT_codex_COMMAND=new-command\n")})
	if err != nil {
		t.Fatal(err)
	}
	defer prospective.close()
	_, err = cli.prepareIntegrationSettingsCaptures(context.Background(), loaded, request, prospective)
	if err == nil || !strings.Contains(err.Error(), "yard default:") || !strings.Contains(err.Error(), "yard neighbor:") || strings.Index(err.Error(), "yard default:") > strings.Index(err.Error(), "yard neighbor:") {
		t.Fatalf("independent refusal diagnostics: %v", err)
	}
}

type unavailableIdentityPlatform struct {
	*settingsCapturePlatformFixture
}

func (unavailableIdentityPlatform) CheckAppliedGuestIdentity(context.Context) error {
	return errors.New("applied developer identity cannot be verified")
}
func TestIdentityAuthoringRefusesUnverifiableAppliedOwner(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	loaded, err := fixture.cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	platform := &settingsCapturePlatformFixture{initPlatformFixture: newInitPlatformFixture(), capture: &settingsCaptureFixture{}}
	fixture.cli.options.InitPlatform = unavailableIdentityPlatform{platform}
	path := filepath.Join(loaded.Context.Paths.ConfigHome, "yards/default/config.env")
	before, beforeErr := os.ReadFile(path)
	prepared, err := fixture.cli.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: nativeConfigCommandDefinition(), Arguments: []string{"set", "DEV_USER", "other", "--scope", "yard"}})
	if err == nil {
		prepared.Close()
		t.Fatal("unverifiable identity change accepted")
	}
	if !strings.Contains(err.Error(), "applied developer identity") || platform.prepares != 0 {
		t.Fatalf("wrong refusal: %v prepares=%d", err, platform.prepares)
	}
	after, afterErr := os.ReadFile(path)
	if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) {
		t.Fatal("refused identity edit changed source")
	}
}
