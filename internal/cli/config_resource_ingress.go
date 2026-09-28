package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/resource"
)

// Changing the authority or endpoint of a public resource must not strand its
// old route. Its existing SHUTDOWN contract proves closure without mutation.
func (cli *CLI) checkResourceConfigChange(ctx context.Context, loaded config.Loaded, request configAuthoringRequest) error {
	if request.scope != config.ScopeYard {
		return nil
	}
	selected := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])
	for _, definition := range cli.resources.Definitions() {
		proxy := definition.Proxy
		if proxy == nil || proxy.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP ||
			!slices.Contains(selected, definition.Profile) {
			continue
		}
		affected := false
		switch request.name {
		case "ENVIRONMENT_PROFILES":
			affected = request.action == "unset" || !slices.Contains(strings.Fields(request.value), definition.Profile)
		case "YARD_TEMPLATE":
			affected = request.action == "unset" || request.value != loaded.Environment[request.name]
		default:
			affected = (request.name == proxy.AdvertiseHostSetting || request.name == proxy.HostPortSetting ||
				request.name == proxy.OwnerInterfaceSetting) &&
				(request.action == "unset" || request.value != loaded.Environment[request.name])
		}
		if !affected {
			continue
		}
		output, err := cli.prepareResource(ctx, loaded, definition, []string{definition.Shutdown})
		if err != nil {
			return fmt.Errorf("verify resource shutdown before changing %s: run %s %s in the running yard first", request.name, definition.Command, definition.Shutdown)
		}
		assessment, err := cli.resources.AssessPrepareResult(cli.coreActions, definition.Command, definition.Shutdown, output)
		if err != nil {
			return err
		}
		if assessment.Changed {
			return fmt.Errorf("run %s %s before changing %s; its public ingress or runtime is still enabled", definition.Command, definition.Shutdown, request.name)
		}
	}
	return nil
}
