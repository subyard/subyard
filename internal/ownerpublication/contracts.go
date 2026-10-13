package ownerpublication

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/Subyard/Subyard/internal/ownerroute"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/resource"
)

// Load uses owning declarations; device names alone never establish ownership.
func CoreContracts() []Contract {
	return []Contract{{Device: previewroute.DeviceName, Key: previewroute.Key, Codec: "preview", Owns: func(marker string, device map[string]string) bool {
		host, _, _ := previewroute.Owned(marker, device)
		return host != ""
	}}}
}

func Load(root string) ([]Contract, error) {
	contracts := CoreContracts()
	integrations, err := profile.LoadIntegrations(root)
	if err != nil {
		return nil, err
	}
	for _, definition := range integrations {
		dashboard := definition.Dashboard
		if dashboard == nil || dashboard.ContainerRoute == nil {
			continue
		}
		contracts = append(contracts, Contract{Device: dashboard.ContainerRoute.DeviceName, Key: dashboard.ContainerRoute.MarkerKey, Codec: "dashboard", GuestPort: dashboard.GuestPort,
			Owns: func(marker string, device map[string]string) bool {
				host, _, _ := ownerroute.Owned(marker, device, dashboard.GuestPort)
				return host != ""
			}})
	}
	registry, err := resource.Load(root)
	if err != nil {
		return nil, err
	}
	for _, definition := range registry.Definitions() {
		proxy := definition.Proxy
		if proxy == nil || !proxy.OwnershipMetadata || proxy.IsOwnerIPv4Ingress() {
			continue
		}
		contracts = append(contracts, Contract{Device: proxy.Device, Key: proxy.OwnershipKey(), Codec: "resource", Connect: proxy.Connect, Policy: string(proxy.AddressPolicy), Owns: resourceOwnership(*proxy)})
	}
	return contracts, nil
}

func resourceOwnership(proxy resource.ProxyContract) func(string, map[string]string) bool {
	return func(marker string, device map[string]string) bool {
		host, _, ok := strings.Cut(strings.TrimPrefix(device["listen"], "tcp:"), ":")
		if !ok || (!ownerroute.TailscaleAddress(host) && !(host == "127.0.0.1" && proxy.AddressPolicy == resource.ProxyAddressLoopbackOrTailscale)) {
			return false
		}
		expected := map[string]string{"type": "proxy", "bind": "host", "listen": device["listen"], "connect": proxy.Connect}
		return maps.Equal(device, expected) && (marker == proxy.OwnershipValue(device) || marker == strings.Replace(proxy.OwnershipValue(device), "v1:", "v1:pending:", 1))
	}
}

// Encode binds public owning declarations into the root-owned power unit. The
// unit and copied binary already share the install/rollback snapshot boundary.
func Encode(contracts []Contract) (string, error) {
	payload, err := json.Marshal(struct {
		Version     int        `json:"version"`
		ReaderEpoch int        `json:"readerEpoch"`
		Contracts   []Contract `json:"contracts"`
	}{1, 2, contracts})
	return base64.StdEncoding.EncodeToString(payload), err
}
func Decode(value string) ([]Contract, error) {
	payload, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Version     int        `json:"version"`
		ReaderEpoch int        `json:"readerEpoch"`
		Contracts   []Contract `json:"contracts"`
	}
	if strictJSON(payload, &envelope) != nil || envelope.Version != 1 || envelope.ReaderEpoch != 2 {
		return nil, fmt.Errorf("invalid publication contract schema")
	}
	seen := map[string]bool{}
	for index := range envelope.Contracts {
		contract := &envelope.Contracts[index]
		if contract.Device == "" || contract.Key == "" || seen[contract.Device] {
			return nil, fmt.Errorf("invalid publication contract identity")
		}
		seen[contract.Device] = true
		switch contract.Codec {
		case "preview":
			if contract.Device != previewroute.DeviceName || contract.Key != previewroute.Key {
				return nil, fmt.Errorf("invalid preview publication contract")
			}
			contract.Owns = CoreContracts()[0].Owns
		case "dashboard":
			if contract.GuestPort < 1 || contract.GuestPort > 65535 {
				return nil, fmt.Errorf("invalid dashboard publication port")
			}
			port := contract.GuestPort
			contract.Owns = func(marker string, device map[string]string) bool {
				host, _, _ := ownerroute.Owned(marker, device, port)
				return host != ""
			}
		case "resource":
			if contract.Key != "user.subyard.resource."+contract.Device || (contract.Policy != string(resource.ProxyAddressTailscaleOnly) && contract.Policy != string(resource.ProxyAddressLoopbackOrTailscale)) {
				return nil, fmt.Errorf("invalid resource publication contract")
			}
			proxy := resource.ProxyContract{Device: contract.Device, Connect: contract.Connect, AddressPolicy: resource.ProxyAddressPolicy(contract.Policy)}
			contract.Owns = resourceOwnership(proxy)
		default:
			return nil, fmt.Errorf("unsupported publication contract codec")
		}
	}
	return envelope.Contracts, nil
}
