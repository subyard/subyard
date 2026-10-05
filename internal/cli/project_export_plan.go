package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/adapters/projectruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

// Controller paths are opaque labels on the owner, never owner filesystem inputs.
type ProjectExportDescriptor struct {
	Schema             int    `json:"schema"`
	ProjectID          string `json:"projectId"`
	SourceKey          string `json:"sourceKey"`
	ArchiveDigest      string `json:"archiveDigest"`
	ArchiveSize        int64  `json:"archiveSize"`
	SourceTree         string `json:"sourceTree"`
	Destination        string `json:"destination"`
	DestinationBinding string `json:"destinationBinding"`
}

type ownerProjectExport struct {
	descriptor        ProjectExportDescriptor
	temporaryIdentity string
	finalized         bool
}

type controllerProjectExport struct {
	store *projectruntime.PreparedPatchStore
}

func hydrateExportProject(project *projectExecution, owner domain.ProjectRecord) error {
	selected := project.Record
	if err := owner.Validate(owner.ProjectID); err != nil {
		return err
	}
	sourceKey := selected.SourceKey
	if sourceKey == "" && selected.HostPath != "" {
		sourceKey = state.SourceKey(selected.HostPath)
	}
	if owner.ProjectID != selected.ProjectID || owner.Name != selected.Name || owner.Mode != domain.ProjectSync || selected.Mode != domain.ProjectSync || owner.YardPath != state.YardPath(owner.ProjectID) || selected.YardPath != owner.YardPath || owner.Target != selected.Target || sourceKey != state.SourceKey(owner.HostPath) || selected.HostPath != "" && selected.HostPath != owner.HostPath {
		return fmt.Errorf("%w: authoritative export project differs from selected owner scope", domain.ErrPlanStale)
	}
	project.Record = owner
	before, approved := owner, owner
	project.recordBefore, project.recordApproved = &before, &approved
	return nil
}

func validExportDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func (descriptor ProjectExportDescriptor) validate() error {
	if descriptor.Schema != 1 || !domain.SafeID(descriptor.ProjectID) || !validExportDigest(descriptor.SourceKey) || !validExportDigest(descriptor.ArchiveDigest) || !validExportDigest(descriptor.SourceTree) || !validExportDigest(descriptor.DestinationBinding) || descriptor.ArchiveSize <= 0 || !filepath.IsAbs(descriptor.Destination) || filepath.Clean(descriptor.Destination) != descriptor.Destination || len(descriptor.Destination) > 4096 || !utf8.ValidString(descriptor.Destination) || strings.ContainsFunc(descriptor.Destination, unicode.IsControl) {
		return errors.New("invalid retained project export descriptor")
	}
	return nil
}

