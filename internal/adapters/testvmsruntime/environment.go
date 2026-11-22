package testvmsruntime

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	EnvironmentPair      = "subyard-pair"
	EnvironmentAndroid   = "android-test"
	EnvironmentContainer = "system-container"
	DisposableLifecycle  = "disposable-v1"
)

// EnvironmentSpec is resolved by the broker's trusted configuration. Clients
// select a name and supported guest count, never arbitrary devices, privileges or resource limits.
type EnvironmentSpec struct {
	Name      string `json:"type"`
	Image     string `json:"image,omitempty"`
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
	case EnvironmentContainer:
		spec.Count = 2
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

const DefaultImage = "debian-13"

func environmentImageType(name string) string {
	if name == EnvironmentContainer {
		return "container"
	}
	return "virtual-machine"
}

func normalizedImage(image string) string {
	if image == "" {
		return DefaultImage
	}
	return image
}

// ValidateImage accepts only the supported guest OS releases.
func ValidateImage(image string) error {
	switch normalizedImage(image) {
	case DefaultImage, "ubuntu-24.04", "ubuntu-26.04":
		return nil
	default:
		return fmt.Errorf("unsupported VM image %q (use debian-13, ubuntu-24.04 or ubuntu-26.04)", image)
	}
}

func (cfg Config) withImage(image string) (Config, error) {
	if err := ValidateImage(image); err != nil {
		return cfg, err
	}
	switch image {
	case DefaultImage:
		cfg.Image = "images:debian/13/cloud"
	case "ubuntu-24.04":
		cfg.Image = "images:ubuntu/24.04/cloud"
	case "ubuntu-26.04":
		cfg.Image = "images:ubuntu/26.04/cloud"
	}
	// An omitted selection preserves the broker's existing configured source.
	return cfg, nil
}

func (spec EnvironmentSpec) Validate() error {
	refusals := []error{ValidateImage(spec.Image)}
	if spec.Lifecycle != DisposableLifecycle || spec.CPU < 1 {
		refusals = append(refusals, errors.New("invalid environment lifecycle or CPU limit"))
	}
	if ((spec.Name != EnvironmentPair && spec.Name != EnvironmentContainer) || (spec.Count != 1 && spec.Count != 2)) &&
		(spec.Name != EnvironmentAndroid || spec.Count != 1) {
		refusals = append(refusals, errors.New("invalid named environment composition"))
	}
	if spec.Name == EnvironmentContainer {
		if spec.Memory != "" || spec.Disk != "" {
			refusals = append(refusals, errors.New("system-container does not accept memory or disk limits"))
		}
		return domain.CollectRefusals(refusals...)
	}
	if _, err := sizeMiB(spec.Memory); err != nil {
		refusals = append(refusals, errors.New("invalid environment memory limit"))
	}
	if disk, err := sizeMiB(spec.Disk); err != nil || disk < 10*1024 {
		refusals = append(refusals, errors.New("environment disk must be at least 10GiB"))
	}
	return domain.CollectRefusals(refusals...)
}

func (cfg Config) systemContainer() bool {
	return cfg.Environment != nil && cfg.Environment.Name == EnvironmentContainer
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
