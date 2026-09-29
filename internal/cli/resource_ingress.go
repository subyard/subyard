package cli

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type resourceIngress struct {
	loaded   config.Loaded
	service  *yardnetwork.Service
	yards    []yardnetwork.Yard
	preview  yardnetwork.IngressPreview
	up       bool
	contract resource.ProxyContract
	address  string
	port     int
}

func (cli *CLI) prepareResourceIngress(ctx context.Context, loaded config.Loaded, definition resource.Definition, verb string) (*resourceIngress, error) {
	if definition.Proxy == nil || definition.Proxy.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP ||
		(verb != definition.BringUp && verb != definition.Shutdown) {
		return nil, nil
	}
	if loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardKind != domain.YardVM ||
		loaded.Context.YardName == "" || loaded.Context.YardName == "default" ||
		!slices.Contains(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]), definition.Profile) {
		return nil, errors.New("public UDP ingress requires a selected resource in a local named VM yard")
	}
	contexts, err := cli.powerYardContexts(loaded)
	if err != nil {
		return nil, err
	}
	service := cli.networkService(contexts)
	if service == nil {
		return nil, errors.New("owner network policy adapter is unavailable")
	}
	if cli.options.NetworkPolicy == nil {
		service.ClearStaleUDP = clearResourceStaleUDP
	}
	// Use the prepared selection for the target yard while resolving other
	// yards from their own persisted configuration.
	selectedSource := service.ContractSource
	service.ContractSource = func(yard yardnetwork.Yard) ([]resource.ProxyContract, error) {
		if yard == networkYard(loaded.Context) {
			return cli.resources.ProxyContracts(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])), nil
		}
		if selectedSource == nil {
			return nil, nil
		}
		return selectedSource(yard)
	}
	yards := make([]yardnetwork.Yard, 0, len(contexts))
	for _, selected := range contexts {
		yards = append(yards, networkYard(selected))
	}
	up := verb == definition.BringUp
	port := 0
	if up {
		port, err = strconv.Atoi(loaded.Environment[definition.Proxy.HostPortSetting])
		if err != nil {
			return nil, errors.New("resource owner UDP port is invalid")
		}
	}
	ingress := &resourceIngress{
		loaded:  loaded,
		service: service, yards: yards, up: up, port: port, contract: *definition.Proxy,
		address: loaded.Environment[definition.Proxy.AdvertiseHostSetting],
	}
	ingress.preview, err = service.PreviewResourceIngress(ctx, yards, networkYard(loaded.Context), ingress.contract, ingress.address, ingress.port, up)
	if err != nil {
		return nil, err
	}
	return ingress, nil
}

func (ingress *resourceIngress) augment(assessment domain.ActionAssessment) domain.ActionAssessment {
	if ingress == nil {
		return assessment
	}
	if ingress.up && (assessment.Changed || ingress.preview.After.Changed) {
		assessment.Consequences = append(assessment.Consequences,
			"clear stale UDP connection state for the exact approved owner endpoint")
	}
	if !ingress.preview.After.Changed {
		return assessment
	}
	verb := "remove"
	if ingress.up {
		verb = "allow"
	}
	assessment.Changed = true
	guestPort, _ := ingress.contract.GuestUDPPort()
	interruption := "update the stopped yard"
	for _, update := range ingress.preview.After.Updates {
		if strings.EqualFold(update.Yard.InstanceInfo.Status, "running") {
			interruption = "briefly restart the running yard"
		}
	}
	assessment.Consequences = append(assessment.Consequences,
		fmt.Sprintf("%s owned UDP/%d ingress in yard %s's isolation ACL; %s %s; network fingerprint:%s",
			verb, guestPort, ingress.preview.Target.Name, interruption, ingress.preview.Target.Name, ingress.preview.After.Fingerprint))
	return assessment
}