func (execution *projectExecution) prepareProjectExportDestination(ctx context.Context, cli *CLI) error {
	if execution.controllerExport != nil {
		return execution.controllerExport.store.Check()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store := cli.projectExportStore(execution.Loaded)
	var prepared *projectruntime.PreparedPatchStore
	var err error
	switch store := store.(type) {
	case projectruntime.PatchStore:
		prepared, err = store.Prepare(execution.Record.ProjectID)
	case *projectruntime.PatchStore:
		prepared, err = store.Prepare(execution.Record.ProjectID)
	case *projectruntime.PreparedPatchStore:
		prepared, err = store, store.Check()
	default:
		return errors.New("exact export requires the native retained patch publisher")
	}
	if err != nil {
		return err
	}
	execution.controllerExport = &controllerProjectExport{store: prepared}
	return nil
}

func executionProjectExportStore(project *projectExecution, fallback ports.ProjectExportStore) ports.ProjectExportStore {
	if project != nil && project.controllerExport != nil {
		return project.controllerExport.store
	}
	return fallback
}

func (execution *projectExecution) exportDestinationBinding() string {
	if execution.ownerExport != nil {
		return operationStateDigest(execution.ownerExport.descriptor)
	}
	if execution.controllerExport != nil {
		return execution.controllerExport.store.Binding()
	}
	return ""
}

func (execution *projectExecution) exportOperationSteps() []domain.OperationStep {
	decision := domain.StepApply
	dependent := domain.StepConditional
	if !execution.ActionChanged {
		decision, dependent = domain.StepSkip, domain.StepSkip
	}
	ownerTarget := execution.Loaded.Context.IncusProject + "/" + execution.Loaded.Context.YardInstanceName + ":" + execution.Record.YardPath
	destination := "controller native export destination"
	if execution.controllerExport != nil {
		destination = "controller:" + execution.controllerExport.store.Path()
	}
	if execution.ownerExport != nil {
		destination = "controller:" + execution.ownerExport.descriptor.Destination
	}
	return []domain.OperationStep{{ID: "project.export.diff", Target: ownerTarget, Observed: "yard tree " + execution.exportObservedTree, Desired: "patch from source tree " + execution.exportSourceTree + " to yard tree " + execution.exportObservedTree, Decision: decision,
		Preconditions: []string{"unchanged owner context and project record", "retained controller archive and native guest source", "exclusive operation-owned temporary snapshot"}, Verify: "portable diff uses the approved archive and unchanged guest tree", Consequence: "compare the retained controller copy with the exact owner project"},
		{ID: "project.export.owner.verify", Target: ownerTarget, Observed: "export data plane not verified", Desired: "owner verifies approved project, baseline and source trees", Decision: dependent, DependsOn: []string{"project.export.diff"}, Preconditions: []string{"same retained owner session and operation digest"}, Verify: "owner native readback equals captured source facts", Consequence: "verify the exact project difference before publishing"},
		{ID: "project.export.publish", Target: destination, Observed: "patch absent", Desired: "exact retained portable patch with file mode 0600 and directory mode 0700", Decision: dependent, DependsOn: []string{"project.export.owner.verify"}, Preconditions: []string{"controller destination identity remains unchanged", "owner finalization succeeded"}, Verify: "published bytes, inode, ownership and protected modes match", Consequence: "publish the verified patch at the exact controller destination"}}
}

func (cli *CLI) prepareOwnerProjectExport(ctx context.Context, loaded config.Loaded, descriptor ProjectExportDescriptor) (*preparedCommand, error) {
	if err := descriptor.validate(); err != nil {
		return nil, err
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		return nil, errors.New("export plan requires the local owner")
	}
	definition, ok := cli.manifest.Lookup("export")
	if !ok {
		return nil, errors.New("export command is unavailable")
	}
	readOnly, err := openProjectStoreReadOnly(loaded.Context.Paths.StateDir)
	if err != nil {
		return nil, err
	}
	record, err := readOnly.store.GetReadOnly(ctx, descriptor.ProjectID)
	if err != nil {
		return nil, err
	}
	if record.Mode != domain.ProjectSync || state.SourceKey(record.HostPath) != descriptor.SourceKey {
		return nil, errors.New("export descriptor does not match the authoritative synced project")
	}
	before, approved := record, record
	project := &projectExecution{Loaded: loaded, Store: readOnly.store, Record: record, recordBefore: &before, recordApproved: &approved, OperationID: cli.ensureOperationID(),
		ownerExport: &ownerProjectExport{descriptor: descriptor}, sourceDigest: descriptor.ArchiveDigest, sourceSize: descriptor.ArchiveSize, exportSourceTree: descriptor.SourceTree}
	project.inputBaseline, err = cli.captureOwnerInputs(loaded, "", nil)
	if err != nil {
		return nil, err
	}
	project.exportObservedTree, err = application.ObserveProjectTree(ctx, cli.projectDataPlane(), loaded.Context, record.YardPath)
	if err != nil {
		return nil, err
	}
	project.ActionChanged = project.exportObservedTree != project.exportSourceTree
	prepared := &preparedCommand{CLI: cli, Loaded: loaded, Definition: definition, Project: project, stepsComplete: true}
	prepared.exactState = project.sourceBinding()
	prepared.policy = commandPolicy(definition, loaded.Context, nil, project)
	prepared.steps = project.exportOperationSteps
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return project.actionPlan("export")
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := cli.checkOwnerProjectExport(ctx, project); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return project.actionPlan("export")
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		identity, err := application.CreateProjectExportTemporary(ctx, cli.projectDataPlane(), loaded.Context, prepared.Plan.OperationID)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		project.ownerExport.temporaryIdentity = identity
		return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok", Output: map[string]any{"project": record, "temporaryIdentity": identity, "observedTree": project.exportObservedTree}}, nil
	}
	prepared.closeResource = func() error {
		if project.ownerExport.temporaryIdentity == "" {
			return nil
		}
		return application.CleanupProjectExportTemporary(cli.projectDataPlane(), loaded.Context, prepared.Plan.OperationID, project.ownerExport.temporaryIdentity)
	}
	if err := prepared.preparePlan(ctx); err != nil {
		_ = prepared.Close()
		return nil, err
	}
	return prepared, nil
}

