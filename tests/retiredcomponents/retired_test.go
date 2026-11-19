package compatibility_test

import (
	"bytes"
	"context"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/cli"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRetiredComponentDataPreservesSupportedInputs(t *testing.T) {
	components, err := profile.LoadRetired("../..")
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"aiobserver", "paseo", "openlit"}
	var ids []string
	for _, component := range components {
		ids = append(ids, component.ID)
	}
	if !slices.Equal(ids, expected) {
		t.Fatalf("retired input inventory: %v", ids)
	}
	if !reflect.DeepEqual(components[0].AdoptionKeys, []string{"user.subyard.ai_observer_provision", "user.subyard.ai_observer_proxy", "user.subyard.deferred_publication.ai-observer"}) || len(components[1].AdoptionKeys) != 0 || len(components[2].AdoptionKeys) != 0 {
		t.Fatal("adoption authority differs from the accepted baseline")
	}
	if components[0].Route == nil || !components[0].Route.Guard || components[0].Route.Defer || components[2].Route == nil || !components[2].Route.Defer || components[2].Route.Guard {
		t.Fatal("historical route policy changed")
	}
	if !slices.Equal(components[0].Settings, []string{"AI_OBSERVER_HOST_PORT"}) || !slices.Equal(components[2].Settings, []string{"OPENLIT_HOST_PORT"}) {
		t.Fatal("removed settings inventory changed")
	}
	for _, suffix := range []string{"CHECK", "CLEANUP", "COMMAND", "CONFIG", "CONFIG_DEST", "DEPENDS", "HEALTH", "PERSIST", "PROJECTS_CHANGED", "PROVISION", "RULES", "RULES_DEST"} {
		if !slices.Contains(components[1].Settings, "AGENT_paseo_"+suffix) {
			t.Fatalf("missing retired assignment: %s", suffix)
		}
	}
}

func TestRetainedDashboardDefersWhenOwnerAddressDisappears(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../config/retired-components.json")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/retired-components.json"), data, 0644)
	contracts, err := ownerpublication.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ownerpublication.Encode(contracts)
	if err != nil {
		t.Fatal(err)
	}
	contracts, err = ownerpublication.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:100.101.102.103:22222", "connect": "tcp:127.0.0.1:3000"}
	instance := ports.InstanceInfo{LocalConfig: map[string]string{"user.subyard.openlit_dashboard": "v2:100.101.102.103:22222"}, Devices: map[string]map[string]string{"openlit-dashboard": device}, LocalDevices: map[string]map[string]string{"openlit-dashboard": device}}
	config, devices, waiting, err := ownerpublication.Plan(instance, contracts, nil)
	if err != nil || len(devices) != 0 || config[ownerpublication.Prefix+"openlit-dashboard"] == "" || !slices.Equal(waiting, []string{device["listen"]}) {
		t.Fatalf("retained listener blocks start: %v %v %v %v", config, devices, waiting, err)
	}
	instance.LocalConfig = config
	instance.LocalDevices = devices
	instance.Devices = devices
	_, devices, waiting, err = ownerpublication.Plan(instance, contracts, []netip.Addr{netip.MustParseAddr("100.101.102.103")})
	if err != nil || !maps.Equal(devices["openlit-dashboard"], device) || len(waiting) != 0 {
		t.Fatalf("retained listener did not restore: %v %v %v", devices, waiting, err)
	}
}

