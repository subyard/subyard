package cli

import (
	"context"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
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
		if proxy == nil || !proxy.IsOwnerIPv4Ingress() ||
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
		output, err := cli.prepareResource(ctx, cli.resourceShutdownContext(ctx, loaded, definition, definition.Shutdown), definition, []string{definition.Shutdown})
		if err != nil {
			return resourceRefusal(domain.ConfigResourceShutdownUnverified, request.name, definition.Command, definition.Shutdown)
		}
		assessment, err := cli.resources.AssessPrepareResult(cli.coreActions, definition.Command, definition.Shutdown, output)
		if err != nil {
			return err
		}
		if assessment.Changed {
			return resourceRefusal(domain.ConfigResourceIngressEnabled, definition.Command, definition.Shutdown, request.name)
		}
	}
	return nil
}

// checkResourceConfigChanges reports every selected change that a running
// owner ingress resource refuses.
func (cli *CLI) checkResourceConfigChanges(ctx context.Context, loaded config.Loaded, requests []configAuthoringRequest) error {
	var refusals []error
	for _, request := range requests {
		refusals = append(refusals, cli.checkResourceConfigChange(ctx, loaded, request))
	}
	return domain.CollectRefusals(refusals...)
}

// resourceRefusal types an ingress refusal over a setting name and shipped
// command words, in template order; any other text stays unclassified.
func resourceRefusal(code domain.ConfigRefusalCode, words ...string) error {
	names := make([]domain.ConfigName, len(words))
	for index, word := range words {
		name, ok := domain.ConfigSettingName(word)
		if !ok {
			name, ok = domain.ConfigCommandName(word)
		}
		if !ok {
			return domain.UnclassifiedConfigRefusal(code, words...)
		}
		names[index] = name
	}
	return domain.NewConfigRefusal(code, names...)
}
