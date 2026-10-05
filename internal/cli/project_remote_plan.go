package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

// ProjectCopySourceDescriptor carries facts about a retained controller draft.
// Source is an opaque registry identity on the owner, never an owner file path.
type ProjectCopySourceDescriptor struct {
	Schema        int    `json:"schema"`
	Source        string `json:"source"`
	ArchiveDigest string `json:"archiveDigest"`
	ArchiveSize   int64  `json:"archiveSize"`
	TreeDigest    string `json:"treeDigest"`
	RequestedName string `json:"requestedName"`
	ExplicitName  bool   `json:"explicitName"`
	TargetProfile string `json:"targetProfile"`
}

type ownerProjectCopy struct {
	source      ProjectCopySourceDescriptor
	reservation *state.ProjectReservation
	begun       bool
	finalized   bool
}

func (source ProjectCopySourceDescriptor) validate() error {
	validDigest := func(value string) bool {
		decoded, err := hex.DecodeString(value)
		return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
	}
	if source.Schema != 1 || !filepath.IsAbs(source.Source) || filepath.Clean(source.Source) != source.Source || len(source.Source) > 4096 || !utf8.ValidString(source.Source) || strings.ContainsFunc(source.Source, unicode.IsControl) || !domain.SafeProjectName(source.RequestedName) || !validDigest(source.ArchiveDigest) || !validDigest(source.TreeDigest) || source.ArchiveSize <= 0 || !domain.SafeName(source.TargetProfile) {
		return errors.New("invalid retained project source descriptor")
	}
	return nil
}

func (execution *projectExecution) copySourceDescriptor(ctx context.Context) (ProjectCopySourceDescriptor, error) {
	if execution == nil || execution.preparedArchive == nil || execution.Record.Mode != domain.ProjectSync {
		return ProjectCopySourceDescriptor{}, errors.New("sync requires retained controller input")
	}
	archive, err := execution.preparedArchive.Open(ctx, execution.Record.HostPath)
	if err != nil {
		return ProjectCopySourceDescriptor{}, err
	}
	tree, err := application.ProjectArchiveContentDigest(archive, false)
	if err := errors.Join(err, archive.Close()); err != nil {
		return ProjectCopySourceDescriptor{}, err
	}
	target := execution.Record.Target
	if target == "" {
		target = "yard"
	}
	source := ProjectCopySourceDescriptor{Schema: 1, Source: execution.Record.HostPath, ArchiveDigest: execution.sourceDigest, ArchiveSize: execution.sourceSize, TreeDigest: tree, RequestedName: execution.RequestedName, ExplicitName: execution.ExplicitName, TargetProfile: target}
	return source, source.validate()
}

func (cli *CLI) prepareOwnerProjectCopy(ctx context.Context, loaded config.Loaded, source ProjectCopySourceDescriptor) (*preparedCommand, error) {
	if err := source.validate(); err != nil {
		return nil, err
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		return nil, errors.New("project copy plan requires the local owner")
	}
	if err := requireProjectRole(loaded); err != nil {
		return nil, err
	}
	if err := validateProjectTarget(cli.options.RepositoryRoot, source.TargetProfile); err != nil {
		return nil, err
	}
	definition, ok := cli.manifest.Lookup("sync")
	if !ok {
		return nil, errors.New("sync command is unavailable")
	}
	store, err := openProjectPreparationStore(ctx, loaded.Context)
	if err != nil {
		return nil, err
	}
	project := &projectExecution{
		Loaded: loaded, Store: store, OperationID: cli.ensureOperationID(), RequestedName: source.RequestedName, ExplicitName: source.ExplicitName,
		sourceDigest: source.ArchiveDigest, sourceSize: source.ArchiveSize, RequiresProjects: true,
		ownerCopy: &ownerProjectCopy{source: source},
		Record:    domain.ProjectRecord{Schema: 1, IdentityVersion: 2, ProjectID: source.RequestedName, Name: source.RequestedName, HostPath: source.Source, SourceKey: state.SourceKey(source.Source), YardPath: state.YardPath(source.RequestedName), Mode: domain.ProjectSync, SSHHost: loaded.Context.SSHHost, Target: source.TargetProfile, ImportedAt: time.Now().UTC().Format(time.RFC3339)},
	}
	if source.TargetProfile != "yard" {
		project.Record.Profile = source.TargetProfile
	}
	prepared := &preparedCommand{CLI: cli, Definition: definition, Loaded: loaded, Project: project, exactState: operationStateDigest(source)}
	prepared.stepsComplete = true
	prepared.policy = commandPolicy(definition, loaded.Context, nil, project)
	prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := cli.observeProjectCopy(ctx, project); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return project.actionPlan("sync")
	}
	prepared.refresh = prepared.assess
	prepared.steps = func() []domain.OperationStep { return project.operationSteps("sync") }
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		return cli.beginOwnerProjectCopy(ctx, prepared)
	}
	prepared.closeResource = func() error { return cli.abortOwnerProjectCopy(context.Background(), prepared) }
	if err := prepared.preparePlan(ctx); err != nil {
		_ = prepared.Close()
		return nil, err
	}
	return prepared, nil
}

