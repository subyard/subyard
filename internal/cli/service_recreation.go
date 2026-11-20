package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
// local runtime with the declared settings. A stopped runtime and an
// unfinished init are left to their own lifecycle.
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
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) || errors.Is(err, os.ErrNotExist) {
		return activationApplicability{state: "absent"}, nil
	}
	if err != nil {
		return activationApplicability{}, err
	}
	result := activationApplicability{state: "inactive", target: loaded.Context.YardName}
	if instance.Config["user.subyard.initialized"] == "false" {
		return result, nil
	}
	switch strings.ToUpper(instance.Status) {
	case "RUNNING":
	case "STOPPED":
		switch desired := strings.ToLower(strings.TrimSpace(instance.Config["user.subyard.desired_power"])); desired {
		case "", "stopped":
			return result, nil
		case "running":
		default:
			return activationApplicability{}, fmt.Errorf("yard %s has unsupported desired power %q", loaded.Context.YardName, desired)
		}
	default:
		return activationApplicability{}, fmt.Errorf("yard %s is in unsupported state %q", loaded.Context.YardName, instance.Status)
	}
	result.state, result.applies = "active", true
	return result, nil
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

// assess proves the exact registered owner before any effect. Ownership
// refusals leave the runtime as it is; only failed reads are errors.
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
	selection := &serviceSelection{definition: recreation.definition, record: records[0], registry: registry, publication: filepath.Join(home, service.RouteRoot, "services")}
	var findings []error
	if len(records) > 1 {
		findings = append(findings, fmt.Errorf("yard %s has %d registrations", yard, len(records)))
	}
	if selection.record.Owner != owner {
		findings = append(findings, fmt.Errorf("its registration belongs to installation %s", selection.record.Owner))
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		findings = append(findings, errors.New("its yard is not a local runtime"))
	} else if len(findings) == 0 {
		findings = append(findings, cli.checkServiceSelection(ctx, loaded, selection))
	}
	return serviceRecreationPlan{selection: selection, loaded: loaded, refusal: domain.CollectRefusals(findings...)}, nil
}

func (recreation *serviceRecreation) consequence(plan serviceRecreationPlan) string {
	return fmt.Sprintf("recreate service %s in yard %s for the new release: its VMs, leases and base images are removed; yard settings are kept", plan.selection.record.Service, plan.selection.record.Yard)
}

func (recreation *serviceRecreation) warning(yard string, refusal error) string {
	id := recreation.definition.InfrastructureService.ID
	return fmt.Sprintf("service %s in yard %s keeps its previous engine: %v; recreate it with yard service remove %s, then yard service install %s", id, yard, refusal, id, id)
}

func (recreation *serviceRecreation) run(ctx context.Context, plan serviceRecreationPlan) error {
	cli, record := recreation.cli, plan.selection.record
	if err := cli.checkServiceSelection(ctx, plan.loaded, plan.selection); err != nil {
		return err
	}
	run := recreation.runYard
	if run == nil {
		run = cli.runReleaseTransitionYardCommand
	}
	// Ordinary teardown rechecks the captured registration and retires it.
	if err := run(ctx, record.Yard, cli.options.Stderr, "teardown", "--yes"); err != nil {
		return fmt.Errorf("remove service %s runtime in yard %s: %w", record.Service, record.Yard, err)
	}
	install := []string{"service", "install", record.Service, "--yard", record.Yard}
	if record.Pool != "default" {
		install = append(install, "--pool", record.Pool)
	}
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

// bindServiceRecreation lets a new-release transition recreate drifted
// registered service runtimes instead of updating them in place.
func (cli *CLI) bindServiceRecreation(request releasetransition.ProcessRequest, reconcilers []releasetransition.V2ActivationReconciler) []releasetransition.V2ActivationReconciler {
	if !cli.newReleaseActivation(request) {
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
