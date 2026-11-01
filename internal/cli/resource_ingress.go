package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type resourceIngress struct {
	loaded   config.Loaded
	service  *yardnetwork.Service
	yards    []yardnetwork.Yard
	contexts []domain.Context
	preview  yardnetwork.IngressPreview
	up       bool
	contract resource.ProxyContract
	address  string
	port     int
}

// Only the original native startup path hands host proxy writes to the parent.
// These values are reconstructed from its retained preview, never recaptured
// from a name after guest work or partial progress.
type resourceOwnedIngress struct {
	uuid                        string
	nic, before, desired        map[string]string
	beforeMarker, desiredMarker string
	published                   bool
}

func (startup *resourceStartup) originalIngress() (*resourceOwnedIngress, error) {
	if startup.intent.instanceUUID == "" {
		return nil, nil
	}
	preview := startup.ingress.preview
	find := func(plan yardnetwork.Plan) (ports.InstanceInfo, error) {
		var found *ports.InstanceInfo
		for _, yard := range plan.Snapshot.Yards {
			if yard.Yard == preview.Target {
				if found != nil || !yard.InstanceFound || yard.InstanceInfo.Config["volatile.uuid"] != startup.intent.instanceUUID {
					return ports.InstanceInfo{}, domain.ErrPlanStale
				}
				copy := yard.InstanceInfo
				found = &copy
			}
		}
		if found == nil {
			return ports.InstanceInfo{}, domain.ErrPlanStale
		}
		return *found, nil
	}
	before, err := find(preview.Before)
	if err != nil {
		return nil, err
	}
	after, err := find(preview.After)
	if err != nil {
		return nil, err
	}
	contract := startup.ingress.contract
	if !maps.Equal(before.Devices["eth0"], after.Devices["eth0"]) {
		return nil, domain.ErrPlanStale
	}
	return &resourceOwnedIngress{uuid: startup.intent.instanceUUID, nic: maps.Clone(before.Devices["eth0"]), before: maps.Clone(before.LocalDevices[contract.Device]), desired: maps.Clone(after.LocalDevices[contract.Device]), beforeMarker: before.LocalConfig[contract.OwnershipKey()], desiredMarker: after.LocalConfig[contract.OwnershipKey()]}, nil
}

func (owned *resourceOwnedIngress) write(ctx context.Context, runner *resourceApplyRunner, rollback bool) error {
	incus, _ := runner.cli.statusPorts()
	writer, ok := incus.(interface {
		SetOwnedResourceIngressIf(context.Context, yardnetwork.Yard, resource.ProxyContract, string, map[string]string, map[string]string, string, map[string]string, string) error
	})
	if !ok {
		return errors.New("original startup ingress requires guarded Incus writer")
	}
	before, marker, desired, wanted := owned.before, owned.beforeMarker, owned.desired, owned.desiredMarker
	if rollback {
		if !owned.published {
			return nil
		}
		before, marker, desired, wanted = owned.desired, owned.desiredMarker, nil, ""
		if len(owned.before) == 0 {
			wanted = owned.beforeMarker
		}
	}
	if err := writer.SetOwnedResourceIngressIf(ctx, runner.ingress.preview.Target, runner.ingress.contract, owned.uuid, owned.nic, before, marker, desired, wanted); err != nil {
		return err
	}
	owned.published = !rollback
	return nil
}

func (cli *CLI) prepareResourceIngress(ctx context.Context, loaded config.Loaded, definition resource.Definition, verb string) (*resourceIngress, error) {
	if definition.Proxy == nil || !definition.Proxy.IsOwnerIPv4Ingress() ||
		(verb != definition.BringUp && verb != definition.Shutdown) {
		return nil, nil
	}
	if loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardKind != domain.YardVM ||
		loaded.Context.YardName == "" || loaded.Context.YardName == "default" ||
		!slices.Contains(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]), definition.Profile) {
		return nil, errors.New("public ingress requires a selected resource in a local named VM yard")
	}
	contextCLI := cli
	if cli.ownerResourceContext != nil {
		contextCLI = cli.ownerResourceContext
	}
	contexts, err := contextCLI.powerYardContexts(loaded)
	if err != nil {
		return nil, err
	}
	service := cli.resourceIngressService(loaded, contexts)
	if service == nil {
		return nil, errors.New("owner network policy adapter is unavailable")
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
			return nil, errors.New("resource owner ingress port is invalid")
		}
	}
	ingress := &resourceIngress{
		loaded: loaded, contexts: contexts,
		service: service, yards: yards, up: up, port: port, contract: *definition.Proxy,
		address: loaded.Environment[definition.Proxy.AdvertiseHostSetting],
	}
	ingress.preview, err = service.PreviewResourceIngress(ctx, yards, networkYard(loaded.Context), ingress.contract, ingress.address, ingress.port, up)
	if err != nil {
		return nil, err
	}
	return ingress, nil
}

