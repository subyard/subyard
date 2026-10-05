package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

var pendingResourceIngress = regexp.MustCompile(`^v1:pending:[0-9a-f]{64}$`)

var errOrphanIngressAccessDeferred = errors.New("owner Incus socket access is not ready for ingress inspection")

func isOrphanIngressSocketPermission(err error) bool {
	var networkError *net.OpError
	return errors.Is(err, os.ErrPermission) && errors.As(err, &networkError) &&
		networkError.Op == "dial" && networkError.Net == "unix"
}

type orphanIngressEntry struct {
	definition          resource.Definition
	contract            resource.ProxyContract
	device              map[string]string
	marker              string
	shutdownEligible    bool
	shutdownUnavailable bool
	shutdown            *domain.ActionAssessment
}

type orphanIngressPlan struct {
	yard    yardnetwork.Yard
	entries []orphanIngressEntry
}

// prepareOrphanIngress inspects only the target yard and only public
// devices declared by the resource registry. A missing selection cannot keep
// a previously owned public route open through init.
func (cli *CLI) prepareOrphanIngress(ctx context.Context, loaded config.Loaded) (*orphanIngressPlan, error) {
	if loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardName == "default" {
		return nil, nil
	}
	selected := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])
	deselected := false
	definitions := cli.resources.Definitions()
	for _, definition := range definitions {
		if definition.Proxy != nil && definition.Proxy.IsOwnerIPv4Ingress() &&
			!slices.Contains(selected, definition.Profile) {
			deselected = true
			break
		}
	}
	if !deselected {
		return nil, nil
	}
	// A proven cold host cannot contain an Incus route. An installed but
	// unavailable daemon remains inconclusive and must fail closed.
	incus, _ := cli.statusPorts()
	if probe, ok := incus.(interface{ LocalInstallationAbsent() bool }); ok && probe.LocalInstallationAbsent() {
		return nil, nil
	}
	yard := networkYard(loaded.Context)
	instance, err := incus.Instance(ctx, yard.Project, yard.Instance)
	if err != nil {
		if errors.Is(err, ports.ErrInstanceNotFound) {
			return nil, nil
		}
		if isOrphanIngressSocketPermission(err) {
			return nil, fmt.Errorf("%w: %w", errOrphanIngressAccessDeferred, err)
		}
		return nil, err
	}
	plan, err := planOrphanIngress(yard, &instance, selected,
		loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"], definitions)
	if err != nil || plan == nil {
		return plan, err
	}
	for i := range plan.entries {
		entry := &plan.entries[i]
		if !entry.shutdownEligible {
			continue
		}
		output, prepareErr := cli.prepareResource(ctx, loaded, entry.definition, []string{entry.definition.Shutdown})
		if prepareErr != nil {
			if len(entry.device) != 0 || entry.marker != "" {
				entry.shutdownUnavailable = true
				continue
			}
			return nil, fmt.Errorf("assess deselected resource shutdown: %w", prepareErr)
		}
		assessment, assessErr := cli.resources.AssessPrepareResult(cli.coreActions,
			entry.definition.Command, entry.definition.Shutdown, output)
		if assessErr != nil {
			return nil, assessErr
		}
		if err := validateOrphanShutdown(entry.definition, assessment); err != nil {
			return nil, err
		}
		entry.shutdown = &assessment
	}
	plan.entries = slices.DeleteFunc(plan.entries, func(entry orphanIngressEntry) bool {
		return len(entry.device) == 0 && entry.marker == "" && (entry.shutdown == nil || !entry.shutdown.Changed)
	})
	if len(plan.entries) == 0 {
		return nil, nil
	}
	return plan, nil
}