func (cli *CLI) beginOwnerProjectCopy(ctx context.Context, prepared *preparedCommand) (domain.AdapterResult, error) {
	project := prepared.Project
	if project == nil || project.ownerCopy == nil || project.ownerCopy.begun || !prepared.Plan.Confirmed {
		return domain.AdapterResult{}, errors.New("project copy is not authorized for admission")
	}
	copy := project.ownerCopy
	fresh, err := cli.loadInventoryLoaded(project.Loaded.Context.YardName, project.Loaded)
	if err != nil {
		return domain.AdapterResult{}, err
	}
	if operationStateDigest(fresh.Context) != operationStateDigest(project.Loaded.Context) {
		return domain.AdapterResult{}, fmt.Errorf("%w: owner yard context changed", domain.ErrPlanStale)
	}
	if err := requireProjectRole(fresh); err != nil {
		return domain.AdapterResult{}, err
	}
	admission, err := project.Store.Admit(ctx, project.OperationID, copy.source.Source, domain.ProjectSync, copy.source.RequestedName, copy.source.ExplicitName, project.WorkspaceNames...)
	if err != nil {
		return domain.AdapterResult{}, err
	}
	if admission.Existing != nil || admission.Reservation == nil {
		return domain.AdapterResult{}, errors.New("sync requires one new independent owner admission")
	}
	copy.reservation = admission.Reservation
	if copy.source.ExplicitName && admission.ProjectID != project.Record.ProjectID {
		_ = cli.abortOwnerProjectCopy(ctx, prepared)
		return domain.AdapterResult{}, fmt.Errorf("%w: explicit project copy name changed", domain.ErrPlanStale)
	}
	project.setCopyIdentity(admission.ProjectID, admission.Name)
	copy.begun = true
	// The retained native reservation belongs to ownerCopy, not the ordinary
	// command commit/abort path: streaming has not happened yet.
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok", Output: map[string]any{"project": project.Record}}, nil
}

func (cli *CLI) finalizeOwnerProjectCopy(ctx context.Context, prepared *preparedCommand) (domain.AdapterResult, error) {
	project := prepared.Project
	if project == nil || project.ownerCopy == nil || !project.ownerCopy.begun || project.ownerCopy.finalized || project.ownerCopy.reservation == nil {
		return domain.AdapterResult{}, errors.New("project copy has no active owner admission")
	}
	copy := project.ownerCopy
	live, err := application.ObserveProjectContentDigest(ctx, cli.projectDataPlane(), project.Loaded.Context, project.Record.YardPath, false)
	if err != nil {
		return domain.AdapterResult{}, err
	}
	if live != copy.source.TreeDigest {
		return domain.AdapterResult{}, fmt.Errorf("%w: copied project content differs from approved source", domain.ErrPlanStale)
	}
	expected, err := application.ProjectMetadata(project.Record, project.Loaded.Context.YardName)
	if err != nil {
		return domain.AdapterResult{}, err
	}
	result, err := cli.projectDataPlane().Execute(ctx, project.Loaded.Context, ports.InstanceExecRequest{Command: []string{"cat", filepath.Join(filepath.Dir(project.Record.YardPath), ".subyard-meta.json")}})
	if err != nil || result.ExitCode != 0 || !bytes.Equal(expected, result.Stdout) {
		return domain.AdapterResult{}, errors.New("copied project metadata does not match approved owner admission")
	}
	if err := project.Store.FinalizeOperation(ctx, project.OperationID, project.Record); err != nil {
		return domain.AdapterResult{}, err
	}
	copy.finalized, copy.reservation = true, nil
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok", Output: map[string]any{"project": project.Record}}, nil
}

func (cli *CLI) abortOwnerProjectCopy(ctx context.Context, prepared *preparedCommand) error {
	if prepared == nil || prepared.Project == nil || prepared.Project.ownerCopy == nil {
		return nil
	}
	copy := prepared.Project.ownerCopy
	if copy.reservation == nil {
		return nil
	}
	err := prepared.Project.Store.AbortAdmission(ctx, copy.reservation.OperationID)
	if err == nil {
		copy.reservation = nil
	}
	return err
}