func (cli *CLI) resourceIngressService(loaded config.Loaded, contexts []domain.Context) *yardnetwork.Service {
	contextCLI := cli
	if cli.ownerResourceContext != nil {
		contextCLI = cli.ownerResourceContext
	}
	service := contextCLI.networkService(contexts)
	if service == nil {
		return nil
	}
	if cli.options.NetworkPolicy == nil {
		service.ClearStaleUDP = func(ctx context.Context, endpoint netip.AddrPort) error {
			return clearResourceStaleUDP(ctx, endpoint, cli.options.RepositoryRoot)
		}
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
	return service
}

func (ingress *resourceIngress) augment(assessment domain.ActionAssessment) domain.ActionAssessment {
	if ingress == nil {
		return assessment
	}
	if ingress.up && ingress.preview.Protocol == "udp" && (assessment.Changed || ingress.preview.After.Changed) {
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
	guestPort := ingress.preview.GuestPort
	interruption := "update the stopped yard"
	for _, update := range ingress.preview.After.Updates {
		if strings.EqualFold(update.Yard.InstanceInfo.Status, "running") {
			interruption = "briefly restart the running yard"
		}
	}
	assessment.Consequences = append(assessment.Consequences,
		fmt.Sprintf("%s owned %s/%d ingress in yard %s's isolation ACL; %s %s; network fingerprint:%s",
			verb, strings.ToUpper(ingress.preview.Protocol), guestPort, ingress.preview.Target.Name, interruption, ingress.preview.Target.Name, ingress.preview.After.Fingerprint))
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
	if cli.resourceSourceEnvironment != nil {
		freshLoaded = ownerLoadedLabels(freshLoaded, cli.options.RepositoryRoot, ingress.loaded.Context.Paths.RepositoryRoot)
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
	if err == nil && ingress.up && ingress.preview.Protocol == "udp" {
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
	var terminal *ports.NativeOperationFailure
	if ports.NativeOperationObserverFrom(ctx) != nil && !errors.Is(err, domain.ErrPlanStale) && !errors.As(err, &terminal) {
		return fmt.Errorf("coordinate resource ingress isolation: %w", err)
	}
	if rollbackErr := ingress.rollback(ctx, runner, operationID); rollbackErr != nil {
		return fmt.Errorf("coordinate resource ingress isolation: %w; rollback ingress failed: %v", err, rollbackErr)
	}
	return fmt.Errorf("coordinate resource ingress isolation: %w; owned public ingress rolled back", err)
}

func clearResourceStaleUDP(ctx context.Context, endpoint netip.AddrPort, sourceRoot string) error {
	path, found := hostruntime.FindConntrack("", "/usr/sbin", "/sbin", "/usr/bin")
	if !found {
		return errors.New("conntrack is required for public UDP resource recovery; rerun yard init")
	}
	cleaner := hostruntime.ConntrackCleaner{Lookup: func(string) (string, error) { return path, nil }}
	cleaner.Run = func(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
		return runResourceConntrack(ctx, sourceRoot, binary, os.Geteuid() != 0, arguments...)
	}
	if ports.NativeOperationObserverFrom(ctx) != nil {
		cleaner.Delete = func(ctx context.Context, binary string, arguments []string, settle func(error) error) error {
			_, err := runResourceConntrackPostcondition(ctx, sourceRoot, binary, os.Geteuid() != 0, settle, arguments...)
			return err
		}
	}
	return cleaner.Clear(ctx, endpoint)
}

func (ingress *resourceIngress) clearStaleUDP(ctx context.Context) error {
	if ingress.preview.Protocol != "udp" {
		return nil
	}
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

// Keep the native cause available to errors.Is/As without rendering handler
// output as retry history. All diagnostic text below consists of closed classes.
type resourceShutdownProbeFailure struct {
	message string
	cause   error
}

func (failure *resourceShutdownProbeFailure) Error() string { return failure.message }
func (failure *resourceShutdownProbeFailure) Unwrap() error { return failure.cause }

func resourceShutdownProbeOutcome(err error) string {
	switch {
	case err == nil:
		return "converged"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, resource.ErrResourceUsageInvalid):
		return "usage_invalid"
	case errors.Is(err, resource.ErrResourcePlanInvalid):
		// prepareResourceModeTimeout retains its context in a closed diagnostic,
		// rather than wrapping it. Match only that complete engine-owned form;
		// arbitrary handler stderr remains classified as plan_invalid.
		prefix := resource.ErrResourcePlanInvalid.Error() + ": prepare timed out or was cancelled: "
		switch err.Error() {
		case prefix + context.DeadlineExceeded.Error():
			return "prepare_timeout"
		case prefix + context.Canceled.Error():
			return "prepare_cancelled"
		}
		return "plan_invalid"
	case errors.Is(err, domain.ErrPlanStale):
		return "plan_stale"
	default:
		return "other_error"
	}
}

// Removing an ingress ACL can restart a running VM. Wait for its agent and
// verify the resource's own read-only shutdown assessment before recording a
// disabled startup intent. Retrying preparation never retries the mutation.
func (ingress *resourceIngress) probeShutdown(ctx context.Context, runner *resourceApplyRunner) error {
	const budget = 5 * time.Minute
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var lastErr error
	var attempts int
	var firstAttempt, lastAttempt time.Duration
	firstFailure, lastOutcome := "none", "none"
	failure := func(reason, stop string, cause error) error {
		if firstFailure == "none" {
			firstFailure = lastOutcome
		}
		return &resourceShutdownProbeFailure{cause: cause, message: fmt.Sprintf(
			"%s: attempts=%d budget_ms=%d elapsed_ms=%d first_attempt_ms=%d last_attempt_ms=%d first_failure=%s last_outcome=%s stop=%s",
			reason, attempts, budget.Milliseconds(), time.Since(started).Milliseconds(), firstAttempt.Milliseconds(),
			lastAttempt.Milliseconds(), firstFailure, lastOutcome, stop)}
	}
	for {
		attempts++
		attemptStarted := time.Now()
		output, err := runner.cli.prepareResource(bounded, runner.loaded, runner.definition, []string{runner.definition.Shutdown})
		lastAttempt = time.Since(attemptStarted)
		if attempts == 1 {
			firstAttempt = lastAttempt
		}
		lastOutcome = resourceShutdownProbeOutcome(err)
		if err != nil && firstFailure == "none" {
			firstFailure = lastOutcome
		}
		if err == nil {
			assessment, assessErr := runner.cli.resources.AssessPrepareResult(
				runner.cli.coreActions, runner.definition.Command, runner.definition.Shutdown, output)
			if assessErr != nil {
				lastOutcome = "assessment_invalid"
				return failure("resource shutdown assessment failed", "refused", assessErr)
			}
			local, ok := localResourceAction(runner.definition, assessment.Action)
			if !ok || local != runner.localAction {
				lastOutcome = "action_changed"
				return failure("resource shutdown assessment changed action", "refused", domain.ErrPlanStale)
			}
			if assessment.Changed {
				lastOutcome = "runtime_remaining"
				return failure("resource shutdown did not converge after ingress reconciliation", "refused",
					errors.New("resource shutdown did not converge after ingress reconciliation"))
			}
			return nil
		}
		if errors.Is(err, resource.ErrResourceUsageInvalid) {
			return failure("resource shutdown assessment rejected request", "refused", err)
		}
		lastErr = err
		if bounded.Err() != nil {
			if ctx.Err() != nil {
				return failure("resource shutdown remains unverified after network restart", "parent_"+resourceShutdownProbeOutcome(ctx.Err()), ctx.Err())
			}
			return failure("resource shutdown remains unverified after network restart", "verification_deadline", lastErr)
		}
		select {
		case <-bounded.Done():
			if ctx.Err() != nil {
				return failure("resource shutdown remains unverified after network restart", "parent_"+resourceShutdownProbeOutcome(ctx.Err()), ctx.Err())
			}
			return failure("resource shutdown remains unverified after network restart", "verification_deadline", lastErr)
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
		release := func() {}
		if runner.effect != domain.ActionSession {
			var err error
			release, err = runner.cli.resourceOwnerBarrier(probeCtx, runner.loaded, probe)
			if err != nil {
				cancel()
				return err
			}
		}
		probeErr := probe.Run()
		release()
		cancel()
		if probeErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("public resource is not ready after network reconciliation")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (ingress *resourceIngress) rollback(parent context.Context, runner *resourceApplyRunner, operationID string) error {
	if ingress == nil || !ingress.up {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancel()
	if runner.ownedIngress != nil {
		if err := runner.ownedIngress.write(ctx, runner, true); err != nil {
			return fmt.Errorf("close original startup ingress: %w", err)
		}
	}
	command := exec.CommandContext(ctx, runner.definition.HandlerPath(), "rollback-ingress")
	configureResourceProcess(command)
	command.Dir = runner.cli.options.WorkingDir
	command.Env = runner.cli.resourceApplyEnvironment(runner.loaded, runner.definition, runner.localAction,
		operationID, runner.effect)
	if runner.ownedIngress != nil {
		command.Env = environmentList(environmentMap(command.Env), map[string]string{"SUBYARD_RESOURCE_INGRESS_OWNER": "parent", "SUBYARD_RESOURCE_INSTANCE_UUID": runner.ownedIngress.uuid})
	}
	if runner.effect != domain.ActionSession {
		release, err := runner.cli.resourceOwnerBarrier(ctx, runner.loaded, command)
		if err != nil {
			return err
		}
		defer release()
	}
	command.Stdout = runner.cli.options.Stdout
	command.Stderr = runner.cli.options.Stderr
	if err := shelladapter.RunNativeCommand(ctx, command, nil, nil, nil); err != nil {
		return fmt.Errorf("run owned ingress rollback: %w", err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
	defer cleanupCancel()
	pending := ""
	if runner.ownedIngress != nil && len(runner.ownedIngress.before) == 0 {
		pending = runner.ownedIngress.beforeMarker
	}
	uuid := ""
	if runner.ownedIngress != nil {
		uuid = runner.ownedIngress.uuid
	}
	if err := ingress.cleanupRollbackACL(cleanupCtx, pending, uuid); err != nil {
		return fmt.Errorf("verify owned public ingress rollback and clean isolation ACL: %w", err)
	}
	return nil
}

func (ingress *resourceIngress) cleanupRollbackACL(ctx context.Context, originalPending, originalUUID string) error {
	service := *ingress.service
	if originalUUID != "" {
		service.Host = resourceOriginalNetworkHost{Host: service.Host, target: ingress.preview.Target, uuid: originalUUID}
	}
	closure, err := service.PreviewResourceIngress(ctx, ingress.yards,
		ingress.preview.Target, ingress.contract, "", 0, false)
	if err != nil {
		return err
	}
	if err := ingress.preview.ValidateRollbackOriginalPending(closure, ingress.contract, originalPending); err != nil {
		return err
	}
	actual := closure.Before
	if !actual.Changed {
		return nil
	}
	if err := service.Apply(ctx, actual); err != nil {
		return err
	}
	verified, err := service.PreviewResourceIngress(ctx, ingress.yards,
		ingress.preview.Target, ingress.contract, "", 0, false)
	if err != nil {
		return err
	}
	if err := ingress.preview.ValidateRollbackOriginalPending(verified, ingress.contract, originalPending); err != nil {
		return err
	}
	if verified.Before.Changed {
		return errors.New("resource ingress isolation ACL did not converge after rollback")
	}
	return nil
}
