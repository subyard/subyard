package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type integrationSettingsCapture struct {
	loaded   config.Loaded
	prepared ports.PreparedSettingsCapture
}

func (cli *CLI) prepareIntegrationSettingsCaptures(ctx context.Context, loaded config.Loaded, request configAuthoringRequest, prospective *configProspective) ([]integrationSettingsCapture, error) {
	if !loaded.Catalog.IntegrationInitSetting(request.name) {
		return nil, nil
	}
	names := []string{loaded.Context.YardName}
	if request.scope != config.ScopeYard {
		var err error
		names, err = config.YardNames(loaded.Context.Paths.ConfigDir, loaded.Context.Paths.ConfigHome)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, "default") {
			names = append(names, "default")
		}
		slices.Sort(names)
	}
	environment := maps.Clone(cli.baseEnv)
	for name := range environment {
		if _, known := loaded.Catalog.LookupSetting(name); known || config.IsLegacySetting(name) {
			delete(environment, name)
		}
	}
	environment["SUBYARD_CONFIG_HOME"], environment["SUBYARD_HOME"] = loaded.Context.Paths.ConfigHome, loaded.Context.Paths.DataHome
	var captures []integrationSettingsCapture
	var failures []error
	for _, name := range names {
		options := config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: loaded.Context.Paths.OperatorHome, YardName: name, Environment: environment}
		before, err := config.Load(options)
		if err != nil {
			failures = append(failures, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		options.FileOverrides = map[string]string{prospective.path: prospective.draft}
		after, err := config.Load(options)
		if err != nil {
			failures = append(failures, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		if before.Context.AccessKind != domain.AccessLocal || before.Environment[request.name] == after.Environment[request.name] {
			continue
		}
		if strings.HasPrefix(request.name, "AGENT_") {
			affected := false
			for _, id := range before.Integrations.Effective {
				affected = affected || strings.HasPrefix(request.name, "AGENT_"+id+"_")
			}
			if !affected {
				continue
			}
		}
		platform, ok := cli.initPlatform(before, nil).(interface {
			PrepareAppliedIntegrationSettings(context.Context) (ports.PreparedSettingsCapture, error)
		})
		if !ok {
			continue
		}
		incus, _ := cli.statusPorts()
		if probe, ok := incus.(interface{ LocalInstallationAbsent() bool }); ok && probe.LocalInstallationAbsent() {
			continue
		}
		instance, err := incus.Instance(ctx, before.Context.IncusProject, before.Context.YardInstanceName)
		if errors.Is(err, ports.ErrInstanceNotFound) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		if !strings.EqualFold(instance.Status, "running") {
			failures = append(failures, fmt.Errorf("yard %s: applied integration settings cannot be verified while stopped; start the yard before editing %s", name, request.name))
			continue
		}
		if request.name == "DEV_USER" || request.name == "DEV_UID" {
			if _, _, err := config.ReadAppliedIdentityHint(before.Context.Paths.ConfigHome, name); err != nil {
				failures = append(failures, fmt.Errorf("yard %s: %w", name, err))
				continue
			}
			if checker, ok := platform.(interface{ CheckAppliedGuestIdentity(context.Context) error }); ok {
				if err := checker.CheckAppliedGuestIdentity(ctx); err != nil {
					failures = append(failures, fmt.Errorf("yard %s: %w", name, err))
					continue
				}
			}
		}
		capture, err := platform.PrepareAppliedIntegrationSettings(ctx)
		if err != nil {
			failures = append(failures, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		captures = append(captures, integrationSettingsCapture{loaded: before, prepared: capture})
	}
	return captures, errors.Join(failures...)
}

func lockIntegrationSettingsCaptures(ctx context.Context, loaded config.Loaded, yardLocked bool, captures []integrationSettingsCapture) (func(), error) {
	var unlocks []func()
	unlock := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, capture := range captures {
		if yardLocked && capture.loaded.Context.YardName == loaded.Context.YardName {
			continue
		}
		release, err := lockIntegrationYard(ctx, capture.loaded)
		if err != nil {
			unlock()
			return nil, err
		}
		unlocks = append(unlocks, release)
	}
	return unlock, nil
}