func (prepared *preparedCommand) prepareRemoteProjectCopy(ctx context.Context) error {
	cli, project := prepared.CLI, prepared.Project
	if project == nil || prepared.Definition.Name != "sync" || project.Loaded.Context.AccessKind != domain.AccessRemote {
		return errors.New("remote project copy requires controller sync")
	}
	if err := project.prepareSource(ctx, cli); err != nil {
		return err
	}
	source, err := project.copySourceDescriptor(ctx)
	if err != nil {
		return err
	}
	session, err := cli.openOwnerRPC(ctx, project.Loaded.Context)
	if err != nil {
		return err
	}
	previousClose := prepared.closeResource
	prepared.closeResource = func() error {
		var prior error
		if previousClose != nil {
			prior = previousClose()
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
	var exact exactOperationPlan
	if err := session.call(ctx, "operation.plan", cli.ensureOperationID(), struct {
		Command    string                       `json:"command"`
		Exact      bool                         `json:"exact"`
		Source     *ProjectCopySourceDescriptor `json:"source"`
		StepSchema int                          `json:"stepSchema"`
	}{"sync", true, &source, 1}, &exact); err != nil {
		return err
	}
	digest, err := hex.DecodeString(exact.Digest)
	if err != nil || len(digest) != 32 || exact.Schema != 1 || exact.StepSchema != 1 || len(exact.Plan.Steps) == 0 || exact.Plan.OperationID != cli.ensureOperationID() || exact.Plan.Command != "sync" || exact.Plan.Target != domain.TargetLocalOwner || exact.Plan.Effect != domain.CommandMutate || exact.ExpiresAt.IsZero() || exact.Plan.Confirmed && exact.Plan.Confirmation != domain.ConfirmationNever {
		return errors.New("owner returned invalid project copy plan")
	}
	if err := domain.ValidateOperationSteps(exact.Plan.Steps); err != nil {
		return err
	}
	prepared.Plan, prepared.ownerPlan, prepared.executeNoOp = exact.Plan, true, true
	prepared.assess, prepared.refresh, prepared.steps = nil, nil, nil
	project.remoteCopy = true
	prepared.preview = func() {
		for _, consequence := range prepared.Plan.Consequences {
			fmt.Fprintln(cli.options.Stdout, "  "+consequence)
		}
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if err := project.checkPreparedSource(ctx, cli); err != nil {
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
			return domain.AdapterResult{}, errors.New("owner project admission response mismatch")
		}
		encoded, err := json.Marshal(response.Result.Output["project"])
		if err != nil {
			return domain.AdapterResult{}, err
		}
		var admitted domain.ProjectRecord
		if err := json.Unmarshal(encoded, &admitted); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := admitted.Validate(admitted.ProjectID); err != nil {
			return domain.AdapterResult{}, err
		}
		if admitted.Mode != domain.ProjectSync || admitted.HostPath != source.Source || admitted.SourceKey != state.SourceKey(source.Source) || admitted.Target != source.TargetProfile || source.ExplicitName && admitted.ProjectID != source.RequestedName {
			return domain.AdapterResult{}, errors.New("owner admitted a different project copy scope")
		}
		project.Record = admitted
		yard := project.Loaded.Context
		yard.YardName = yard.OwnerYardName
		if yard.YardName == "" {
			yard.YardName = "default"
		}
		orchestrator.Runner = application.ProjectActionRunner{Data: cli.projectDataPlane(), Archive: cli.projectArchiver(), PreparedArchive: project.preparedArchive, Project: project.Record, Yard: yard}
		result, stderr, err := orchestrator.RunAdapter(ctx, projectTransferPlan(prepared.Plan), domain.AdapterRequest{Schema: shelladapter.ProtocolSchema, OperationID: exact.Plan.OperationID, Adapter: "project", Action: "sync"}, nil)
		writeAdapterDiagnostics(diagnostics, stderr)
		if err != nil || result.Status != "ok" {
			return result, err
		}
		if err := session.call(ctx, "project.copy.finalize", exact.Plan.OperationID, struct {
			Digest string `json:"digest"`
		}{exact.Digest}, &response); err != nil {
			return result, err
		}
		if response.Result.OperationID != exact.Plan.OperationID || response.Result.Status != "ok" {
			return result, errors.New("owner project finalization response mismatch")
		}
		if err := cli.invalidateOwnerInventory(project.Loaded); err != nil {
			return result, err
		}
		return result, nil
	}
	return nil
}

// The owner authorized the action assessment. The controller consumes the same
// confirmed operation and bounded steps solely to transfer its retained input.
func projectTransferPlan(owner domain.OperationPlan) domain.OperationPlan {
	owner.Assessment = nil
	owner.ConfirmationRequest = nil
	return owner
}