func (ingress *resourceIngress) refresh(ctx context.Context, cli *CLI) error {
	if ingress == nil {
		return nil
	}
	freshLoaded, err := config.Load(config.LoadOptions{
		Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome: ingress.loaded.Context.Paths.OperatorHome,
		YardName:     ingress.loaded.Context.YardName,
		Environment:  cli.baseEnv,
	})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(freshLoaded.Context, ingress.loaded.Context) ||
		len(freshLoaded.Settings) != len(ingress.loaded.Settings) {
		return fmt.Errorf("%w: resource yard configuration changed after confirmation", domain.ErrPlanStale)
	}
	for name, original := range ingress.loaded.Settings {
		if freshLoaded.Settings[name].EffectiveValue != original.EffectiveValue {
			return fmt.Errorf("%w: resource yard setting %s changed after confirmation", domain.ErrPlanStale, name)
		}
	}
	if freshLoaded.Environment["ENVIRONMENT_PROFILES"] != ingress.loaded.Environment["ENVIRONMENT_PROFILES"] ||
		freshLoaded.Environment["YARD_TEMPLATE"] != ingress.loaded.Environment["YARD_TEMPLATE"] ||
		freshLoaded.Environment[ingress.contract.AdvertiseHostSetting] != ingress.address ||
		freshLoaded.Environment[ingress.contract.HostPortSetting] != ingress.loaded.Environment[ingress.contract.HostPortSetting] ||
		freshLoaded.Environment[ingress.contract.OwnerInterfaceSetting] != ingress.loaded.Environment[ingress.contract.OwnerInterfaceSetting] {
		return fmt.Errorf("%w: resource ingress selection or endpoint changed after confirmation", domain.ErrPlanStale)
	}
	fresh, err := ingress.service.PreviewResourceIngress(ctx, ingress.yards, ingress.preview.Target, ingress.contract, ingress.address, ingress.port, ingress.up)
	if err != nil {
		return err
	}
	if fresh.Before.Fingerprint != ingress.preview.Before.Fingerprint ||
		fresh.After.Fingerprint != ingress.preview.After.Fingerprint {
		return domain.ErrPlanStale
	}
	return nil
}

func (ingress *resourceIngress) apply(ctx context.Context, runner *resourceApplyRunner, operationID string) error {
	if ingress == nil {
		return nil
	}
	actual, err := ingress.service.Prepare(ctx, ingress.yards, yardnetwork.Change{})
	if err == nil {
		err = ingress.preview.ValidateAfter(actual)
	}
	if err == nil && actual.Changed {
		err = ingress.service.Apply(ctx, actual)
	}
	if err == nil {
		var verified yardnetwork.Plan
		verified, err = ingress.service.Prepare(ctx, ingress.yards, yardnetwork.Change{})
		if err == nil && verified.Changed {
			err = errors.New("resource ingress network policy did not converge")
		}
	}
	if err == nil && ingress.up {
		err = ingress.probeReady(ctx, runner)
	}
	if err == nil && ingress.up {
		err = ingress.clearStaleUDP(ctx)
	}
	if err == nil && !ingress.up {
		for _, update := range actual.Updates {
			if update.Yard.Yard == ingress.preview.Target && strings.EqualFold(update.Yard.InstanceInfo.Status, "running") {
				err = ingress.probeShutdown(ctx, runner)
				break
			}
		}
	}
	if err == nil {
		return nil
	}
	if !ingress.up {
		return fmt.Errorf("coordinate resource ingress isolation: %w", err)
	}
	if rollbackErr := ingress.rollback(runner, operationID); rollbackErr != nil {
		return fmt.Errorf("coordinate resource ingress isolation: %w; rollback ingress failed: %v", err, rollbackErr)
	}
	return fmt.Errorf("coordinate resource ingress isolation: %w; owned public ingress rolled back", err)
}

func clearResourceStaleUDP(ctx context.Context, endpoint netip.AddrPort) error {
	path, found := hostruntime.FindConntrack("", "/usr/sbin", "/sbin", "/usr/bin")
	if !found {
		return errors.New("conntrack is required for public UDP resource recovery; rerun yard init")
	}
	cleaner := hostruntime.ConntrackCleaner{Lookup: func(string) (string, error) { return path, nil }}
	if os.Geteuid() != 0 {
		cleaner.Run = func(ctx context.Context, _ string, arguments ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "sudo", append([]string{"-n", "--", path}, arguments...)...).Output()
		}
	}
	return cleaner.Clear(ctx, endpoint)
}

