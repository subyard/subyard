package amnezia_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		definition.Proxy.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP ||
		definition.Proxy.Connect != "udp:guest:host-port" || !definition.Proxy.OwnershipMetadata || !definition.Startup {
		t.Fatal("shipped VPN proxy descriptor is unavailable")
	}
	admin, ok := registry.Lookup("vpn-admin")
	if !ok || admin.Proxy == nil || admin.Profile != "amnezia" || admin.Startup ||
		admin.Proxy.AddressPolicy != resource.ProxyAddressOwnerIPv4TCP || admin.Proxy.Connect != "tcp:guest:22" || !admin.Proxy.OwnershipMetadata {
		t.Fatal("shipped native application administrative proxy descriptor is unavailable")
	}
	wantManagement := &resource.ManagementContract{Application: "AmneziaVPN", HostSetting: admin.Proxy.AdvertiseHostSetting, PortSetting: admin.Proxy.HostPortSetting, User: "amnezia"}
	if !reflect.DeepEqual(admin.Management, wantManagement) || admin.Dashboard != nil {
		t.Fatalf("native application connection contract=%+v, want=%+v", admin.Management, wantManagement)
	}
	if admin.Proxy.AdvertiseHostSetting != definition.Proxy.AdvertiseHostSetting || admin.Proxy.OwnerInterfaceSetting != definition.Proxy.OwnerInterfaceSetting {
		t.Fatal("VPN and administrative routes do not share the typed owner address/interface")
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
		"address":    {definition.Proxy.AdvertiseHostSetting, "10.20.30.40", config.SettingIPv4},
		"interface":  {definition.Proxy.OwnerInterfaceSetting, "eth0", config.SettingInterface},
		"port":       {definition.Proxy.HostPortSetting, "42020", config.SettingPort},
		"admin port": {admin.Proxy.HostPortSetting, "42022", config.SettingPort},
	}
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: root, OperatorHome: home, YardName: "vpn-e2e", DisablePrivate: true,
		Environment: map[string]string{
			"SUBYARD_CONFIG_HOME":     configHome,
			"RESOURCE_VPN_IPV4":       endpoint["address"].value,
			"RESOURCE_VPN_INTERFACE":  endpoint["interface"].value,
			"RESOURCE_VPN_PORT":       endpoint["port"].value,
			"RESOURCE_VPN_ADMIN_PORT": endpoint["admin port"].value,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		definition resource.Definition
		protocol   string
		guestPort  int
	}{
		{definition, "udp", 42020}, {admin, "tcp", 22},
	} {
		hostPort, err := strconv.Atoi(loaded.Environment[test.definition.Proxy.HostPortSetting])
		if err != nil {
			t.Fatal(err)
		}
		protocol, guestPort, valid := test.definition.Proxy.GuestEndpoint(hostPort)
		if !valid || protocol != test.protocol || guestPort != test.guestPort {
			t.Errorf("%s endpoint resolved to %s/%d, want %s/%d", test.definition.Command, protocol, guestPort, test.protocol, test.guestPort)
		}
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
