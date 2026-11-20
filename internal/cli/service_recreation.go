package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// newReleaseActivation reports whether a request activates another release, or
// resumes the exact incomplete cross-release journal after that activation.
// Readiness, same-release repair and config gates keep the native owners.
func (cli *CLI) newReleaseActivation(request releasetransition.ProcessRequest) bool {
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return false
	}
	active, err := links.Observe()
	if err != nil || request.Direction != releasetransition.DirectionActivateTarget {
		return false
	}
	if active.Active != request.Target {
		return true
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	return err == nil && journal.Checkpoint != releasetransition.JournalComplete &&
		journal.Goal == (releasetransition.Goal{Target: request.Target, Direction: request.Direction}) &&
		journal.Releases.From != journal.Releases.Target
}

// activationService returns the profile whose infrastructure service binds the
// native stage owner with this activation ID.
func (cli *CLI) activationService(activation string) (profile.Definition, bool) {
	for _, definition := range cli.catalog.Profiles() {
		if service := definition.InfrastructureService; service != nil && service.RegistrationActivation == activation {
			return definition, true
		}
	}
	return profile.Definition{}, false
}

// serviceStageApplicability observes the service default yard when it is a
// local runtime with the declared settings. A stopped runtime without running
// intent and an unfinished init are left to their own lifecycle. Settings
// without a finished runtime carry a warning that names the reinstall.
func (cli *CLI) serviceStageApplicability(ctx context.Context, activation string, names []string, load func(string) (config.Loaded, error)) (activationApplicability, error) {
	definition, ok := cli.activationService(activation)
	if !ok || !slices.Contains(names, definition.InfrastructureService.DefaultYard) {
		return activationApplicability{state: "absent"}, nil
	}
	loaded, err := load(definition.InfrastructureService.DefaultYard)
	if err != nil {
		return activationApplicability{}, err
	}
	if loaded.Context.AccessKind != domain.AccessLocal || !definition.InfrastructureService.Matches(loaded.Environment) {
		return activationApplicability{state: "absent"}, nil
	}
	yard := loaded.Context.YardName
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		warning, err := cli.serviceRuntimeMissing(definition, yard)
		if err != nil {
			return activationApplicability{}, err
		}
		return activationApplicability{state: "absent", warnings: []string{warning}}, nil
	}
	if err != nil && ports.LocalInstallationAbsent(incus) {
		return activationApplicability{state: "absent"}, nil
	}
	if err != nil {
		return activationApplicability{}, err
	}
	result := activationApplicability{state: "inactive", target: yard}
	if instanceUninitialized(configTarget{Name: yard}, instance) {
		service := definition.InfrastructureService
		result.warnings = []string{fmt.Sprintf("service %s in yard %s has not finished installing; run %s", service.ID, yard, serviceInstallCommand(service.ID, yard, "default"))}
		return result, nil
	}
	switch {
	case strings.EqualFold(instance.Status, "running"), application.InstanceStoppedForRunningIntent(instance):
	case strings.EqualFold(instance.Status, "stopped"):
		return result, nil
	default:
		return activationApplicability{}, fmt.Errorf("yard %s is in unsupported state %q", yard, instance.Status)
	}
	result.state, result.applies = "active", true
	return result, nil
}

// serviceRuntimeMissing names the commands that reinstall a service runtime
// whose settings remain; a registration left by an interrupted teardown is
// removed first.
func (cli *CLI) serviceRuntimeMissing(definition profile.Definition, yard string) (string, error) {
	service := definition.InfrastructureService
	home, _, _, err := cli.servicePaths()
	if err != nil {
		return "", err
	}
	records, err := (ownerservice.Registry{Root: filepath.Join(home, "services")}).List(service.ID)
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.Yard == yard {
			return fmt.Sprintf("service %s has no runtime in yard %s but keeps its registration; run %s, then %s", service.ID, yard, serviceRemoveCommand(service.ID, record.Pool), serviceInstallCommand(service.ID, yard, record.Pool)), nil
		}
	}
	return fmt.Sprintf("service %s has no runtime in yard %s; run %s", service.ID, yard, serviceInstallCommand(service.ID, yard, "default")), nil
}

