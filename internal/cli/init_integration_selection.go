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
)

// This is a prepared write to the existing yard config, not another desired store.
type initIntegrationSelection struct {
	write    *config.YardIntegrationWrite
	path     string
	before   config.PersistentFileSnapshot
	original config.IntegrationSelection
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
	if selection.Provenance.Scope == "command" {
		return config.Loaded{}, nil, errors.New("cannot adopt temporary CODING_TOOL_INTEGRATIONS or AGENTS command overrides; configure the selected yard's profile or persistent settings")
	}
	if _, registered, err := configsync.ReadSourceRecord(loaded.Context.Paths.ConfigHome); err != nil {
		return config.Loaded{}, nil, err
	} else if registered {
		if bootstrap != nil {
			return config.Loaded{}, nil, errors.New("yard definition is source-managed; define the selected yard in its source and sync before init")
		}
		// The loader already resolved the profile and source settings. Init consumes
		// that intent without writing a local override of the registered source.
		return loaded, nil, nil
	}
	if explicitCanonical {
		return loaded, nil, nil
	}
	if selection.Provenance.Scope != "yard" || selection.Provenance.Role != "scalar settings" {
		// Inherited profile settings remain inherited, including on a fresh named
		// yard. Only an explicit local legacy assignment needs canonicalization.
		return loaded, nil, nil
	}
	desired := selection.Requested
	if !selection.AllowsCodingTools {
		desired = []string{}
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
		return proposed, &initIntegrationSelection{path: path, before: snapshot, original: selection}, nil
	}
	write, err := config.PlanYardIntegrationWrite(loaded, desired)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	return proposed, &initIntegrationSelection{write: write, path: path, before: snapshot, original: selection}, nil
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