func planOrphanIngress(yard yardnetwork.Yard, instance *ports.InstanceInfo, selected []string, role string, definitions []resource.Definition) (*orphanIngressPlan, error) {
	if instance == nil {
		return nil, nil
	}
	plan := &orphanIngressPlan{yard: yard}
	for _, definition := range definitions {
		contract := definition.Proxy
		if contract == nil || !contract.IsOwnerIPv4Ingress() ||
			slices.Contains(selected, definition.Profile) {
			continue
		}
		device := instance.LocalDevices[contract.Device]
		marker := instance.LocalConfig[contract.OwnershipKey()]
		if len(device) == 0 && marker == "" {
			if len(instance.Devices[contract.Device]) != 0 {
				return nil, fmt.Errorf("resource ingress device %q is inherited or unowned", contract.Device)
			}
			if role != definition.Profile || !strings.EqualFold(instance.Status, "running") {
				continue
			}
		}
		if instance.Type != domain.YardVM ||
			(len(device) != 0 || marker != "") && (len(device) == 0 && !pendingResourceIngress.MatchString(marker) ||
				len(device) != 0 && (!validOrphanPublicIngressDevice(device, *contract) ||
					!maps.Equal(device, instance.Devices[contract.Device]) ||
					(marker != contract.OwnershipValue(device) && marker != "v1:pending:"+strings.TrimPrefix(contract.OwnershipValue(device), "v1:")))) {
			return nil, fmt.Errorf("resource ingress device %q has ambiguous ownership", contract.Device)
		}
		plan.entries = append(plan.entries, orphanIngressEntry{definition: definition, contract: *contract,
			device: maps.Clone(device), marker: marker,
			shutdownEligible: role == definition.Profile && strings.EqualFold(instance.Status, "running")})
	}
	if len(plan.entries) == 0 {
		return nil, nil
	}
	return plan, nil
}

func validateOrphanShutdown(definition resource.Definition, assessment domain.ActionAssessment) error {
	if _, ok := localResourceAction(definition, assessment.Action); !ok ||
		assessment.Effect == domain.ActionDestruction ||
		slices.Contains(assessment.Impacts, domain.ImpactPersistentData) ||
		slices.Contains(assessment.Impacts, domain.ImpactExternalSystem) ||
		(assessment.Effect == domain.ActionMutation && assessment.Recovery != domain.RecoveryReversible) ||
		(assessment.Changed && assessment.Effect != domain.ActionMutation) ||
		(!assessment.Changed && assessment.Effect != domain.ActionRead && assessment.Effect != domain.ActionMutation) {
		return fmt.Errorf("resource %s has an unsafe declared shutdown; run %s %s explicitly first",
			definition.Command, definition.Command, definition.Shutdown)
	}
	return nil
}

func validOrphanPublicIngressDevice(device map[string]string, contract resource.ProxyContract) bool {
	listenProtocol, endpointText, _ := strings.Cut(device["listen"], ":")
	endpoint, _ := netip.ParseAddrPort(endpointText)
	protocol, guestPort, valid := contract.GuestEndpoint(int(endpoint.Port()))
	valid = valid && listenProtocol == protocol
	if !valid || len(device) != 5 || device["type"] != "proxy" || device["bind"] != "host" || device["nat"] != "true" {
		return false
	}
	listenText, listenOK := strings.CutPrefix(device["listen"], protocol+":")
	connectText, connectOK := strings.CutPrefix(device["connect"], protocol+":")
	listen, listenErr := netip.ParseAddrPort(listenText)
	connect, connectErr := netip.ParseAddrPort(connectText)
	return listenOK && connectOK && listenErr == nil && connectErr == nil &&
		resource.ExplicitOwnerIPv4(listen.Addr()) && listen.Port() != 0 &&
		device["listen"] == protocol+":"+listen.String() && device["connect"] == protocol+":"+connect.String() && connect.Addr().Is4() && connect.Addr().IsPrivate() &&
		int(connect.Port()) == guestPort
}

func (plan *orphanIngressPlan) consequences() []string {
	if plan == nil {
		return nil
	}
	result := make([]string, 0, len(plan.entries)*2)
	for _, entry := range plan.entries {
		if len(entry.device) != 0 || entry.marker != "" {
			result = append(result, fmt.Sprintf("close owned public ingress %s in yard %s; preserve guest state", entry.contract.Device, plan.yard.Name))
		}
		if entry.shutdown != nil && entry.shutdown.Changed {
			result = append(result, entry.shutdown.Consequences...)
		} else if entry.shutdownUnavailable || entry.shutdown == nil {
			result = append(result, fmt.Sprintf("guest shutdown for resource %s is unverified; close its owned route and require recovery before continuing init", entry.definition.Command))
		}
	}
	return result
}