func serviceInstallArguments(id, yard, pool string) []string {
	arguments := []string{"service", "install", id, "--yard", yard}
	if pool != "default" {
		arguments = append(arguments, "--pool", pool)
	}
	return arguments
}

func serviceInstallCommand(id, yard, pool string) string {
	return "yard " + strings.Join(serviceInstallArguments(id, yard, pool), " ")
}

func serviceRemoveCommand(id, pool string) string {
	if pool != "default" {
		return "yard service remove " + id + " --pool " + pool
	}
	return "yard service remove " + id
}

// serviceRecreation replaces the in-place update of a registered service
// runtime with ordinary teardown and service install. Without a registration
// the native owner updates the runtime in place and registers it afterwards.
type serviceRecreation struct {
	cli        *CLI
	configHome string
	definition profile.Definition
	runYard    func(context.Context, string, io.Writer, ...string) error
}

// serviceRecreationPlan is empty for an unregistered runtime. A refusal keeps
// the runtime and its previous engine.
type serviceRecreationPlan struct {
	selection *serviceSelection
	loaded    config.Loaded
	refusal   error
}

// serviceRuntimeStopper stops a runtime whose session loss the approved plan
// disclosed. The guard still fences new SSH connections first.
type serviceRuntimeStopper interface {
	RunApprovedMigrationStop(context.Context) error
}

// assess proves the exact registered owner before any effect. Each ownership
// finding is collected independently and leaves the runtime as it is; a
// failed read is an error that blocks the stage for a retry.
func (recreation *serviceRecreation) assess(ctx context.Context, yard string) (serviceRecreationPlan, error) {
	cli, service := recreation.cli, recreation.definition.InfrastructureService
	home, _, owner, err := cli.servicePaths()
	if err != nil {
		return serviceRecreationPlan{}, err
	}
	registry := ownerservice.Registry{Root: filepath.Join(home, "services")}
	records, err := registry.List(service.ID)
	if err != nil {
		return serviceRecreationPlan{}, err
	}
	records = slices.DeleteFunc(records, func(record ownerservice.Registration) bool { return record.Yard != yard })
	if len(records) == 0 {
		return serviceRecreationPlan{}, nil
	}
	loaded, err := cli.resolveReleaseTransitionContext(ctx, yard, recreation.configHome)
	if err != nil {
		return serviceRecreationPlan{}, err
	}
	record := records[0]
	selection := &serviceSelection{definition: recreation.definition, record: record, registry: registry, publication: filepath.Join(home, service.RouteRoot, "services")}
	var findings []error
	if len(records) > 1 {
		findings = append(findings, fmt.Errorf("yard %s has %d registrations", yard, len(records)))
	}
	if record.Owner != owner {
		findings = append(findings, fmt.Errorf("its registration belongs to installation %s", record.Owner))
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		findings = append(findings, errors.New("its yard is not a local runtime"))
	} else {
		uuid, ownership, err := cli.serviceRuntime(ctx, loaded, recreation.definition)
		var unproven serviceRuntimeUnproven
		switch {
		case errors.As(err, &unproven):
			findings = append(findings, err)
		case err != nil:
			return serviceRecreationPlan{}, err
		case uuid != record.UUID || ownership != record.Ownership || loaded.Context.IncusProject != record.Project || loaded.Context.YardInstanceName != record.Instance:
			findings = append(findings, errors.New("its infrastructure service runtime changed"))
		}
	}
	return serviceRecreationPlan{selection: selection, loaded: loaded, refusal: domain.CollectRefusals(findings...)}, nil
}