func TestShippedRetiredSettingsAndAliasesRemainReadable(t *testing.T) {
	declaration, err := os.ReadFile("../../config/retired-components.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, aliases := range []bool{false, true} {
		for _, location := range []string{"config.env", "yards/default/config.env"} {
			t.Run(location+map[bool]string{true: "-aliases", false: "-current"}[aliases], func(t *testing.T) {
				root, home := testkit.TempDir(t), testkit.TempDir(t)
				if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "config/retired-components.json"), declaration, 0644)
				testkit.WriteFile(t, filepath.Join(root, "config/subyard.env"), []byte("SHIFT_MODE=shift\nDEV_UID=1000\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nNESTED_E2E_VMS=0\nSTORAGE_PATH=$SUBYARD_HOME/storage\nHOST_BASE=$SUBYARD_HOME/host\nRESTRICTED_DISK_PATHS=$SUBYARD_HOME/host\nSSH_PORT=2222\n"), 0600)
				names := []string{"CODING_TOOL_INTEGRATIONS", "ENVIRONMENT_PROFILES"}
				if aliases {
					names = []string{"AGENTS", "YARD_PROFILES"}
				}
				content := names[0] + "='aiobserver paseo'\n" + names[1] + "=openlit\nOPENLIT_HOST_PORT=24318\nAI_OBSERVER_HOST_PORT=8080\nAGENT_paseo_COMMAND=retained\n"
				path := filepath.Join(home, location)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte(content), 0600)
				loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: map[string]string{"SUBYARD_CONFIG_HOME": home, "OPENLIT_HOST_PORT": "24318", "AI_OBSERVER_HOST_PORT": "8080", "AGENT_paseo_COMMAND": "retained"}})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(loaded.Integrations.Retired, []string{"aiobserver", "paseo"}) || len(loaded.Integrations.Effective) != 0 || !slices.Equal(loaded.EnvironmentProfiles.Requested, []string{"openlit"}) || len(loaded.EnvironmentProfiles.Effective) != 0 {
					t.Fatal("retired input authority changed")
				}
				for _, key := range []string{"AI_OBSERVER_HOST_PORT", "OPENLIT_HOST_PORT", "AGENT_paseo_COMMAND"} {
					if _, ok := loaded.Environment[key]; ok {
						t.Fatalf("discarded assignment became effective: %s", key)
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != content {
					t.Fatal("loading rewrote stored settings")
				}
			})
		}
	}
}

func TestShippedRemovedProfileEnableReportsRemoval(t *testing.T) {
	root, home := testkit.TempDir(t), testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	declaration, err := os.ReadFile("../../config/retired-components.json")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/retired-components.json"), declaration, 0644)
	testkit.WriteFile(t, filepath.Join(root, "config/commands.registry"), []byte("profile||@profile||deny|mutate|dynamic|public|lifecycle|profile|profile <command> <id>|profiles|--json --yes --help|enable disable setup status\n"), 0600)
	testkit.WriteFile(t, filepath.Join(root, "config/subyard.env"), []byte("SHIFT_MODE=shift\nDEV_UID=1000\nDEV_USER=dev\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nSTORAGE_PATH=$SUBYARD_HOME/storage\nHOST_BASE=$SUBYARD_HOME/host\nRESTRICTED_DISK_PATHS=$SUBYARD_HOME/host\nSSH_PORT=2222\n"), 0600)
	path := filepath.Join(home, "config.env")
	before := []byte("ENVIRONMENT_PROFILES=openlit\n")
	testkit.WriteFile(t, path, before, 0600)
	var output bytes.Buffer
	incus := &testkit.Incus{}
	program, err := cli.New(cli.Options{RepositoryRoot: root, Environment: []string{"HOME=" + testkit.TempDir(t), "SUBYARD_CONFIG_HOME=" + home, "SUBYARD_HOME=" + testkit.TempDir(t), "SUBYARD_NO_AUDIT=1"}, Arguments: []string{"profile", "enable", "openlit", "--yes"}, Incus: incus, Executor: incus, Stdout: &output, Stderr: &output, ProfileRuntime: func(config.Loaded) cli.ProfileServiceRuntime { t.Fatal("removed owner was invoked"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(output.String(), `profile "openlit" was removed and cannot be enabled`) {
		t.Fatalf("enable code=%d output=%s", code, &output)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || len(incus.ExecCalls) != 0 {
		t.Fatal("refused enable changed settings or executed a guest command")
	}
}