func (plan *orphanIngressPlan) refresh(ctx context.Context, cli *CLI, loaded config.Loaded) error {
	fresh, err := cli.prepareOrphanIngress(ctx, loaded)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(plan, fresh) {
		return fmt.Errorf("%w: owned public ingress changed after approval", domain.ErrPlanStale)
	}
	return nil
}

type orphanIngressCloser interface {
	RemoveOwnedResourceIngress(context.Context, yardnetwork.Yard, resource.ProxyContract, map[string]string, string, bool) error
}

func (plan *orphanIngressPlan) apply(ctx context.Context, cli *CLI, loaded config.Loaded, operationID string) error {
	if plan == nil {
		return nil
	}
	if err := plan.refresh(ctx, cli, loaded); err != nil {
		return err
	}
	for _, entry := range plan.entries {
		if entry.shutdown != nil && entry.shutdown.Changed && operationID == "" {
			return errors.New("init operation identity is required for resource shutdown")
		}
	}
	incus, _ := cli.statusPorts()
	closer, _ := incus.(orphanIngressCloser)
	for _, entry := range plan.entries {
		if len(entry.device) != 0 || entry.marker != "" {
			if closer == nil {
				return errors.New("owner Incus adapter cannot close owned resource ingress")
			}
			retainPending := entry.shutdown == nil || entry.shutdown.Changed
			if err := closer.RemoveOwnedResourceIngress(ctx, plan.yard, entry.contract, entry.device, entry.marker, retainPending); err != nil {
				return err
			}
		}
	}
	for _, entry := range plan.entries {
		if entry.shutdown != nil && entry.shutdown.Changed {
			local, _ := localResourceAction(entry.definition, entry.shutdown.Action)
			command := exec.CommandContext(ctx, entry.definition.HandlerPath(), entry.definition.Shutdown)
			configureResourceProcess(command)
			command.Dir = cli.options.WorkingDir
			command.Env = cli.resourceApplyEnvironment(loaded, entry.definition, local, operationID, entry.shutdown.Effect)
			command.Stdout = cli.options.Stdout
			command.Stderr = cli.options.Stderr
			if err := command.Run(); err != nil {
				return fmt.Errorf("run deselected resource %s shutdown: %w", entry.definition.Command, err)
			}
		}
		if entry.shutdown == nil {
			return fmt.Errorf("owned public ingress closed but guest shutdown for resource %s is unverified; start the dedicated yard, restore its profile selection if needed, run %s %s, then retry init",
				entry.definition.Command, entry.definition.Command, entry.definition.Shutdown)
		}
	}
	instance, err := incus.Instance(ctx, plan.yard.Project, plan.yard.Instance)
	if err != nil && !errors.Is(err, ports.ErrInstanceNotFound) {
		return err
	}
	var observed *ports.InstanceInfo
	if err == nil {
		observed = &instance
	}
	remaining, err := planOrphanIngress(plan.yard, observed,
		strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]), "", cli.resources.Definitions())
	if err != nil {
		return err
	}
	if remaining != nil {
		return errors.New("owned public ingress removal did not converge")
	}
	for _, entry := range plan.entries {
		if entry.shutdown == nil || !entry.shutdown.Changed {
			continue
		}
		output, err := cli.prepareResource(ctx, loaded, entry.definition, []string{entry.definition.Shutdown})
		if err != nil {
			return fmt.Errorf("verify deselected resource shutdown: %w", err)
		}
		assessment, err := cli.resources.AssessPrepareResult(cli.coreActions,
			entry.definition.Command, entry.definition.Shutdown, output)
		if err != nil || assessment.Changed {
			return fmt.Errorf("deselected resource shutdown did not converge: %v", err)
		}
	}
	return nil
}