func (recreation *serviceRecreation) consequence(plan serviceRecreationPlan) string {
	record := plan.selection.record
	return fmt.Sprintf("recreate service %s in yard %s for the new release: its open SSH sessions are interrupted; teardown deletes its instance, Incus project and /srv volume with their VMs, leases and base images, and also the shared bridge, storage pool and power reconciler when it is the last managed yard; yard settings are kept and the runtime gets a new UUID", record.Service, record.Yard)
}

func (recreation *serviceRecreation) warning(yard string, refusal error) string {
	id := recreation.definition.InfrastructureService.ID
	return fmt.Sprintf("service %s in yard %s keeps its previous engine: %v; recreate it with %s, then %s", id, yard, refusal, serviceRemoveCommand(id, "default"), serviceInstallCommand(id, yard, "default"))
}

// run rechecks the exact owner, stops the runtime with its disclosed session
// loss, then removes and reinstalls it through the ordinary commands.
func (recreation *serviceRecreation) run(ctx context.Context, plan serviceRecreationPlan, platform ports.ReconcileStageRunner) error {
	cli, record := recreation.cli, plan.selection.record
	if err := cli.checkServiceSelection(ctx, plan.loaded, plan.selection); err != nil {
		return err
	}
	stopper, ok := platform.(serviceRuntimeStopper)
	if !ok {
		return errors.New("the approved service runtime stop is unavailable")
	}
	// Teardown refuses live SSH sessions; the approved plan already ends them.
	if err := stopper.RunApprovedMigrationStop(ctx); err != nil {
		return fmt.Errorf("stop service %s runtime in yard %s: %w", record.Service, record.Yard, err)
	}
	run := recreation.runYard
	if run == nil {
		run = cli.runReleaseTransitionYardCommand
	}
	// Ordinary teardown rechecks the captured registration and retires it.
	if err := run(ctx, record.Yard, cli.options.Stderr, "teardown", "--yes"); err != nil {
		return fmt.Errorf("remove service %s runtime in yard %s: %w", record.Service, record.Yard, err)
	}
	install := serviceInstallArguments(record.Service, record.Yard, record.Pool)
	if err := run(ctx, record.Yard, cli.options.Stderr, append(install, "--yes")...); err != nil {
		return serviceReinstallError{command: "yard " + strings.Join(install, " "), yard: record.Yard, err: err}
	}
	return nil
}

type serviceReinstallError struct {
	command, yard string
	err           error
}

func (failure serviceReinstallError) Error() string {
	return fmt.Sprintf("reinstall service runtime in yard %s: %v", failure.yard, failure.err)
}
func (failure serviceReinstallError) Unwrap() error { return failure.err }
func (failure serviceReinstallError) ActivationDiagnostic() (string, string) {
	return "the service runtime in yard " + failure.yard + " was removed for recreation but its install did not finish", failure.command
}

// bindServiceRecreation lets every forward activation, including readiness
// and same-release gates, recreate drifted registered service runtimes
// instead of updating them in place. A refused recreation therefore stays
// refused. Rollback activates an older engine, which updates in place.
func (cli *CLI) bindServiceRecreation(request releasetransition.ProcessRequest, reconcilers []releasetransition.V2ActivationReconciler) []releasetransition.V2ActivationReconciler {
	if request.Direction == releasetransition.DirectionActivatePrevious {
		return reconcilers
	}
	for _, owner := range reconcilers {
		stage, ok := owner.(*activationStageReconciler)
		if !ok {
			continue
		}
		if definition, declared := cli.activationService(stage.ID()); declared {
			stage.recreation = &serviceRecreation{cli: cli, configHome: request.ConfigHome, definition: definition}
		}
	}
	return reconcilers
}

// legacyTestYardName is the pre-v0.11 test-yard name. Release work never loads
// it and its init refuses; the operator recreates it explicitly.
const (
	legacyTestYardName       = "e2e-yard"
	legacyTestYardRecreation = "yard -Y e2e-yard teardown --reset-config, then yard service install test-vms"
	legacyTestYardWarning    = legacyTestYardName + " requires recreation and is not updated; preserve other sessions' VMs; run " + legacyTestYardRecreation
)
