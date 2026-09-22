package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// This is a prepared write to the existing yard config, not another desired store.
type initIntegrationSelection struct {
	write          *config.YardIntegrationWrite
	path           string
	before         config.PersistentFileSnapshot
	original       config.IntegrationSelection
	instanceExists *bool
}

func (cli *CLI) prepareInitIntegrationSelection(ctx context.Context, loaded config.Loaded, bootstrap *initBootstrap) (config.Loaded, *initIntegrationSelection, error) {
	selection := loaded.Integrations
	path, err := configScalarAuthoringPath(loaded, config.ScopeYard)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	snapshot, err := readInitSelectionSnapshot(loaded.Context.Paths.ConfigHome, path)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	content := snapshot.Content
	if bootstrap != nil {
		content = bootstrap.content
	}
	assignments, err := config.ParsePersistentAssignments(path, content)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	explicitCanonical := false
	for _, assignment := range assignments {
		explicitCanonical = explicitCanonical || assignment.Name == "CODING_TOOL_INTEGRATIONS"
	}
	if explicitCanonical && bootstrap == nil {
		return loaded, nil, nil
	}
	if _, registered, err := configsync.ReadSourceRecord(loaded.Context.Paths.ConfigHome); err != nil {
		return config.Loaded{}, nil, err
	} else if registered {
		return config.Loaded{}, nil, errors.New("integration selection is source-managed; set CODING_TOOL_INTEGRATIONS in the selected yard's source, sync, then run init")
	}
	if explicitCanonical {
		return loaded, nil, nil
	}
	if selection.Provenance.Scope == "command" {
		return config.Loaded{}, nil, errors.New("cannot adopt temporary CODING_TOOL_INTEGRATIONS or AGENTS command overrides; set the selected yard's persistent selection first")
	}
	desired := selection.Requested
	var instanceExists *bool
	explicitYard := selection.Provenance.Scope == "yard" && selection.Provenance.Role == "scalar settings"
	if !selection.AllowsCodingTools {
		desired = []string{}
	} else if !explicitYard && loaded.Context.YardName != "default" && loaded.Context.YardName != "" {
		if bootstrap != nil {
			desired = []string{}
		} else {
			exists, err := cli.initSelectionInstanceExists(ctx, loaded.Context)
			if err != nil {
				return config.Loaded{}, nil, fmt.Errorf("cannot establish existing integration intent: %w; explicitly configure CODING_TOOL_INTEGRATIONS for this yard", err)
			}
			instanceExists = &exists
			if !exists {
				desired = []string{}
			} else if !selection.Present {
				return config.Loaded{}, nil, errors.New("existing yard has no trustworthy integration selection; explicitly configure CODING_TOOL_INTEGRATIONS")
			}
		}
	} else if !selection.Present {
		return config.Loaded{}, nil, errors.New("yard has no trustworthy integration selection; explicitly configure CODING_TOOL_INTEGRATIONS")
	}
	value := strings.Join(desired, " ")
	candidate, err := config.EditPersistentAssignmentContent(path, content, "CODING_TOOL_INTEGRATIONS", &value)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	if bytes.Equal(candidate, content) {
		return loaded, nil, nil
	}
	proposed, err := config.WithIntegrationSelection(loaded, desired)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	if bootstrap != nil {
		bootstrap.content = candidate
		// Bootstrap creation already has its own exact absent-target guard.
		return proposed, &initIntegrationSelection{path: path, before: snapshot, original: selection, instanceExists: instanceExists}, nil
	}
	write, err := config.PlanYardIntegrationWrite(loaded, desired)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	return proposed, &initIntegrationSelection{write: write, path: path, before: snapshot, original: selection, instanceExists: instanceExists}, nil
}

func (cli *CLI) initSelectionInstanceExists(ctx context.Context, yard domain.Context) (bool, error) {
	// Test platforms may supply the same read-only observation without a live daemon.
	if observer, ok := cli.options.InitPlatform.(interface {
		InstanceExists(context.Context) (bool, error)
	}); ok {
		return observer.InstanceExists(ctx)
	}
	incus, _ := cli.statusPorts()
	_, err := incus.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (selection *initIntegrationSelection) check(ctx context.Context, cli *CLI, execution *initExecution) error {
	if selection == nil {
		return nil
	}
	if selection.write != nil {
		if err := selection.write.Check(); err != nil {
			return err
		}
	}
	current, err := readInitSelectionSnapshot(execution.loaded.Context.Paths.ConfigHome, selection.path)
	if err != nil {
		return err
	}
	if !sameConfigAuthoringSnapshot(current, selection.before) || current.Identity != selection.before.Identity {
		return fmt.Errorf("%w: yard integration settings changed", domain.ErrPlanStale)
	}
	if _, registered, err := configsync.ReadSourceRecord(execution.loaded.Context.Paths.ConfigHome); err != nil {
		return err
	} else if registered {
		return fmt.Errorf("%w: integration selection became source-managed", domain.ErrPlanStale)
	}
	options := config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: execution.loaded.Context.Paths.OperatorHome, YardName: execution.loaded.Context.YardName, Environment: cli.baseEnv}
	if execution.bootstrap != nil {
		options.YardSettingsFile = execution.bootstrap.sourcePath
	}
	currentLoaded, err := config.Load(options)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currentLoaded.Integrations, selection.original) {
		return fmt.Errorf("%w: integration selection or dependencies changed", domain.ErrPlanStale)
	}
	if selection.instanceExists != nil {
		exists, err := cli.initSelectionInstanceExists(ctx, execution.loaded.Context)
		if err != nil {
			return err
		}
		if exists != *selection.instanceExists {
			return fmt.Errorf("%w: yard existence changed during integration adoption", domain.ErrPlanStale)
		}
	}
	return nil
}

func (selection *initIntegrationSelection) apply(execution *initExecution) error {
	if selection == nil || execution.bootstrap != nil {
		return nil
	}
	return selection.write.Apply()
}

func readInitSelectionSnapshot(configHome, path string) (config.PersistentFileSnapshot, error) {
	snapshot, err := config.ReadPersistentFileSnapshot(configHome, path)
	if errors.Is(err, os.ErrNotExist) {
		return config.PersistentFileSnapshot{}, nil
	}
	return snapshot, err
}
