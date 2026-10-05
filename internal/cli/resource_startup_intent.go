package cli

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

const (
	startupPending  = "pending"
	startupEnabled  = "enabled"
	startupDisabled = "disabled"
)

func startupIntentKey(definition resource.Definition) string {
	return "user.subyard.startup." + definition.Proxy.Device
}

func startupIntent(instance ports.InstanceInfo, definition resource.Definition) (string, error) {
	key := startupIntentKey(definition)
	value := instance.LocalConfig[key]
	if value == "" {
		if instance.LocalConfig != nil && instance.Config[key] != "" {
			return "", fmt.Errorf("startup intent for %s must be local to the yard instance", definition.Command)
		}
		value = instance.Config[key]
	}
	if instance.LocalConfig[key] != "" && instance.Config[key] != "" && instance.Config[key] != instance.LocalConfig[key] {
		return "", fmt.Errorf("conflicting startup intent for %s", definition.Command)
	}
	switch value {
	case "", startupPending, startupEnabled, startupDisabled:
		return value, nil
	default:
		return "", fmt.Errorf("invalid startup intent for %s", definition.Command)
	}
}

// A provisioned legacy resource may already own its exact public route without
// a startup key. Leave it running under its existing guest and ingress state;
// only a clean resource is armed for first activation.
func startupSeedEligible(loaded config.Loaded, instance ports.InstanceInfo, definition resource.Definition) (bool, error) {
	state, err := startupIntent(instance, definition)
	if err != nil || state != "" {
		return false, err
	}
	contract := definition.Proxy
	if contract == nil || contract.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP {
		return false, fmt.Errorf("startup resource %s lacks public ingress ownership", definition.Command)
	}
	local, localExists := instance.LocalDevices[contract.Device]
	effective, effectiveExists := instance.Devices[contract.Device]
	marker, markerExists := instance.LocalConfig[contract.OwnershipKey()]
	effectiveMarker, effectiveMarkerExists := instance.Config[contract.OwnershipKey()]
	if !localExists && !effectiveExists && !markerExists && !effectiveMarkerExists {
		return true, nil
	}
	hostPort, _ := strconv.Atoi(loaded.Environment[contract.HostPortSetting])
	_, guestPort, valid := contract.GuestEndpoint(hostPort)
	if !valid || !localExists || !effectiveExists || !markerExists || !effectiveMarkerExists ||
		!maps.Equal(local, effective) || marker == "" || marker != effectiveMarker ||
		marker != contract.OwnershipValue(local) {
		return false, fmt.Errorf("startup resource %s has partial or foreign ingress", definition.Command)
	}
	guest := instance.Devices["eth0"]["ipv4.address"]
	guestAddress, guestErr := netip.ParseAddr(guest)
	owner := loaded.Environment[contract.AdvertiseHostSetting]
	ownerAddress, ownerErr := netip.ParseAddr(owner)
	port := loaded.Environment[contract.HostPortSetting]
	portNumber, portErr := strconv.Atoi(port)
	if guestErr != nil || !guestAddress.Is4() || !guestAddress.IsPrivate() ||
		ownerErr != nil || !resource.ExplicitOwnerIPv4(ownerAddress) ||
		portErr != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
		return false, fmt.Errorf("startup resource %s has invalid selected endpoint", definition.Command)
	}
	want := map[string]string{
		"type": "proxy", "bind": "host", "nat": "true",
		"listen":  "udp:" + owner + ":" + port,
		"connect": fmt.Sprintf("udp:%s:%d", guest, guestPort),
	}
	if !maps.Equal(local, want) {
		return false, fmt.Errorf("startup resource %s ingress differs from its selected endpoint", definition.Command)
	}
	return false, nil
}

func (cli *CLI) selectedStartupResources(loaded config.Loaded) []resource.Definition {
	selected := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])
	var definitions []resource.Definition
	for _, definition := range cli.resources.Definitions() {
		if definition.Startup && slices.Contains(selected, definition.Profile) &&
			loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"] == definition.Profile &&
			loaded.Context.AccessKind == domain.AccessLocal && loaded.Context.YardKind == domain.YardVM &&
			loaded.Context.YardName != "" && loaded.Context.YardName != "default" {
			definitions = append(definitions, definition)
		}
	}
	return definitions
}

// This value is durable desired intent. Ingress and guest systemd state are
// observed effects and never substitute for a missing or invalid intent.
func (cli *CLI) setStartupIntent(ctx context.Context, loaded config.Loaded, definition resource.Definition, from, to string) error {
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		return err
	}
	if instance.Name != loaded.Context.YardInstanceName || instance.Project != loaded.Context.IncusProject || instance.Type != domain.YardVM {
		return fmt.Errorf("startup intent requires the selected dedicated VM instance")
	}
	current, err := startupIntent(instance, definition)
	if err != nil {
		return err
	}
	if current != from {
		return fmt.Errorf("%w: startup intent changed for %s", domain.ErrPlanStale, definition.Command)
	}
	if current == to {
		return nil
	}
	writer, ok := incus.(ports.InstanceConfigWriter)
	if !ok {
		return fmt.Errorf("Incus instance config writer is required for startup intent")
	}
	return writer.SetInstanceConfig(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName,
		map[string]string{startupIntentKey(definition): to})
}

type resourceStartupIntent struct {
	loaded     config.Loaded
	definition resource.Definition
	before     string
	after      string
}

func (cli *CLI) prepareResourceStartupIntent(ctx context.Context, loaded config.Loaded, definition resource.Definition, verb string) (*resourceStartupIntent, error) {
	if !definition.Startup || (verb != definition.BringUp && verb != definition.Shutdown) {
		return nil, nil
	}
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		return nil, err
	}
	if instance.Name != loaded.Context.YardInstanceName || instance.Project != loaded.Context.IncusProject || instance.Type != domain.YardVM {
		return nil, fmt.Errorf("startup intent requires the selected dedicated VM instance")
	}
	before, err := startupIntent(instance, definition)
	if err != nil {
		return nil, err
	}
	after := startupDisabled
	if verb == definition.BringUp {
		after = startupEnabled
	}
	return &resourceStartupIntent{loaded: loaded, definition: definition, before: before, after: after}, nil
}

func (intent *resourceStartupIntent) augment(assessment domain.ActionAssessment) domain.ActionAssessment {
	if intent != nil && intent.before != intent.after {
		assessment.Changed = true
		assessment.Consequences = append(assessment.Consequences,
			"record "+intent.definition.Command+" startup intent as "+intent.after)
	}
	return assessment
}

func (intent *resourceStartupIntent) refresh(ctx context.Context, cli *CLI) error {
	if intent == nil {
		return nil
	}
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, intent.loaded.Context.IncusProject, intent.loaded.Context.YardInstanceName)
	if err != nil {
		return err
	}
	current, err := startupIntent(instance, intent.definition)
	if err != nil {
		return err
	}
	if current != intent.before {
		return domain.ErrPlanStale
	}
	return nil
}

func (intent *resourceStartupIntent) commit(ctx context.Context, cli *CLI) error {
	if intent == nil || intent.before == intent.after {
		return nil
	}
	return cli.setStartupIntent(ctx, intent.loaded, intent.definition, intent.before, intent.after)
}
