package amnezia_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestShippedAmneziaProxyUsesLoadedTypedEndpointSettings(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.Lookup("vpn")
	if !ok || definition.Proxy == nil || definition.Profile != "amnezia" ||
		definition.Proxy.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP {
		t.Fatal("shipped VPN proxy descriptor is unavailable")
	}
	preset, err := os.ReadFile(filepath.Join(root, "config", "profiles", "amnezia", "yard.env"))
	if err != nil {
		t.Fatal(err)
	}
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, "config")
	writeFixture(t, filepath.Join(configHome, "yards", "vpn-e2e", "config.env"), string(preset))
	endpoint := map[string]struct {
		setting string
		value   string
		typeOf  config.SettingValueType
	}{
		"address":   {definition.Proxy.AdvertiseHostSetting, "10.20.30.40", config.SettingIPv4},
		"interface": {definition.Proxy.OwnerInterfaceSetting, "eth0", config.SettingInterface},
		"port":      {definition.Proxy.HostPortSetting, "42020", config.SettingPort},
	}
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: root, OperatorHome: home, YardName: "vpn-e2e", DisablePrivate: true,
		Environment: map[string]string{
			"SUBYARD_CONFIG_HOME":    configHome,
			"RESOURCE_VPN_IPV4":      endpoint["address"].value,
			"RESOURCE_VPN_INTERFACE": endpoint["interface"].value,
			"RESOURCE_VPN_PORT":      endpoint["port"].value,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for field, expected := range endpoint {
		if loaded.Environment[expected.setting] != expected.value {
			t.Errorf("proxy %s setting %q did not resolve: got %q, want %q",
				field, expected.setting, loaded.Environment[expected.setting], expected.value)
		}
		setting, err := config.ValidateSettingName(config.ScopeCommand, expected.setting, false)
		if err != nil || setting.Type != expected.typeOf {
			t.Errorf("proxy %s setting %q has wrong type: %#v, %v", field, expected.setting, setting.Type, err)
		}
	}
}
