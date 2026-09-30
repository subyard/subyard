// Package previewroute defines the owned container static-preview endpoint.
package previewroute

import (
	"encoding/json"
	"maps"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/observerroute"
)

const Key = "user.subyard.preview_proxy"
const DeviceName = "subyard-preview"
const EndpointKey = "user.subyard.preview_endpoint_sha256"

func Device(host, port string) map[string]string {
	return map[string]string{"type": "proxy", "bind": "host",
		"listen": "tcp:" + host + ":" + port, "connect": "tcp:127.0.0.1:8765"}
}

// Owned recognizes interrupted installation, but never a foreign device.
func Owned(marker string, device map[string]string) (host, port string, ready bool) {
	value, ok := strings.CutPrefix(marker, "v1:")
	if !ok {
		return "", "", false
	}
	value, pending := strings.CutPrefix(value, "pending:")
	host, port, ok = strings.Cut(value, ":")
	number, err := strconv.Atoi(port)
	if !ok || !observerroute.TailscaleAddress(host) || err != nil ||
		number < 1024 || number > 65535 || strconv.Itoa(number) != port ||
		!maps.Equal(device, Device(host, port)) {
		return "", "", false
	}
	return host, port, !pending
}

// Metadata describes the installed route; fallback uses the existing code SSH forward.
func Metadata(host, port string) []byte {
	if host == "127.0.0.1" {
		port = "8765"
	}
	number, _ := strconv.Atoi(port)
	payload, _ := json.Marshal(struct {
		Version int    `json:"version"`
		Host    string `json:"host"`
		Port    int    `json:"port"`
	}{1, host, number})
	return append(payload, '\n')
}
