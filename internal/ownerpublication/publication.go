// Package ownerpublication defers receipt-owned host listeners independently of yard power.
package ownerpublication

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/observation"
	"github.com/Subyard/Subyard/internal/ports"
)

const Prefix = "user.subyard.deferred_publication."

type Contract struct {
	Device      string                               `json:"device"`
	Key         string                               `json:"key"`
	Codec       string                               `json:"codec"`
	GuestPort   int                                  `json:"guestPort,omitempty"`
	Connect     string                               `json:"connect,omitempty"`
	Policy      string                               `json:"policy,omitempty"`
	CleanupOnly bool                                 `json:"cleanupOnly,omitempty"`
	Owns        func(string, map[string]string) bool `json:"-"`
}

type Receipt struct {
	Version   int               `json:"version"`
	Ownership string            `json:"ownership"`
	Device    map[string]string `json:"device"`
}

// Plan preserves unrelated state. Its config and device delta must be committed
// together using the observed Incus ETag, including on a running yard.
func Plan(instance ports.InstanceInfo, contracts []Contract, addresses []netip.Addr) (config map[string]string, devices map[string]map[string]string, waiting []string, err error) {
	config = maps.Clone(instance.LocalConfig)
	if config == nil {
		config = map[string]string{}
	}
	devices = maps.Clone(instance.LocalDevices)
	if devices == nil {
		devices = map[string]map[string]string{}
	}
	known := map[string]bool{}
	for _, contract := range contracts {
		key := Prefix + contract.Device
		known[key] = true
		device := instance.Devices[contract.Device]
		local := devices[contract.Device]
		ownership := config[contract.Key]
		saved := config[key]
		if saved != "" {
			var receipt Receipt
			if strictJSON([]byte(saved), &receipt) != nil || receipt.Version != 1 ||
				receipt.Ownership != ownership || !contract.Owns(ownership, receipt.Device) ||
				(len(device) != 0 && (!maps.Equal(device, local) || !maps.Equal(device, receipt.Device))) ||
				(len(local) != 0 && !maps.Equal(local, receipt.Device)) {
				return nil, nil, nil, fmt.Errorf("invalid deferred publication ownership for %q", contract.Device)
			}
			device = receipt.Device
		} else if len(device) == 0 || !maps.Equal(device, local) || !contract.Owns(ownership, device) {
			continue // Foreign, inherited, required and unreceipted listeners retain their start prerequisite.
		}
		if contract.CleanupOnly && saved == "" {
			continue
		}
		address, eligible := listenAddress(device)
		if !eligible {
			if saved != "" {
				return nil, nil, nil, fmt.Errorf("invalid deferred listener %q", contract.Device)
			}
			continue
		}
		// Validate the same durable receipt/listener schema before cancelling.
		// Retirement must never restore an exposure that is no longer active.
		if contract.CleanupOnly {
			delete(config, key)
			delete(devices, contract.Device)
			continue
		}
		present := slices.ContainsFunc(addresses, func(available netip.Addr) bool { return available.Unmap() == address })
		if !present {
			if saved == "" {
				payload, marshalErr := json.Marshal(Receipt{Version: 1, Ownership: ownership, Device: device})
				if marshalErr != nil {
					return nil, nil, nil, marshalErr
				}
				config[key] = string(payload)
			}
			delete(devices, contract.Device)
			waiting = append(waiting, device["listen"])
		} else if saved != "" {
			devices[contract.Device] = maps.Clone(device)
			delete(config, key)
		}
	}
	for key, value := range config {
		if strings.HasPrefix(key, Prefix) && value != "" && !known[key] {
			return nil, nil, nil, fmt.Errorf("unknown deferred publication %q", key)
		}
	}
	slices.Sort(waiting)
	return config, devices, waiting, nil
}

func listenAddress(device map[string]string) (netip.Addr, bool) {
	if device["type"] != "proxy" || device["nat"] == "true" || device["bind"] != "host" {
		return netip.Addr{}, false
	}
	protocol, endpoint, _ := strings.Cut(device["listen"], ":")
	if protocol != "tcp" && protocol != "udp" {
		return netip.Addr{}, false
	}
	host, port, err := net.SplitHostPort(endpoint)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(host)
	return address.Unmap(), err == nil && !address.IsLoopback() && !address.IsUnspecified()
}

// Deferred is a diagnostic only; reconciliation still validates its owning contract.
func Deferred(instance ports.InstanceInfo) []string {
	var result []string
	for key, value := range instance.Config {
		if !strings.HasPrefix(key, Prefix) || value == "" {
			continue
		}
		var receipt Receipt
		if json.Unmarshal([]byte(value), &receipt) == nil && receipt.Version == 1 {
			result = append(result, receipt.Device["listen"])
		}
	}
	slices.Sort(result)
	return result
}

func strictJSON(payload []byte, target any) error {
	if _, err := observation.Decode(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing publication JSON")
	}
	return nil
}
