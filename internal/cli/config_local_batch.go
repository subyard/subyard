package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func (prepared *preparedCommand) prepareLocalConfigBatch(ctx context.Context, requests []configAuthoringRequest) error {
	cli, loaded := prepared.CLI, prepared.Loaded
	scope := requests[0].scope
	home := loaded.Context.Paths.ConfigHome
	path, baseline, targetErr := checkLocalBatchTarget(loaded, scope)
	assignments, assignmentErrs := checkLocalBatchAssignments(loaded.Catalog, requests)
	check := func(locked bool) ([]error, error) {
		return cli.checkLocalBatchConstraints(loaded, scope, path, baseline, assignments, locked)
	}
	contextual, constraintsErr := check(false)
	var ingress []error
	for index, assignment := range assignments {
		if assignment.Valid && contextual[index] == nil {
			request := requests[index]
			request.name = assignment.Name
			ingress = append(ingress, cli.checkResourceConfigChange(ctx, loaded, request))
		}
	}
	for index := range assignmentErrs {
		assignmentErrs[index] = domain.CollectRefusals(assignmentErrs[index], contextual[index])
	}
	if err := domain.CollectOrderedRefusals(targetErr, domain.CollectOrderedRefusals(assignmentErrs...),
		constraintsErr, domain.CollectRefusals(ingress...)); err != nil {
		return configAuthoringFailure{home, err}
	}

	canonical := make([]configAuthoringRequest, len(requests))
	edits := make([]config.PersistentAssignment, len(requests))
	for index, assignment := range assignments {
		edits[index] = assignment.PersistentAssignment
		canonical[index] = requests[index]
		canonical[index].name = assignment.Name
	}
	content, err := loaded.Catalog.EditPersistentAssignmentsContent(path, baseline.Content, edits)
	if err != nil {
		return configAuthoringFailure{home, err}
	}
	desired := config.PersistentFileSnapshot{Exists: true, Content: content}
	prospective, err := cli.prepareConfigProspective(loaded, canonical[0], path, desired)
	if err != nil {
		return err
	}
	prepared.closeResource = func() error { prospective.close(); return nil }
	prospective.checkBatch = func(locked bool) error {
		refusals, constraints := check(locked)
		return domain.CollectOrderedRefusals(domain.CollectOrderedRefusals(refusals...), constraints)
	}
	return prepared.attachLocalConfigScalars(ctx, canonical, prospective, *baseline, desired, func(guard func() error) error {
		return config.WriteLocalScalarFileIfUnchanged(home, configScalarAuthoringRoot(loaded, path), path, *baseline, desired.Content, guard)
	})
}

func checkLocalBatchTarget(loaded config.Loaded, scope config.SettingScope) (string, *config.PersistentFileSnapshot, error) {
	home := loaded.Context.Paths.ConfigHome
	storageErr := config.CheckLocalBatchStorage(home)
	path, err := configScalarAuthoringPath(loaded, scope)
	if err != nil {
		return "", nil, domain.CollectOrderedRefusals(storageErr, configUsage(err))
	}
	snapshot, err := readConfigPersistentSnapshot(configScalarAuthoringRoot(loaded, path), path)
	if err != nil {
		return path, nil, domain.CollectOrderedRefusals(storageErr, err)
	}
	return path, &snapshot, storageErr
}

func checkLocalBatchAssignments(catalog config.Catalog, requests []configAuthoringRequest) ([]config.LocalBatchAssignment, []error) {
	seen := make(map[string]int, len(requests))
	assignments := make([]config.LocalBatchAssignment, len(requests))
	refusals := make([]error, len(requests))
	for index, request := range requests {
		name := config.CanonicalSettingName(request.name)
		first, repeated := seen[name]
		if !repeated {
			seen[name] = index
		}
		var checks []error
		var repeatedIDs error
		if repeated {
			message := "multiple arguments assign the same setting"
			_, firstSafe := domain.ConfigSettingName(requests[first].name)
			_, currentSafe := domain.ConfigSettingName(request.name)
			if firstSafe && currentSafe {
				message = requests[first].name + " and " + request.name + " assign the same setting"
			}
			checks = append(checks, configPublicRefusal(message))
			assignments[first].Valid = false
		}
		assignment := &assignments[index]
		assignment.PersistentAssignment = config.PersistentAssignment{Name: name, Value: request.value}
		definition, err := configAuthoringDefinition(catalog, request.action, request.scope, request.name, false)
		if err != nil {
			checks = append(checks, configNamedRefusal{request.name, err})
		} else {
			syntax, writable := configScalarSyntaxRefusal(catalog, definition, request)
			assignment.Writable, assignment.Valid = writable, writable && syntax == nil && !repeated
			checks = append(checks, syntax)
			if writable && name == "CODING_TOOL_INTEGRATIONS" {
				ids := strings.Fields(assignment.Value)
				retired := catalog.CheckSelectableIntegrations(ids)
				repeatedIDs = catalog.CheckRepeatedIntegrations(ids)
				assignment.Valid = assignment.Valid && retired == nil && repeatedIDs == nil
				checks = append(checks, retired)
			}
		}
		if err := domain.CollectRefusals(checks...); err != nil {
			refusals[index] = configUsage(err)
		}
		refusals[index] = domain.CollectRefusals(refusals[index], repeatedIDs)
	}
	return assignments, refusals
}

func (cli *CLI) checkLocalBatchConstraints(loaded config.Loaded, scope config.SettingScope, path string, baseline *config.PersistentFileSnapshot, assignments []config.LocalBatchAssignment, locked bool) ([]error, error) {
	paths := loaded.Context.Paths
	refusals := make([]error, len(assignments))
	names := []string{loaded.Context.YardName}
	var constraints []error
	if scope != config.ScopeYard {
		var err error
		names, err = config.LocalBatchYardNames(paths.ConfigDir, paths.ConfigHome)
		constraints = append(constraints, err)
	}
	environment := cli.configProspectiveEnvironment(loaded)
	for _, name := range names {
		perAssignment, err := loaded.Catalog.CheckLocalBatchConstraints(config.LoadOptions{
			Resources: cli.operationResourceRegistry, RepositoryRoot: cli.options.RepositoryRoot,
			OperatorHome: paths.OperatorHome, YardName: name, Environment: environment, ConfigLocked: locked,
		}, scope, path, baseline, assignments)
		for index, refusal := range perAssignment {
			refusals[index] = domain.CollectRefusals(refusals[index], refusal)
		}
		if err != nil {
			constraints = append(constraints, fmt.Errorf("yard %s: %w", name, err))
		}
	}
	return refusals, domain.CollectOrderedRefusals(constraints...)
}
