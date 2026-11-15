package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/resourceendpoint"
)

const savedResourceEndpointDetail = "saved endpoint allocation for "

// applySavedResourceEndpoints fills unset endpoint settings from saved state. It
// returns the refusals and which listener settings stay unchecked because their
// saved endpoint could not be read; every other endpoint is still applied.
func applySavedResourceEndpoints(
	root string,
	options LoadOptions,
	ctx domain.Context,
	values environment,
	tracker *settingTracker,
) (unchecked func(string) bool, err error) {
	unchecked = func(string) bool { return false }
	if ctx.AccessKind != domain.AccessLocal || options.SyncSource || options.LayerPaths != nil {
		return unchecked, nil
	}
	selected := make(map[string]struct{})
	for _, profile := range strings.Fields(values["ENVIRONMENT_PROFILES"]) {
		selected[profile] = struct{}{}
	}
	if len(selected) == 0 {
		return unchecked, nil
	}
	loadResources := options.Resources
	if loadResources == nil {
		loadResources = resource.Load
	}
	registry, err := loadResources(root)
	if err != nil {
		// Without metadata any profile listener may be an unread endpoint.
		return func(name string) bool {
			definition, ok := tracker.catalog.LookupSetting(name)
			return ok && strings.HasPrefix(definition.Owner, "profile:")
		}, fmt.Errorf("load resource endpoint metadata: %w", err)
	}
	var refusals []error
	unread := map[string]bool{}

	for _, definition := range registry.Definitions() {
		if definition.Endpoint == nil || definition.Proxy == nil {
			continue
		}
		if _, ok := selected[definition.Profile]; !ok {
			continue
		}
		identity := definition.Profile + "." + definition.Name
		layer := tracker.addLayer(
			"derived", "derived",
			fmt.Sprintf("resource %s default %s:%d", identity,
				definition.Endpoint.HostMode, definition.Endpoint.PreferredPort),
			true, settingScalar,
			definition.Proxy.AdvertiseHostSetting, definition.Proxy.HostPortSetting,
		)
		if values[definition.Proxy.AdvertiseHostSetting] != "" &&
			values[definition.Proxy.HostPortSetting] != "" {
			continue
		}
		host, port, exists, err := resourceendpoint.ReadSaved(
			filepath.Join(ctx.Paths.DataHome, "resource-endpoints"),
			ctx.YardName, identity,
		)
		if err != nil {
			refusals = append(refusals, fmt.Errorf("read saved endpoint for %s: %w", identity, err))
			unread[definition.Proxy.HostPortSetting] = true
			continue
		}
		if !exists {
			continue
		}
		detail := savedResourceEndpointDetail + identity
		if values[definition.Proxy.AdvertiseHostSetting] == "" {
			values[definition.Proxy.AdvertiseHostSetting] = host
			tracker.record(layer, definition.Proxy.AdvertiseHostSetting, host, "", 0, detail)
		}
		if values[definition.Proxy.HostPortSetting] == "" {
			portValue := strconv.Itoa(port)
			values[definition.Proxy.HostPortSetting] = portValue
			tracker.record(layer, definition.Proxy.HostPortSetting, portValue, "", 0, detail)
		}
	}
	return func(name string) bool { return unread[name] }, domain.CollectRefusals(refusals...)
}

// ResourceEndpointOverrides returns only operator-provided endpoint values.
// Values derived from resource endpoint state are intentionally omitted so an
// allocator can distinguish persisted choices from explicit settings.
func ResourceEndpointOverrides(
	loaded Loaded,
	definition resource.Definition,
) (host string, port string) {
	if definition.Proxy == nil {
		return "", ""
	}
	return explicitResourceEndpointValue(loaded, definition.Proxy.AdvertiseHostSetting),
		explicitResourceEndpointValue(loaded, definition.Proxy.HostPortSetting)
}

func explicitResourceEndpointValue(loaded Loaded, name string) string {
	value := loaded.Environment[name]
	if value == "" {
		return ""
	}
	trace, ok := loaded.Settings[name]
	if !ok {
		return value
	}
	for _, resolution := range trace.Resolutions {
		if resolution.Status != "effective" {
			continue
		}
		if resolution.Scope == "derived" &&
			strings.HasPrefix(resolution.Detail, savedResourceEndpointDetail) {
			return ""
		}
		return value
	}
	return value
}
