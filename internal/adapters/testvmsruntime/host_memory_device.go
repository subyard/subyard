package testvmsruntime

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
)

const (
	hostMemoryDevice       = "subyard-host-memory"
	hostMemoryOwnerKey     = "user.subyard.host_memory"
	hostMemoryOwnerVersion = "v1"
)

type hostMemoryInstance struct {
	Config          map[string]string            `json:"config"`
	Devices         map[string]map[string]string `json:"devices"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
}

func hostMemoryDeviceSpec() map[string]string {
	return map[string]string{"type": "disk", "source": "/proc/meminfo", "path": hostMemoryPath, "readonly": "true"}
}

func (backend *Backend) hostMemoryInstance(ctx context.Context) (hostMemoryInstance, error) {
	var instance hostMemoryInstance
	body, err := backend.incus(ctx, "query", "/1.0/instances/"+backend.Instance+"?project="+url.QueryEscape(backend.Project))
	if err != nil {
		return instance, err
	}
	if err := json.Unmarshal([]byte(body), &instance); err != nil {
		return instance, doctorCheck("physical memory device evidence invalid", nil)
	}
	if instance.Config == nil || instance.Devices == nil || instance.ExpandedDevices == nil {
		return instance, doctorCheck("physical memory device evidence incomplete", nil)
	}
	marker := instance.Config[hostMemoryOwnerKey]
	if marker != "" && marker != hostMemoryOwnerVersion && marker != "pending:"+hostMemoryOwnerVersion {
		return instance, doctorCheck("physical memory device ownership conflict", nil)
	}
	device := instance.Devices[hostMemoryDevice]
	if device != nil && (marker == "" || !reflect.DeepEqual(device, hostMemoryDeviceSpec())) {
		return instance, doctorCheck("physical memory device ownership conflict", nil)
	}
	if device != nil && !reflect.DeepEqual(instance.ExpandedDevices[hostMemoryDevice], device) {
		return instance, doctorCheck("physical memory device effective evidence inconsistent", nil)
	}
	for name, expanded := range instance.ExpandedDevices {
		if name == hostMemoryDevice {
			if device == nil || !reflect.DeepEqual(expanded, device) {
				return instance, doctorCheck("physical memory device is inherited or divergent", nil)
			}
		} else if expanded["path"] == hostMemoryPath {
			return instance, doctorCheck("physical memory path is occupied", nil)
		}
	}
	for name, local := range instance.Devices {
		if name != hostMemoryDevice && local["path"] == hostMemoryPath {
			return instance, doctorCheck("physical memory path is occupied", nil)
		}
	}
	return instance, nil
}

func (backend *Backend) hostMemoryConverged(ctx context.Context, enabled bool) (bool, error) {
	instance, err := backend.hostMemoryInstance(ctx)
	if err != nil {
		return false, err
	}
	if !enabled {
		return instance.Config[hostMemoryOwnerKey] == "" && instance.Devices[hostMemoryDevice] == nil, nil
	}
	return instance.Config[hostMemoryOwnerKey] == hostMemoryOwnerVersion && instance.Devices[hostMemoryDevice] != nil, nil
}

// This device can be hotplugged. Keep it outside instance lifecycle reconciliation
// so adding telemetry cannot stop an outer yard or reset its active leases.
func (backend *Backend) reconcileHostMemory(ctx context.Context, enabled bool) error {
	instance, err := backend.hostMemoryInstance(ctx)
	if err != nil {
		return err
	}
	marker, device := instance.Config[hostMemoryOwnerKey], instance.Devices[hostMemoryDevice]
	if !enabled {
		if device != nil {
			if _, err := backend.incus(ctx, "config", "device", "remove", backend.Instance, hostMemoryDevice, "--project", backend.Project); err != nil {
				return err
			}
		}
		if marker != "" {
			_, err = backend.incus(ctx, "config", "unset", backend.Instance, hostMemoryOwnerKey, "--project", backend.Project)
		}
		return err
	}
	if device == nil {
		if marker != "pending:"+hostMemoryOwnerVersion {
			if _, err := backend.incus(ctx, "config", "set", backend.Instance, hostMemoryOwnerKey, "pending:"+hostMemoryOwnerVersion, "--project", backend.Project); err != nil {
				return err
			}
		}
		if _, err := backend.incus(ctx, "config", "device", "add", backend.Instance, hostMemoryDevice, "disk", "--project", backend.Project,
			"source=/proc/meminfo", "path="+hostMemoryPath, "readonly=true"); err != nil {
			return err
		}
	}
	if marker != hostMemoryOwnerVersion || device == nil {
		_, err = backend.incus(ctx, "config", "set", backend.Instance, hostMemoryOwnerKey, hostMemoryOwnerVersion, "--project", backend.Project)
	}
	return err
}