func (cli *CLI) checkOwnerProjectExport(ctx context.Context, project *projectExecution) error {
	if err := project.checkProjectRecord(ctx, cli); err != nil {
		return err
	}
	if err := project.inputBaseline.check(ctx, cli); err != nil {
		return err
	}
	fresh, err := cli.loadInventoryLoaded(project.Loaded.Context.YardName, project.Loaded)
	if err != nil {
		return err
	}
	if operationStateDigest(fresh.Context) != operationStateDigest(project.Loaded.Context) {
		return fmt.Errorf("%w: owner export context changed", domain.ErrPlanStale)
	}
	live, err := application.ObserveProjectTree(ctx, cli.projectDataPlane(), project.Loaded.Context, project.Record.YardPath)
	if err != nil {
		return err
	}
	if live != project.exportObservedTree && live != project.exportSourceTree {
		return fmt.Errorf("%w: owner export source changed", domain.ErrPlanStale)
	}
	project.ActionChanged = live != project.exportSourceTree
	return nil
}

func (cli *CLI) verifyLocalProjectExport(ctx context.Context, project *projectExecution) error {
	if err := project.checkProjectRecord(ctx, cli); err != nil {
		return err
	}
	if err := project.inputBaseline.check(ctx, cli); err != nil {
		return err
	}
	fresh, err := cli.loadInventoryLoaded(project.Loaded.Context.YardName, project.Loaded)
	if err != nil {
		return err
	}
	if operationStateDigest(fresh.Context) != operationStateDigest(project.Loaded.Context) {
		return fmt.Errorf("%w: local export context changed", domain.ErrPlanStale)
	}
	live, err := application.ObserveProjectTree(ctx, cli.projectDataPlane(), project.Loaded.Context, project.Record.YardPath)
	if err != nil {
		return err
	}
	if live != project.exportObservedTree {
		return fmt.Errorf("%w: local export source changed during diff", domain.ErrPlanStale)
	}
	return project.checkPreparedSource(ctx, cli)
}

func (cli *CLI) finalizeOwnerProjectTransfer(ctx context.Context, prepared *preparedCommand) (domain.AdapterResult, error) {
	if prepared.Project == nil || prepared.Project.ownerExport == nil {
		return cli.finalizeOwnerProjectCopy(ctx, prepared)
	}
	project, transfer := prepared.Project, prepared.Project.ownerExport
	if transfer.finalized {
		return domain.AdapterResult{}, errors.New("project export finalization already consumed")
	}
	if err := cli.checkOwnerProjectExport(ctx, project); err != nil {
		return domain.AdapterResult{}, err
	}
	if transfer.temporaryIdentity != "" {
		if err := application.CheckProjectExportTemporary(ctx, cli.projectDataPlane(), project.Loaded.Context, prepared.Plan.OperationID, transfer.temporaryIdentity); err != nil {
			return domain.AdapterResult{}, err
		}
		baseline, err := application.ObserveProjectTree(ctx, cli.projectDataPlane(), project.Loaded.Context, filepath.Join(application.ProjectExportTemporary(prepared.Plan.OperationID), "a"))
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if baseline != transfer.descriptor.SourceTree {
			return domain.AdapterResult{}, fmt.Errorf("%w: export snapshot differs from approved controller source", domain.ErrPlanStale)
		}
		// A changed source becoming converged before diff is allowed; after a
		// retained diff, it must still be the original approved source tree.
		if !project.ActionChanged {
			return domain.AdapterResult{}, fmt.Errorf("%w: export source changed during transfer", domain.ErrPlanStale)
		}
	} else if project.ActionChanged {
		return domain.AdapterResult{}, fmt.Errorf("%w: export no-op acquired new work", domain.ErrPlanStale)
	}
	if transfer.temporaryIdentity != "" {
		if err := application.CleanupProjectExportTemporary(cli.projectDataPlane(), project.Loaded.Context, prepared.Plan.OperationID, transfer.temporaryIdentity); err != nil {
			return domain.AdapterResult{}, err
		}
		transfer.temporaryIdentity = ""
	}
	transfer.finalized = true
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
}

