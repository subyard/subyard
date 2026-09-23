package testvmsruntime

import (
	"errors"
	"fmt"
	"runtime"
)

const (
	EnvironmentPair     = "subyard-pair"
	EnvironmentAndroid  = "android-test"
	DisposableLifecycle = "disposable-v1"
)

// EnvironmentSpec is resolved by the broker's trusted configuration. Clients
// select a name, never arbitrary devices, privileges or resource limits.
type EnvironmentSpec struct {
	Name      string `json:"type"`
	Count     int    `json:"vm_count"`
	CPU       int    `json:"cpu_per_vm"`
	Memory    string `json:"memory_per_vm"`
	Disk      string `json:"disk_per_vm"`
	Lifecycle string `json:"lifecycle"`
}

func (cfg Config) EnvironmentSpec(name string) (EnvironmentSpec, error) {
	return cfg.environmentSpecForArch(name, runtime.GOARCH)
}

func (cfg Config) environmentSpecForArch(name, arch string) (EnvironmentSpec, error) {
	spec := EnvironmentSpec{Name: name, CPU: cfg.CPU, Lifecycle: DisposableLifecycle}
	switch name {
	case EnvironmentPair:
		spec.Count, spec.Memory, spec.Disk = 2, cfg.Memory, cfg.Disk
	case EnvironmentAndroid:
		if arch != "amd64" {
			return EnvironmentSpec{}, errors.New("android-test requires x86_64")
		}
		spec.Count, spec.Memory, spec.Disk = 1, "8GiB", "40GiB"
	default:
		return EnvironmentSpec{}, fmt.Errorf("unsupported environment type %q", name)
	}
	return spec, spec.Validate()
}

func (spec EnvironmentSpec) Validate() error {
	if spec.Lifecycle != DisposableLifecycle || spec.CPU < 1 {
		return errors.New("invalid environment lifecycle or CPU limit")
	}
	if (spec.Name != EnvironmentPair || spec.Count != 2) &&
		(spec.Name != EnvironmentAndroid || spec.Count != 1) {
		return errors.New("invalid named environment composition")
	}
	if _, err := sizeMiB(spec.Memory); err != nil {
		return errors.New("invalid environment memory limit")
	}
	if disk, err := sizeMiB(spec.Disk); err != nil || disk < 10*1024 {
		return errors.New("environment disk must be at least 10GiB")
	}
	return nil
}

func (cfg Config) guestCount() int {
	if cfg.Environment != nil {
		return cfg.Environment.Count
	}
	return provisionedGuestCount // Retained leases preserve their original contract.
}

func (cfg Config) totalSize(value string) string {
	amount, _ := sizeMiB(value)
	return fmt.Sprintf("%dMiB", amount*cfg.guestCount())
}

func (cfg Config) withEnvironment(spec EnvironmentSpec) Config {
	cfg.Environment = &spec
	cfg.CPU, cfg.Memory, cfg.Disk = spec.CPU, spec.Memory, spec.Disk
	return cfg
}