func (ingress *resourceIngress) clearStaleUDP(ctx context.Context) error {
	if ingress.service.ClearStaleUDP == nil {
		return errors.New("public UDP conntrack cleaner is unavailable")
	}
	owner, err := netip.ParseAddr(ingress.address)
	if err != nil || !resource.ExplicitOwnerIPv4(owner) || ingress.port < 1 || ingress.port > 65535 {
		return errors.New("public UDP resource endpoint is invalid")
	}
	expected := netip.AddrPortFrom(owner, uint16(ingress.port))
	verified := *ingress.service
	verified.UseApprovedIngress = true
	verified.ContractSource = nil
	seen := false
	verified.ClearStaleUDP = func(ctx context.Context, endpoint netip.AddrPort) error {
		if endpoint != expected {
			return nil
		}
		seen = true
		return ingress.service.ClearStaleUDP(ctx, endpoint)
	}
	if verified.Lock == nil {
		return errors.New("host network policy lock is required for UDP recovery")
	}
	release, err := verified.Lock.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := verified.ClearBootStaleUDP(ctx, ingress.preview.Target); err != nil {
		return err
	}
	if !seen {
		return errors.New("approved public UDP resource endpoint was not found")
	}
	return nil
}

// Removing an ingress ACL can restart a running VM. Wait for its agent and
// verify the resource's own read-only shutdown assessment before recording a
// disabled startup intent. Retrying preparation never retries the mutation.
func (ingress *resourceIngress) probeShutdown(ctx context.Context, runner *resourceApplyRunner) error {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var lastErr error
	for {
		output, err := runner.cli.prepareResource(bounded, runner.loaded, runner.definition, []string{runner.definition.Shutdown})
		if err == nil {
			assessment, assessErr := runner.cli.resources.AssessPrepareResult(
				runner.cli.coreActions, runner.definition.Command, runner.definition.Shutdown, output)
			if assessErr != nil {
				return assessErr
			}
			local, ok := localResourceAction(runner.definition, assessment.Action)
			if !ok || local != runner.localAction {
				return domain.ErrPlanStale
			}
			if assessment.Changed {
				return errors.New("resource shutdown did not converge after ingress reconciliation")
			}
			return nil
		}
		if errors.Is(err, resource.ErrResourceUsageInvalid) {
			return err
		}
		lastErr = err
		if bounded.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("resource shutdown remains unverified after network restart: %w", lastErr)
		}
		select {
		case <-bounded.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("resource shutdown remains unverified after network restart: %w", lastErr)
		case <-time.After(2 * time.Second):
		}
	}
}

func (ingress *resourceIngress) probeReady(ctx context.Context, runner *resourceApplyRunner) error {
	// Ingress ACL reconciliation may restart the VM. Match the normal VM
	// agent boot allowance before requiring the guest resource to be ready.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		probe := exec.CommandContext(probeCtx, runner.definition.HandlerPath(), "is-up")
		configureResourceProcess(probe)
		probe.Dir = runner.cli.options.WorkingDir
		probe.Env = runner.cli.resourceEnvironment(runner.loaded, runner.definition, "", "", "")
		probeErr := probe.Run()
		cancel()
		if probeErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("public UDP resource is not ready after network reconciliation")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (ingress *resourceIngress) rollback(runner *resourceApplyRunner, operationID string) error {
	if ingress == nil || !ingress.up {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, runner.definition.HandlerPath(), "rollback-ingress")
	configureResourceProcess(command)
	command.Dir = runner.cli.options.WorkingDir
	command.Env = runner.cli.resourceApplyEnvironment(runner.loaded, runner.definition, runner.localAction,
		operationID, runner.effect)
	command.Stdout = runner.cli.options.Stdout
	command.Stderr = runner.cli.options.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run owned ingress rollback: %w", err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cleanupCancel()
	if err := ingress.cleanupRollbackACL(cleanupCtx); err != nil {
		return fmt.Errorf("verify owned public ingress rollback and clean isolation ACL: %w", err)
	}
	return nil
}

func (ingress *resourceIngress) cleanupRollbackACL(ctx context.Context) error {
	closure, err := ingress.service.PreviewResourceIngress(ctx, ingress.yards,
		ingress.preview.Target, ingress.contract, "", 0, false)
	if err != nil {
		return err
	}
	if err := ingress.preview.ValidateRollback(closure, ingress.contract); err != nil {
		return err
	}
	actual := closure.Before
	if !actual.Changed {
		return nil
	}
	if err := ingress.service.Apply(ctx, actual); err != nil {
		return err
	}
	verified, err := ingress.service.PreviewResourceIngress(ctx, ingress.yards,
		ingress.preview.Target, ingress.contract, "", 0, false)
	if err != nil {
		return err
	}
	if err := ingress.preview.ValidateRollback(verified, ingress.contract); err != nil {
		return err
	}
	if verified.Before.Changed {
		return errors.New("resource ingress isolation ACL did not converge after rollback")
	}
	return nil
}
