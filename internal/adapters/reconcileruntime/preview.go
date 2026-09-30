package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/observerroute"
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
	endpointDigest := runtime.previewEndpointHash(ctx)
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

func (runtime Runtime) previewEndpointHash(ctx context.Context) string {
	return fmt.Sprintf("%x", sha256.Sum256(runtime.previewEndpoint(ctx)))
}

func (runtime Runtime) previewEndpoint(ctx context.Context) []byte {
	host := "127.0.0.1"
	if runtime.Yard.YardKind != domain.YardVM {
		host = observerroute.Host(ctx)
	}
	return previewroute.Metadata(host, runtime.environmentValue("WEB_PREVIEW_HOST_PORT"))
}

func (runtime Runtime) previewRouteConverged(ctx context.Context, instance ports.InstanceInfo) bool {
	host := "127.0.0.1"
	if runtime.Yard.YardKind != domain.YardVM {
		host = observerroute.Host(ctx)
	}
	marker := instance.LocalConfig[previewroute.Key]
	device := instance.Devices[previewroute.DeviceName]
	if host == "127.0.0.1" {
		return marker == "" && len(device) == 0 && len(instance.LocalDevices[previewroute.DeviceName]) == 0
	}
	actualHost, port, ready := previewroute.Owned(marker, device)
	return ready && actualHost == host && port == runtime.environmentValue("WEB_PREVIEW_HOST_PORT") &&
		maps.Equal(device, instance.LocalDevices[previewroute.DeviceName])
}
