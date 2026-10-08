package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
)

func (runtime Runtime) previewSourceHash() (string, error) {
	payload, err := (config.MaterializedAsset{
		Source: filepath.Join(runtime.RepositoryRoot, "config", "preview", "subyard-preview"),
	}).ReadSource()
	if err != nil {
		return "", fmt.Errorf("static preview helper source is unavailable: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func (runtime Runtime) previewConverged(ctx context.Context) (bool, error) {
	digest, err := runtime.previewSourceHash()
	if err != nil {
		return false, err
	}
	endpointDigest, err := runtime.previewEndpointHash(ctx)
	if err != nil {
		return false, err
	}
	return runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `
helper=/usr/local/bin/subyard-preview
[ ! -L "$helper" ]
[ "$(stat -c '%F|%a|%u:%g' "$helper")" = 'regular file|755|0:0' ]
[ "$(sha256sum "$helper" | cut -d ' ' -f 1)" = "$1" ]
endpoint=/etc/subyard/preview.json
[ ! -L "$endpoint" ]
[ "$(stat -c '%F|%a|%u:%g' "$endpoint")" = 'regular file|644|0:0' ]
[ "$(sha256sum "$endpoint" | cut -d ' ' -f 1)" = "$2" ]
`, "subyard", digest, endpointDigest})
}

func (runtime Runtime) previewEndpointHash(ctx context.Context) (string, error) {
	endpoint, err := runtime.previewEndpoint(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(endpoint)), nil
}

func (runtime Runtime) previewEndpoint(ctx context.Context) ([]byte, error) {
	return runtime.previewEndpointForHost(ctx, previewroute.Host(ctx))
}

func (runtime Runtime) preparePreview(ctx context.Context) ([]byte, error) {
	if runtime.NetworkPolicy == nil {
		return nil, fmt.Errorf("yard network policy service is required")
	}
	host := previewroute.Host(ctx)
	endpoint, err := runtime.previewEndpointForHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if err := runtime.preparePreviewRoute(ctx, host); err != nil {
		return nil, err
	}
	if err := runtime.NetworkPolicy.Ensure(ctx, runtime.networkPolicyYard()); err != nil {
		return nil, fmt.Errorf("reconcile static preview network ingress: %w", err)
	}
	return endpoint, nil
}

func (runtime Runtime) previewEndpointForHost(ctx context.Context, host string) ([]byte, error) {
	guest := ""
	if host != "127.0.0.1" && runtime.Yard.YardKind == domain.YardVM {
		if runtime.Incus == nil {
			return nil, fmt.Errorf("Incus client is required for VM preview endpoint")
		}
		instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
		if err != nil {
			return nil, fmt.Errorf("inspect VM preview endpoint: %w", err)
		}
		guest = instance.Devices["eth0"]["ipv4.address"]
		address, parseErr := netip.ParseAddr(guest)
		if parseErr != nil || !address.Is4() || !address.IsPrivate() || address.String() != guest {
			return nil, fmt.Errorf("VM preview requires a private primary IPv4 pin")
		}
	}
	return previewroute.Metadata(host, runtime.environmentValue("WEB_PREVIEW_HOST_PORT"), guest), nil
}

func (runtime Runtime) previewRouteConverged(ctx context.Context, instance ports.InstanceInfo) bool {
	return runtime.previewRouteConvergedForHost(previewroute.Host(ctx), instance)
}

// Fresh VMs acquire their primary pin in the instance stage. Until then, the
// network-policy stage must establish only the baseline policy for their start.
func (runtime Runtime) previewRouteDrift(ctx context.Context, host string) (bool, error) {
	if runtime.Incus == nil {
		return false, fmt.Errorf("Incus client is required for preview route")
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect preview route: %w", err)
	}
	if host != "127.0.0.1" && runtime.Yard.YardKind == domain.YardVM && instance.Devices["eth0"]["ipv4.address"] == "" {
		return false, nil
	}
	return !runtime.previewRouteConvergedForHost(host, instance), nil
}

func (runtime Runtime) preparePreviewRoute(ctx context.Context, host string) error {
	drift, err := runtime.previewRouteDrift(ctx, host)
	if err != nil || !drift {
		return err
	}
	return runtime.runScript(ctx, runtime.Stderr, "prepare-preview-route.sh", host)
}

func (runtime Runtime) previewRouteConvergedForHost(host string, instance ports.InstanceInfo) bool {
	marker := instance.LocalConfig[previewroute.Key]
	device := instance.Devices[previewroute.DeviceName]
	if host == "127.0.0.1" {
		return marker == "" && len(device) == 0 && len(instance.LocalDevices[previewroute.DeviceName]) == 0
	}
	actualHost, port, ready := previewroute.Owned(marker, device)
	guest := ""
	if runtime.Yard.YardKind == domain.YardVM {
		guest = instance.Devices["eth0"]["ipv4.address"]
		if guest == "" {
			return false
		}
	}
	return ready && actualHost == host && port == runtime.environmentValue("WEB_PREVIEW_HOST_PORT") &&
		maps.Equal(device, instance.LocalDevices[previewroute.DeviceName]) && maps.Equal(device, previewroute.Device(host, port, guest))
}
