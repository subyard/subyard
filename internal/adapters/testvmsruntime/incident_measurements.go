package testvmsruntime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type pressureSample struct {
	Avg10  float64 `json:"avg10_percent"`
	Avg60  float64 `json:"avg60_percent"`
	Avg300 float64 `json:"avg300_percent"`
	Total  uint64  `json:"total_microseconds"`
}

// Parse only kernel PSI counters. Never copy paths or unrecognized fields.
func parsePressure(body []byte) (map[string]pressureSample, error) {
	if len(body) > 4096 {
		return nil, errors.New("pressure counters exceed bound")
	}
	result := map[string]pressureSample{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || (fields[0] != "some" && fields[0] != "full") {
			return nil, errors.New("invalid pressure counters")
		}
		if _, exists := result[fields[0]]; exists {
			return nil, errors.New("duplicate pressure counters")
		}
		var values [3]float64
		for i, key := range []string{"avg10", "avg60", "avg300"} {
			name, raw, ok := strings.Cut(fields[i+1], "=")
			value, err := strconv.ParseFloat(raw, 64)
			if !ok || name != key || err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
				return nil, errors.New("invalid pressure average")
			}
			values[i] = value
		}
		name, raw, ok := strings.Cut(fields[4], "=")
		total, err := strconv.ParseUint(raw, 10, 64)
		if !ok || name != "total" || err != nil {
			return nil, errors.New("invalid pressure total")
		}
		result[fields[0]] = pressureSample{values[0], values[1], values[2], total}
	}
	if _, ok := result["some"]; !ok {
		return nil, errors.New("pressure counters unavailable")
	}
	return result, nil
}

func pressureEvidence(procRoot string) (map[string]map[string]pressureSample, []string) {
	result := map[string]map[string]pressureSample{}
	var missing []string
	for _, resource := range []string{"cpu", "io", "memory"} {
		file, err := os.Open(filepath.Join(procRoot, "pressure", resource))
		if err != nil {
			missing = append(missing, resource)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(file, 4097))
		file.Close()
		if err != nil {
			missing = append(missing, resource)
			continue
		}
		counters, err := parsePressure(body)
		if err != nil {
			missing = append(missing, resource)
			continue
		}
		result[resource] = counters
	}
	return result, missing
}

func incidentJSON(value any) string {
	body, _ := json.Marshal(value)
	return string(body)
}

// Project expanded instance metadata conservatively; all unlisted configuration,
// cloud-init, device sources and raw QEMU values are discarded before persistence.
func instanceEvidence(body string) string {
	if len(body) > 1<<20 {
		return "unavailable: instance metadata exceeds bound"
	}
	var instance struct {
		Architecture string                       `json:"architecture"`
		Config       map[string]string            `json:"expanded_config"`
		Devices      map[string]map[string]string `json:"expanded_devices"`
	}
	if json.Unmarshal([]byte(body), &instance) != nil {
		return "unavailable: invalid instance metadata"
	}
	result := map[string]any{}
	var missing []string
	if instance.Architecture == "x86_64" || instance.Architecture == "aarch64" {
		result["architecture"] = instance.Architecture
	} else {
		missing = append(missing, "architecture")
	}
	fingerprint := instance.Config["volatile.base_image"]
	if imageFingerprint.MatchString(fingerprint) {
		result["base_fingerprint"] = fingerprint
	} else {
		missing = append(missing, "base_fingerprint")
	}
	cpu, err := strconv.ParseUint(instance.Config["limits.cpu"], 10, 32)
	if err == nil && cpu > 0 {
		result["cpu_count"] = cpu
	} else {
		missing = append(missing, "cpu_count")
	}
	memory, err := sizeMiB(instance.Config["limits.memory"])
	if err == nil && memory > 0 {
		result["memory_bytes"] = uint64(memory) << 20
	} else {
		missing = append(missing, "memory_bytes")
	}
	disk, roots := 0, 0
	for _, device := range instance.Devices {
		if device["type"] == "disk" && device["path"] == "/" {
			roots++
			disk, _ = sizeMiB(device["size"])
		}
	}
	if roots == 1 && disk > 0 {
		result["root_disk_bytes"] = uint64(disk) << 20
	} else {
		missing = append(missing, "root_disk_bytes")
	}
	switch instance.Config["security.secureboot"] {
	case "true":
		result["secure_boot"] = true
	case "false":
		result["secure_boot"] = false
	default:
		missing = append(missing, "secure_boot")
	}
	_, policy := vmSleepPolicy(instance.Architecture)
	observed := instance.Config["raw.qemu.conf"]
	if observed == policy {
		result["vm_policy"] = "incus-default"
		if policy != "" {
			result["vm_policy"] = "subyard-x86-s3-v1"
		}
	} else if observed != "" {
		result["vm_policy_sha256"] = fmt.Sprintf("%x", sha256.Sum256([]byte(observed)))
	} else {
		missing = append(missing, "vm_policy")
	}
	if len(missing) > 0 {
		result["unavailable"] = missing
	}
	return incidentJSON(result)
}