func (prepared *preparedCommand) prepareRemoteProjectExport(ctx context.Context) error {
	cli, project := prepared.CLI, prepared.Project
	if project == nil || project.Loaded.Context.AccessKind != domain.AccessRemote || prepared.Definition.Name != "export" {
		return errors.New("remote export requires a controller project")
	}
	session, err := cli.openOwnerRPC(ctx, project.Loaded.Context)
	if err != nil {
		return err
	}
	priorClose := prepared.closeResource
	prepared.closeResource = func() error {
		var prior error
		if priorClose != nil {
			prior = priorClose()
		}
		err := session.close()
		_, _ = cli.options.Stderr.Write(session.diagnostics())
		return errors.Join(prior, err)
	}
	if err := session.negotiate(ctx); err != nil {
		return err
	}
	if err := session.requireOperationSteps(); err != nil {
		return err
	}
	var ownerProjects rpcProjectList
	if err := session.call(ctx, "project.list", cli.ensureOperationID(), struct {
		Live bool `json:"live"`
	}{false}, &ownerProjects); err != nil {
		return err
	}
	var ownerRecord *domain.ProjectRecord
	for index := range ownerProjects.Projects {
		if ownerProjects.Projects[index].ProjectID == project.Record.ProjectID {
			if ownerRecord != nil {
				return errors.New("owner export project identity is ambiguous")
			}
			ownerRecord = &ownerProjects.Projects[index]
		}
	}
	if ownerRecord == nil {
		return fmt.Errorf("%w: owner export project no longer exists", domain.ErrPlanStale)
	}
	if err := hydrateExportProject(project, *ownerRecord); err != nil {
		return err
	}
	if err := project.prepareSource(ctx, cli); err != nil {
		return err
	}
	if err := project.prepareProjectExportDestination(ctx, cli); err != nil {
		return err
	}
	archive, err := project.preparedArchive.Open(ctx, project.Record.HostPath)
	if err != nil {
		return err
	}
	tree, err := application.ProjectArchiveTreeDigest(archive)
	if err := errors.Join(err, archive.Close()); err != nil {
		return err
	}
	descriptor := ProjectExportDescriptor{Schema: 1, ProjectID: project.Record.ProjectID, SourceKey: state.SourceKey(project.Record.HostPath), ArchiveDigest: project.sourceDigest, ArchiveSize: project.sourceSize, SourceTree: tree, Destination: project.controllerExport.store.Path(), DestinationBinding: project.controllerExport.store.Binding()}
	if err := descriptor.validate(); err != nil {
		return err
	}
	var exact exactOperationPlan
	if err := session.call(ctx, "operation.plan", cli.ensureOperationID(), struct {
		Command    string                  `json:"command"`
		Exact      bool                    `json:"exact"`
		StepSchema int                     `json:"stepSchema"`
		Export     ProjectExportDescriptor `json:"export"`
	}{"export", true, 1, descriptor}, &exact); err != nil {
		return err
	}
	if !validExportDigest(exact.Digest) || exact.Schema != 1 || exact.StepSchema != 1 || len(exact.Plan.Steps) != 3 || exact.Plan.OperationID != cli.ensureOperationID() || exact.Plan.Command != "export" || exact.Plan.Target != domain.TargetLocalOwner || exact.Plan.Effect != domain.CommandMutate || exact.Plan.Confirmation != domain.ConfirmationNever || exact.ExpiresAt.IsZero() {
		return errors.New("owner returned an invalid exact export plan")
	}
	if err := domain.ValidateOperationSteps(exact.Plan.Steps); err != nil {
		return err
	}
	if exact.Plan.Steps[2].Target != "controller:"+descriptor.Destination {
		return errors.New("owner export destination scope mismatch")
	}
	prepared.Plan, prepared.ownerPlan, prepared.executeNoOp = exact.Plan, true, true
	prepared.assess, prepared.refresh, prepared.steps = nil, nil, nil
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, diagnostics io.Writer) (result domain.AdapterResult, runErr error) {
		if err := project.checkPreparedSource(ctx, cli); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := project.controllerExport.store.Check(); err != nil {
			return domain.AdapterResult{}, err
		}
		var response struct {
			Plan   domain.OperationPlan `json:"plan"`
			Result domain.AdapterResult `json:"result"`
		}
		if err := session.call(ctx, "operation.execute", exact.Plan.OperationID, struct {
			Confirmed bool   `json:"confirmed"`
			Digest    string `json:"digest"`
		}{true, exact.Digest}, &response); err != nil {
			return domain.AdapterResult{}, err
		}
		if response.Plan.OperationID != exact.Plan.OperationID || response.Result.OperationID != exact.Plan.OperationID || response.Result.Status != "ok" {
			return domain.AdapterResult{}, errors.New("owner export authorization response mismatch")
		}
		identity, _ := response.Result.Output["temporaryIdentity"].(string)
		var draft *application.ProjectExportDraft
		var runner application.ProjectActionRunner
		if identity != "" {
			encoded, err := json.Marshal(response.Result.Output["project"])
			if err != nil {
				return domain.AdapterResult{}, err
			}
			var ownerRecord domain.ProjectRecord
			if err := json.Unmarshal(encoded, &ownerRecord); err != nil {
				return domain.AdapterResult{}, err
			}
			observed, _ := response.Result.Output["observedTree"].(string)
			if ownerRecord != project.Record || !validExportDigest(observed) {
				return domain.AdapterResult{}, errors.New("owner authorized a different export project")
			}
			yard := project.Loaded.Context
			yard.YardName = yard.OwnerYardName
			if yard.YardName == "" {
				yard.YardName = "default"
			}
			runner = application.ProjectActionRunner{Data: cli.projectDataPlane(), Archive: cli.projectArchiver(), PreparedArchive: project.preparedArchive, Exports: project.controllerExport.store, Yard: yard, Project: ownerRecord, ExportObservedTree: observed, ExportSourceTree: descriptor.SourceTree, VerifyPrepared: true}
			draft, err = runner.PrepareExport(ctx, exact.Plan.OperationID, identity)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			defer func() { runErr = errors.Join(runErr, draft.Close()) }()
		}
		if err := session.call(ctx, "project.copy.finalize", exact.Plan.OperationID, struct {
			Digest string `json:"digest"`
		}{exact.Digest}, &response); err != nil {
			return domain.AdapterResult{}, err
		}
		if response.Result.OperationID != exact.Plan.OperationID || response.Result.Status != "ok" {
			return domain.AdapterResult{}, errors.New("owner export verification response mismatch")
		}
		result = domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: exact.Plan.OperationID, Status: "ok", Output: map[string]any{"projectId": project.Record.ProjectID, "yardPath": project.Record.YardPath}}
		if draft == nil || !draft.Changed {
			fmt.Fprintf(diagnostics, "no changes in the yard (%s)\n", project.Record.Name)
			return result, nil
		}
		path, err := runner.PublishExport(ctx, draft)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		result.Output["patch"] = path
		fmt.Fprintf(diagnostics, "exported %s\npatch: %s\n", project.Record.Name, path)
		return result, nil
	}
	return nil
}
