package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/state"
)

type projectCommit string

const (
	projectCommitNone   projectCommit = ""
	projectCommitPut    projectCommit = "put"
	projectCommitDelete projectCommit = "delete"
)

type projectExecution struct {
	OwnerConnection       *ownerinventory.Connection
	Loaded                config.Loaded
	YardIdentity          string
	Arguments             []string
	Environment           map[string]string
	Record                domain.ProjectRecord
	Store                 *state.FileStore
	Commit                projectCommit
	Profile               application.ProjectEnvironmentProfile
	SecretPath            string
	HostLinks             []string
	preparedArchive       *preparedProjectArchive
	sourceDigest          string
	sourceSize            int64
	cloneRevision         string
	cloneUnborn           bool
	sourceIdentity        string
	exportSourceTree      string
	exportObservedTree    string
	approvedRemoval       *projectRemovalObservation
	approvedHookPaths     []string
	ownerCopy             *ownerProjectCopy
	ownerExport           *ownerProjectExport
	controllerExport      *controllerProjectExport
	remoteCopy            bool
	bindDeviceConverged   bool
	bindMetadataConverged bool
	bindSourceBefore      os.FileInfo
	recordBefore          *domain.ProjectRecord
	recordApproved        *domain.ProjectRecord
	inputBaseline         *ownerInputBaseline
	secretIdentity        *ownerInputFile
	approvedEnvironment   string
	environmentObserved   string
	Reservation           *state.ProjectReservation
	OperationID           string
	ExplicitName          bool
	RequestedName         string
	RemoteReserved        bool
	PreviewExisting       *domain.ProjectRecord
	ActionChanged         bool
	WorkspaceNames        []string
	CopyObserved          bool
	Removal               projectRemovalObservation
	RequiresProjects      bool
	RequiresActiveTarget  bool
}

type projectRemovalObservation = application.ProjectRemovalObservation

func (execution *projectExecution) removeActionPlan() (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil || execution.Commit != projectCommitDelete {
		return "", domain.ActionDelta{}, errors.New("project removal execution is required")
	}
	return application.PlanProjectRemoval(execution.Record, execution.Environment["SUBYARD_PROJECT_REMOVE_SOFT"] == "1")
}

func (execution *projectExecution) actionPlan(commandName string) (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil {
		return "", domain.ActionDelta{}, errors.New("project execution is required")
	}
	return application.PlanProjectAction(commandName, execution.Record, execution.ActionChanged,
		execution.Environment["SUBYARD_PROJECT_REBUILD"] == "1", execution.automaticCopy(), execution.RequestedName)
}

func (cli *CLI) observeProjectAction(
	ctx context.Context,
	commandName string,
	execution *projectExecution,
) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, execution.closePreparedSource())
		}
	}()
	if execution == nil {
		return errors.New("project execution is required")
	}
	if err := execution.checkProjectRecord(ctx, cli); err != nil {
		return err
	}
	switch commandName {
	case "sync", "clone":
		return cli.observeProjectCopy(ctx, execution)
	case "bind":
		instances, _ := cli.statusPorts()
		runner := application.ProjectActionRunner{Yard: execution.Loaded.Context, Project: execution.Record,
			Instances: instances, Data: cli.projectDataPlane()}
		var err error
		execution.ActionChanged, execution.bindDeviceConverged, execution.bindMetadataConverged, err =
			runner.ObserveBind(ctx, execution.ActionChanged, execution.bindDeviceConverged, execution.bindMetadataConverged)
		return err
	case "export":
		if execution.Record.Mode != domain.ProjectSync {
			return fmt.Errorf("%s projects cannot be exported", execution.Record.Mode)
		}
		if !domain.SafeID(execution.OperationID) {
			return errors.New("export assessment requires a safe operation ID")
		}
		if err := execution.prepareSource(ctx, cli); err != nil {
			return err
		}
		runner := application.ProjectActionRunner{Yard: execution.Loaded.Context, Project: execution.Record,
			Data: cli.projectDataPlane(), PreparedArchive: execution.preparedArchive}
		changed, live, source, err := runner.ObserveExport(ctx, execution.exportObservedTree)
		if err != nil {
			return err
		}
		if execution.exportObservedTree == "" {
			execution.exportObservedTree = live
			execution.exportSourceTree = source
		}
		execution.ActionChanged = changed
		return nil
	case "up", "down":
		runner := application.ProjectActionRunner{Yard: execution.Loaded.Context, Project: execution.Record,
			Data: cli.projectDataPlane()}
		var err error
		execution.ActionChanged, execution.environmentObserved, execution.approvedEnvironment, err =
			runner.ObserveEnvironment(ctx, commandName, execution.Profile, execution.SecretPath != "",
				execution.Environment["SUBYARD_PROJECT_REBUILD"] == "1", execution.ActionChanged,
				execution.environmentObserved, execution.approvedEnvironment)
		return err
	default:
		return nil
	}
}

func (cli *CLI) prepareProjectRemoval(
	ctx context.Context,
	execution *projectExecution,
) error {
	if err := execution.checkProjectRecord(ctx, cli); err != nil {
		return err
	}
	action, _, err := execution.removeActionPlan()
	if err != nil {
		return err
	}
	instances, _ := cli.statusPorts()
	runner := application.ProjectActionRunner{Yard: execution.Loaded.Context, Project: execution.Record,
		Data: cli.projectDataPlane(), Instances: instances}
	return runner.ObserveRemoval(ctx, action, &execution.Removal, &execution.approvedRemoval)
}

func (cli *CLI) prepareProjectExecution(
	ctx context.Context,
	loaded config.Loaded,
	definition command.Definition,
	arguments []string,
	explicit bool,
	readOnly bool,
) (execution *projectExecution, err error) {
	defer func() {
		if err == nil && execution != nil {
			execution.RequiresProjects = projectCommandRequiresProjects(definition.Name)
			execution.RequiresActiveTarget = slices.Contains([]string{"sync", "bind", "clone", "up"}, definition.Name)
			err = cli.captureProjectOwner(ctx, execution)
			if err == nil && execution.Loaded.Context.AccessKind == domain.AccessRemote {
				err = cli.recheckProjectRole(ctx, execution)
			}
		}
	}()
	switch definition.Name {
	case "init", "provision":
		return cli.prepareProjectInventory(ctx, loaded, arguments)
	case "sync", "bind":
		return cli.prepareProjectImport(ctx, loaded, definition.Name, arguments)
	case "clone":
		return cli.prepareProjectClone(ctx, loaded, arguments)
	case "code", "export", "remove", "shell", "up", "down", "info":
		return cli.prepareExistingProject(ctx, loaded, definition.Name, arguments, explicit, readOnly)
	default:
		return nil, nil
	}
}

func projectCommandRequiresProjects(name string) bool {
	switch name {
	case "sync", "bind", "clone", "code", "shell", "up":
		return true
	default:
		return false
	}
}

func requireProjectRole(loaded config.Loaded) error {
	if loaded.Environment["ALLOWS_PROJECTS"] == "false" {
		return errors.New("selected yard role does not accept work projects")
	}
	return nil
}

// Capture the registration during preparation without creating locks or state.
// Execute checks it again under the host mutation lock after confirmation.
func (cli *CLI) captureProjectOwner(ctx context.Context, execution *projectExecution) error {
	yard := execution.Loaded.Context
	if yard.AccessKind != domain.AccessRemote {
		return nil
	}
	root := filepath.Join(yard.Paths.DataHome, "owner-inventory")
	connections, err := (ownerinventory.Connections{Root: root}).ListReadOnly()
	if err != nil {
		return err
	}
	for _, connection := range connections {
		if connection.Destination != yard.OwnerEndpoint {
			continue
		}
		if err := cli.checkProjectOwnerRoute(ctx, execution, connection); err != nil {
			return err
		}
		execution.OwnerConnection = &connection
		return nil
	}
	if strings.HasPrefix(filepath.Clean(yard.Paths.StateDir), filepath.Join(root, "routing")+string(filepath.Separator)) {
		return fmt.Errorf("%w: owner project route is no longer registered", domain.ErrPlanStale)
	}
	return nil
}

func (cli *CLI) checkProjectOwnerRoute(ctx context.Context, execution *projectExecution, connection ownerinventory.Connection) error {
	yard := execution.Loaded.Context
	root := filepath.Join(yard.Paths.DataHome, "owner-inventory")
	if !strings.HasPrefix(filepath.Clean(yard.Paths.StateDir), filepath.Join(root, "routing")+string(filepath.Separator)) {
		return nil
	}
	effective, err := cli.effectiveOwnerConnection(ctx, execution.Loaded, connection)
	if err != nil {
		return fmt.Errorf("%w: revalidate owner project route: %v", domain.ErrPlanStale, err)
	}
	if yard.OwnerEndpoint != connection.Destination ||
		yard.Paths.StateDir != filepath.Join(root, "routing", connection.HostID, yard.OwnerYardName, "projects") ||
		effective.Yards[yard.OwnerYardName].SSHHost != yard.SSHHost {
		return fmt.Errorf("%w: owner project route changed; prepare the command again", domain.ErrPlanStale)
	}
	return nil
}

func (cli *CLI) beginProjectMutation(ctx context.Context, execution *projectExecution) (func(), error) {
	release := func() {}
	if execution != nil && execution.OwnerConnection != nil {
		store := ownerinventory.Connections{Root: filepath.Join(execution.Loaded.Context.Paths.DataHome, "owner-inventory")}
		var err error
		release, err = store.BeginHostMutation(ctx, *execution.OwnerConnection)
		if err != nil {
			return nil, fmt.Errorf("revalidate project owner: %w", err)
		}
		// The lease guards the persisted owner. Compatibility route metadata
		// remains in its registration, so validate that effective route again.
		if err := cli.checkProjectOwnerRoute(ctx, execution, *execution.OwnerConnection); err != nil {
			release()
			return nil, err
		}
	}
	if err := cli.recheckProjectRole(ctx, execution); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (cli *CLI) recheckProjectRole(ctx context.Context, execution *projectExecution) error {
	if execution == nil || !execution.RequiresProjects {
		return nil
	}
	if execution.Loaded.Context.AccessKind == domain.AccessRemote {
		_, err := cli.remoteProjectStateCall(ctx, execution.Loaded.Context, []string{"check-role"})
		if err != nil {
			return fmt.Errorf("recheck project yard role on owner: %w", err)
		}
		return nil
	}
	fresh, err := cli.loadInventoryLoaded(execution.Loaded.Context.YardName, execution.Loaded)
	if err != nil {
		return fmt.Errorf("recheck project yard role: %w", err)
	}
	if err := requireProjectRole(fresh); err != nil {
		return err
	}
	if execution.RequiresActiveTarget && execution.Record.Target != "" {
		return validateProjectTarget(cli.options.RepositoryRoot, fresh, execution.Record.Target)
	}
	return nil
}

func openProjectPreparationStore(ctx context.Context, yard domain.Context) (*state.FileStore, error) {
	if yard.AccessKind == domain.AccessRemote {
		// Canonical routing may be removed concurrently. Preparation must not
		// recreate it or migrate controller records before taking the host lock.
		return state.NewFileStore(yard.Paths.StateDir)
	}
	return openProjectStore(ctx, yard.Paths.StateDir)
}

func (cli *CLI) prepareProjectInventory(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
) (*projectExecution, error) {
	if loaded.Environment["ALLOWS_PROJECTS"] == "false" {
		store, err := openProjectStoreReadOnly(loaded.Context.Paths.StateDir)
		if err != nil {
			return nil, err
		}
		records, err := store.List(ctx)
		if err != nil {
			return nil, err
		}
		if len(records) != 0 {
			return nil, errors.New("selected yard role does not accept work projects; remove existing projects before init or provision")
		}
		return &projectExecution{Loaded: loaded, Arguments: arguments,
			Environment: map[string]string{"SUBYARD_PROJECT_PROFILES": ""}}, nil
	}
	store, err := openProjectStoreReadOnly(loaded.Context.Paths.StateDir)
	if err != nil {
		return nil, err
	}
	records, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	profiles := make([]string, 0)
	for _, record := range records {
		profile := record.Profile
		if profile == "" && record.Target != "" && record.Target != "yard" {
			profile = record.Target
		}
		if profile != "" && domain.SafeName(profile) && !seen[profile] {
			seen[profile] = true
			profiles = append(profiles, profile)
		}
	}
	return &projectExecution{
		Loaded: loaded, Arguments: arguments,
		Environment: map[string]string{"SUBYARD_PROJECT_PROFILES": strings.Join(profiles, " ")},
	}, nil
}

func (cli *CLI) prepareProjectImport(
	ctx context.Context,
	loaded config.Loaded,
	name string,
	arguments []string,
) (*projectExecution, error) {
	path, requestedTarget, targetYard, requestedName, err := parseProjectImportArguments(arguments)
	if err != nil {
		return nil, err
	}
	if name == "bind" && loaded.Context.AccessKind == domain.AccessRemote {
		return nil, errors.New("bind is host-local - use sync or clone")
	}
	hostPath := path
	if !filepath.IsAbs(hostPath) {
		hostPath = filepath.Join(cli.options.WorkingDir, hostPath)
	}
	hostPath, err = filepath.EvalSymlinks(hostPath)
	if err != nil {
		return nil, fmt.Errorf("resolve project path: %w", err)
	}
	hostPath, err = filepath.Abs(hostPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(hostPath)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", path)
	}
	projectName := filepath.Base(hostPath)
	explicitName := requestedName != ""
	if explicitName {
		projectName = requestedName
	}
	if !domain.SafeProjectName(projectName) {
		return nil, fmt.Errorf(
			"invalid project name %q; choose a safe name with --name", projectName,
		)
	}
	selected, err := cli.routeProjectSource(ctx, loaded.Context, hostPath, targetYard)
	if err != nil {
		return nil, err
	}
	selectedLoaded, err := cli.activateProjectContext(selected, loaded, true)
	if err != nil {
		return nil, err
	}
	if name == "bind" && selectedLoaded.Context.AccessKind == domain.AccessRemote {
		return nil, errors.New("bind is host-local - use sync or clone")
	}
	if requestedTarget != "" {
		if err := validateProjectTarget(cli.options.RepositoryRoot, selectedLoaded, requestedTarget); err != nil {
			return nil, err
		}
	}
	store, err := state.NewFileStore(selectedLoaded.Context.Paths.StateDir)
	if err != nil {
		return nil, err
	}
	mode := domain.ProjectMode(name)
	operationID := cli.projectOperationID()
	admission, err := cli.previewProjectAdmission(
		ctx, selectedLoaded, store, hostPath, mode, projectName, explicitName,
	)
	if err != nil {
		return nil, err
	}
	exists := admission.Existing != nil
	target := requestedTarget
	if target == "" && exists {
		target = admission.Existing.Target
	}
	if target == "" {
		target = "yard"
	}
	if err := validateProjectTarget(cli.options.RepositoryRoot, selectedLoaded, target); err != nil {
		if admission.Reservation != nil {
			_ = store.AbortAdmission(ctx, admission.Reservation.OperationID)
		}
		return nil, err
	}
	store, err = openProjectPreparationStore(ctx, selectedLoaded.Context)
	if err != nil {
		return nil, err
	}
	if selectedLoaded.Context.AccessKind != domain.AccessRemote {
		// Selection admission precedes legacy migration. Capture the authoritative
		// consent baseline afterward so our own migration cannot make it stale.
		admission, err = cli.previewProjectAdmission(ctx, selectedLoaded, store, hostPath, mode, projectName, explicitName)
		if err != nil {
			return nil, err
		}
		exists = admission.Existing != nil
		target = requestedTarget
		if target == "" && exists {
			target = admission.Existing.Target
		}
		if target == "" {
			target = "yard"
		}
		if err := validateProjectTarget(cli.options.RepositoryRoot, selectedLoaded, target); err != nil {
			return nil, err
		}
	}
	record := domain.ProjectRecord{
		Schema: 1, IdentityVersion: 2, ProjectID: admission.ProjectID, Name: admission.Name,
		HostPath: hostPath, SourceKey: state.SourceKey(hostPath),
		YardPath: state.YardPath(admission.ProjectID),
		Mode:     mode, SSHHost: selectedLoaded.Context.SSHHost,
		ImportedAt: time.Now().UTC().Format(time.RFC3339), Target: target,
	}
	if exists {
		record.IdentityVersion = admission.Existing.IdentityVersion
		record.ImportedAt = admission.Existing.ImportedAt
		record.RegistrySource = admission.Existing.RegistrySource
	}
	if target != "yard" {
		record.Profile = target
	}
	var bindSource os.FileInfo
	if mode == domain.ProjectBind {
		bindSource = info
	}
	return &projectExecution{
		Loaded: selectedLoaded, Arguments: arguments, Environment: projectSnapshot(record, exists),
		Record: record, Store: store, Commit: projectCommitPut,
		Reservation: admission.Reservation, OperationID: operationID, bindSourceBefore: bindSource,
		ExplicitName: explicitName, RequestedName: projectName,
		PreviewExisting: admission.Existing,
	}, nil
}

func (cli *CLI) prepareProjectClone(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
) (*projectExecution, error) {
	url, name, target, targetYard, explicitName, err := parseProjectCloneArguments(arguments)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(url), ".git")
	}
	if !domain.SafeProjectName(name) {
		return nil, fmt.Errorf("invalid project name %q; choose a safe name with --name", name)
	}
	selected, err := cli.routeProjectSource(ctx, loaded.Context, url, targetYard)
	if err != nil {
		return nil, err
	}
	selectedLoaded, err := cli.activateProjectContext(selected, loaded, true)
	if err != nil {
		return nil, err
	}
	if err := validateProjectTarget(cli.options.RepositoryRoot, selectedLoaded, target); err != nil {
		return nil, err
	}
	store, err := openProjectPreparationStore(ctx, selectedLoaded.Context)
	if err != nil {
		return nil, err
	}
	operationID := cli.projectOperationID()
	admission, err := cli.previewProjectAdmission(
		ctx, selectedLoaded, store, url, domain.ProjectGit, name, explicitName,
	)
	if err != nil {
		return nil, err
	}
	record := domain.ProjectRecord{
		Schema: 1, IdentityVersion: 2, ProjectID: admission.ProjectID, Name: admission.Name,
		HostPath: url, SourceKey: state.SourceKey(url),
		YardPath: state.YardPath(admission.ProjectID),
		Mode:     domain.ProjectGit, SSHHost: selectedLoaded.Context.SSHHost,
		ImportedAt: time.Now().UTC().Format(time.RFC3339), Target: target,
	}
	if target != "yard" {
		record.Profile = target
	}
	return &projectExecution{
		Loaded: selectedLoaded, Arguments: arguments, Environment: projectSnapshot(record, false),
		Record: record, Store: store, Commit: projectCommitPut,
		Reservation: admission.Reservation, OperationID: operationID,
		ExplicitName: explicitName, RequestedName: name,
		PreviewExisting: admission.Existing,
	}, nil
}

func (cli *CLI) projectOperationID() string {
	return cli.ensureOperationID()
}

func (cli *CLI) previewProjectAdmission(
	ctx context.Context,
	loaded config.Loaded,
	store *state.FileStore,
	source string,
	mode domain.ProjectMode,
	requestedName string,
	explicit bool,
	workspaceNames ...string,
) (state.Admission, error) {
	if loaded.Context.AccessKind != domain.AccessRemote {
		if err := requireProjectRole(loaded); err != nil {
			return state.Admission{}, err
		}
		if store == nil {
			return state.Admission{}, errors.New("project store is required")
		}
		return store.PreviewAdmission(ctx, source, mode, requestedName, explicit, workspaceNames...)
	}
	explicitValue := "0"
	if explicit {
		explicitValue = "1"
	}
	output, err := cli.remoteProjectStateCall(ctx, loaded.Context, append([]string{
		"preview", source, string(mode), requestedName, explicitValue,
	}, workspaceNames...))
	if err != nil {
		return state.Admission{}, fmt.Errorf("preview project identity on owner: %w", err)
	}
	var response struct {
		ProjectID string                `json:"projectId"`
		Name      string                `json:"name"`
		Existing  *domain.ProjectRecord `json:"existing"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &response); err != nil {
		return state.Admission{}, fmt.Errorf("decode owner project preview: %w", err)
	}
	if !domain.SafeProjectName(response.ProjectID) || response.Name != response.ProjectID {
		return state.Admission{}, errors.New("owner returned an invalid canonical project preview")
	}
	if response.Existing != nil && (mode == domain.ProjectSync || mode == domain.ProjectGit) {
		return state.Admission{}, errors.New("owner does not support independent project copies; update the owner runtime")
	}
	if response.Existing != nil {
		if err := response.Existing.Validate(response.ProjectID); err != nil {
			return state.Admission{}, fmt.Errorf("validate owner project preview: %w", err)
		}
		if response.Existing.Name != response.Name || response.Existing.Mode != mode {
			return state.Admission{}, errors.New("owner project preview does not match its admission")
		}
	}
	return state.Admission{
		ProjectID: response.ProjectID, Name: response.Name, Existing: response.Existing,
	}, nil
}

func (cli *CLI) prepareExistingProject(
	ctx context.Context,
	loaded config.Loaded,
	name string,
	arguments []string,
	explicit bool,
	readOnly bool,
) (*projectExecution, error) {
	selector, present, err := parseProjectSelector(name, arguments)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	revalidate := name != "shell" && name != "info"
	readOnlyProject := readOnly || name == "remove" || name == "up" || name == "down" || name == "export"
	match, err := cli.resolveProjectForCommand(
		ctx, loaded, selector, explicit, revalidate, true,
	)
	if err != nil {
		return nil, err
	}
	selectedLoaded, err := cli.activateProjectContext(match.Yard, loaded, projectCommandRequiresProjects(name))
	if err != nil {
		return nil, err
	}
	if name == "up" && match.Record.Target != "" {
		if err := validateProjectTarget(cli.options.RepositoryRoot, selectedLoaded, match.Record.Target); err != nil {
			return nil, err
		}
	}
	yardIdentity := ""
	if name == "code" {
		yardIdentity, err = canonicalYardIdentity(selectedLoaded)
		if err != nil {
			return nil, err
		}
	}
	var store *state.FileStore
	if readOnlyProject || selectedLoaded.Context.AccessKind == domain.AccessRemote {
		readOnlyStore, storeErr := openProjectStoreReadOnly(selectedLoaded.Context.Paths.StateDir)
		if storeErr != nil {
			return nil, storeErr
		}
		store = readOnlyStore.store
	} else {
		store, err = openProjectStore(ctx, selectedLoaded.Context.Paths.StateDir)
	}
	if err != nil {
		return nil, err
	}
	execution := &projectExecution{
		Loaded: selectedLoaded, YardIdentity: yardIdentity,
		Arguments: arguments, Environment: projectSnapshot(match.Record, true),
		Record: match.Record, Store: store, OperationID: cli.projectOperationID(),
	}
	before := match.Record
	if selectedLoaded.Context.AccessKind == domain.AccessLocal {
		before, err = store.GetReadOnly(ctx, match.Record.ProjectID)
		if err != nil {
			return nil, err
		}
	}
	execution.recordBefore = &before
	if name == "remove" {
		execution.Commit = projectCommitDelete
		execution.Environment["SUBYARD_PROJECT_REMOVE_SOFT"] = argumentValue(arguments, "--soft")
		if argumentValue(arguments, "--purge") == "1" {
			fmt.Fprintln(cli.options.Stderr, "warning: --purge is deprecated; full removal is the default (--soft keeps the copy)")
		}
	}
	if name == "up" {
		execution.Environment["SUBYARD_PROJECT_REBUILD"] = argumentValue(arguments, "--rebuild")
		if selectedLoaded.Context.AccessKind != domain.AccessRemote && match.Record.Target != "" && match.Record.Target != "yard" {
			execution.Profile, execution.SecretPath, err = loadProjectEnvironmentProfile(
				cli.options.RepositoryRoot, match.Record.Target, selectedLoaded.Environment,
			)
			if err != nil {
				return nil, err
			}
			execution.HostLinks = strings.Split(selectedLoaded.Environment["HOST_LINKS"], "\n")
		}
	}
	// Owner-forwarded commands receive a stable ID, never a controller-only host path.
	if selectedLoaded.Context.AccessKind == domain.AccessRemote &&
		(name == "shell" || name == "up" || name == "down" || name == "info" || name == "remove") {
		execution.Arguments = replaceProjectSelector(name, arguments, match.Record.ProjectID)
	}
	return execution, nil
}

var projectEnvironmentControlKeys = map[string]struct{}{
	"PROFILE_NAME": {}, "PROJECT_ENV_BASE_IMAGE": {}, "CACHES": {}, "DEVICES": {}, "OPTIONAL_FEATURES": {},
	"IMAGE_DOCKERFILE": {}, "IMAGE_CONTEXT": {}, "IMAGE_TAG": {}, "ENV_MOUNTS": {},
	"YARD_MOUNTS": {}, "YARD_CAPS": {}, "YARD_DEVICES": {},
}

func loadProjectEnvironmentProfile(
	root string,
	name string,
	base map[string]string,
) (application.ProjectEnvironmentProfile, string, error) {
	if !domain.SafeName(name) {
		return application.ProjectEnvironmentProfile{}, "", fmt.Errorf("invalid project profile %q", name)
	}
	pkg, err := profile.ResolvePackage(root, name)
	if err != nil {
		return application.ProjectEnvironmentProfile{}, "", err
	}
	if pkg.Config == nil {
		return application.ProjectEnvironmentProfile{}, "", fmt.Errorf("unknown project profile %q", name)
	}
	declared, err := config.ProfileValues(pkg.Config, nil)
	if err != nil {
		return application.ProjectEnvironmentProfile{}, "", err
	}
	values, err := config.ProfileValues(pkg.Config, base)
	if err != nil {
		return application.ProjectEnvironmentProfile{}, "", err
	}
	public := make(map[string]string)
	for key := range declared {
		if _, control := projectEnvironmentControlKeys[key]; !control {
			public[key] = values[key]
		}
	}
	profile := application.ProjectEnvironmentProfile{
		BaseImage: values["PROJECT_ENV_BASE_IMAGE"], Dockerfile: values["IMAGE_DOCKERFILE"],
		Context: values["IMAGE_CONTEXT"], Image: values["IMAGE_TAG"],
		Caches: strings.Fields(values["CACHES"]), Features: strings.Fields(values["OPTIONAL_FEATURES"]),
		Devices: strings.Fields(values["DEVICES"]), Mounts: strings.Fields(values["ENV_MOUNTS"]),
		Environment: public,
	}
	if profile.Dockerfile != "" && profile.Context == "" {
		profile.Context = filepath.Dir(profile.Dockerfile)
	}
	secretsRoot := base["SUBYARD_CONFIG_SECRETS_DIR"]
	if secretsRoot == "" {
		return application.ProjectEnvironmentProfile{}, "",
			errors.New("operator secret directory is required")
	}
	secret := filepath.Join(secretsRoot, "profiles", name, "profile.env")
	info, statErr := os.Lstat(secret)
	if errors.Is(statErr, os.ErrNotExist) {
		secret = ""
	} else if statErr != nil {
		return application.ProjectEnvironmentProfile{}, "", statErr
	} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return application.ProjectEnvironmentProfile{}, "", errors.New("profile.env must be a regular non-symlink file")
	}
	return profile, secret, nil
}

func argumentValue(arguments []string, wanted string) string {
	for _, argument := range arguments {
		if argument == wanted {
			return "1"
		}
	}
	return "0"
}

func (cli *CLI) resolveProjectForCommand(
	ctx context.Context,
	loaded config.Loaded,
	selector string,
	explicit bool,
	force bool,
	readOnly bool,
) (state.Match, error) {
	if !isExplicitProjectPath(selector) {
		return cli.resolveOwnerProject(ctx, loaded, selector, explicit, force, readOnly)
	}
	if explicit {
		selector = stripCurrentYardQualifier(selector, loaded.Context.YardName)
		var store ports.ProjectStore
		var err error
		if readOnly || loaded.Context.AccessKind == domain.AccessRemote {
			store, err = openProjectStoreReadOnly(loaded.Context.Paths.StateDir)
		} else {
			store, err = openProjectStore(ctx, loaded.Context.Paths.StateDir)
		}
		if err != nil {
			return state.Match{}, err
		}
		match, resolveErr := cli.resolveLocalProject(ctx, loaded.Context, store, selector)
		if resolveErr == nil {
			return match, nil
		}
		return state.Match{}, resolveErr
	}
	if readOnly {
		return cli.resolveGlobalProjectReadOnly(ctx, loaded.Context, selector)
	}
	return cli.resolveGlobalProject(ctx, loaded.Context, selector)
}

func (cli *CLI) activateProjectContext(name string, loaded config.Loaded, requireProjects bool) (config.Loaded, error) {
	if selected, ok := cli.inventoryRoutes[name]; ok {
		if requireProjects && selected.Context.AccessKind != domain.AccessRemote {
			if err := requireProjectRole(selected); err != nil {
				return config.Loaded{}, err
			}
		}
		for key, value := range selected.Environment {
			cli.env[key] = value
		}
		cli.env["SUBYARD_YARD"] = name
		cli.env["SUBYARD_CONFIG_LOADED"] = "1"
		cli.env["SUBYARD_ENGINE_CONTEXT"] = "1"
		return selected, nil
	}
	if name == loaded.Context.YardName {
		if requireProjects && loaded.Context.AccessKind != domain.AccessRemote {
			if err := requireProjectRole(loaded); err != nil {
				return config.Loaded{}, err
			}
		}
		return loaded, nil
	}
	selected, err := cli.loadInventoryLoaded(name, loaded)
	if err != nil {
		return config.Loaded{}, err
	}
	if requireProjects && selected.Context.AccessKind != domain.AccessRemote {
		if err := requireProjectRole(selected); err != nil {
			return config.Loaded{}, err
		}
	}
	for _, key := range []string{
		"YARD_NAME", "ACCESS_KIND", "ENVIRONMENT_PROFILES", "YARD_KIND", "YARD_INSTANCE_NAME", "INCUS_PROJECT",
		"INCUS_BRIDGE", "SSH_HOST", "SSH_PORT", "OWNER_ENDPOINT", "OWNER_YARD_NAME", "SHIFT_MODE",
		"FORWARD_SSH_AGENT", "DEV_SUDO", "DEV_UID", "DEV_USER", "NESTED_E2E_VMS",
		"SUBYARD_STATE_DIR", "RESTRICTED_DISK_PATHS", "HOST_BASE", "SRV_VOLUME",
	} {
		delete(cli.env, key)
	}
	for key, value := range selected.Environment {
		cli.env[key] = value
	}
	cli.env["SUBYARD_YARD"] = name
	cli.env["SUBYARD_CONFIG_LOADED"] = "1"
	cli.env["SUBYARD_ENGINE_CONTEXT"] = "1"
	return selected, nil
}

func (cli *CLI) commitProjectExecution(ctx context.Context, execution *projectExecution) error {
	if execution != nil && execution.remoteCopy {
		return nil
	}
	switch execution.Commit {
	case projectCommitNone:
		return nil
	case projectCommitPut:
		if execution.Loaded.Context.AccessKind == domain.AccessRemote {
			action := "upsert"
			arguments := []string{
				action, execution.Record.ProjectID, execution.Record.Name,
				string(execution.Record.Mode), execution.Record.Target,
				execution.Record.HostPath, execution.Record.ImportedAt,
				fmt.Sprint(execution.Record.IdentityVersion),
			}
			if execution.RemoteReserved {
				arguments[0] = "finalize"
				arguments = append(
					[]string{"finalize", execution.OperationID},
					arguments[1:]...,
				)
			}
			if _, err := cli.remoteProjectStateCall(
				ctx, execution.Loaded.Context, arguments,
			); err != nil {
				return errors.New("physical operation completed, but owner project state was not updated; re-run the command")
			}
			execution.RemoteReserved = false
			if err := cli.invalidateOwnerInventory(execution.Loaded); err != nil {
				return fmt.Errorf("owner state updated, but inventory invalidation failed: %w", err)
			}
			if err := execution.Store.Delete(ctx, execution.Record.ProjectID); err != nil &&
				!errors.Is(err, state.ErrNotFound) {
				return fmt.Errorf("owner state updated, but obsolete controller state cleanup failed: %w", err)
			}
			if execution.Reservation != nil {
				if err := execution.Store.AbortAdmission(
					ctx, execution.Reservation.OperationID,
				); err != nil {
					return fmt.Errorf("owner state updated, but controller reservation cleanup failed: %w", err)
				}
				execution.Reservation = nil
			}
			if err := cli.cleanupObsoleteRemoteProjectState(
				ctx, execution.Loaded, execution.Record.ProjectID,
			); err != nil {
				return fmt.Errorf("owner state updated, but obsolete controller state cleanup failed: %w", err)
			}
			return nil
		}
		if execution.Reservation != nil {
			if err := execution.Store.FinalizeOperation(
				ctx, execution.Reservation.OperationID, execution.Record,
			); err != nil {
				return err
			}
			execution.Reservation = nil
			return nil
		}
		return execution.Store.Put(ctx, execution.Record)
	case projectCommitDelete:
		if execution.Loaded.Context.AccessKind == domain.AccessRemote {
			sourceKey := execution.Record.SourceKey
			if sourceKey == "" && execution.Record.HostPath != "" {
				sourceKey = state.SourceKey(execution.Record.HostPath)
			}
			if code := cli.forwardRemote(ctx, execution.Loaded.Context, "_project-state",
				[]string{
					"remove", execution.Record.ProjectID, sourceKey,
				}); code != 0 {
				return errors.New("physical removal completed, but owner project state was not updated; re-run the command")
			}
			if err := cli.invalidateOwnerInventory(execution.Loaded); err != nil {
				return fmt.Errorf("owner state updated, but inventory invalidation failed: %w", err)
			}
			if err := execution.Store.Delete(ctx, execution.Record.ProjectID); err != nil &&
				!errors.Is(err, state.ErrNotFound) {
				return fmt.Errorf("owner state updated, but obsolete controller state cleanup failed: %w", err)
			}
			if err := cli.cleanupObsoleteRemoteProjectState(
				ctx, execution.Loaded, execution.Record.ProjectID,
			); err != nil {
				return fmt.Errorf("owner state updated, but obsolete controller state cleanup failed: %w", err)
			}
			return nil
		}
		return execution.Store.Delete(ctx, execution.Record.ProjectID)
	default:
		return errors.New("unknown project commit")
	}
}

func (cli *CLI) abortProjectExecution(ctx context.Context, execution *projectExecution) {
	if execution == nil {
		return
	}
	if execution.RemoteReserved {
		_, _ = cli.remoteProjectStateCall(
			ctx, execution.Loaded.Context,
			[]string{"abort", execution.OperationID},
		)
		execution.RemoteReserved = false
	}
	if execution.Reservation != nil && execution.Store != nil {
		_ = execution.Store.AbortAdmission(ctx, execution.Reservation.OperationID)
		execution.Reservation = nil
	}
}

func (cli *CLI) reserveRemoteProject(
	ctx context.Context,
	execution *projectExecution,
) error {
	if execution == nil || execution.Commit != projectCommitPut ||
		execution.Loaded.Context.AccessKind != domain.AccessRemote {
		return nil
	}
	explicit := "0"
	if execution.ExplicitName {
		explicit = "1"
	}
	output, err := cli.remoteProjectStateCall(ctx, execution.Loaded.Context, append([]string{
		"reserve", execution.OperationID, execution.Record.HostPath,
		string(execution.Record.Mode), execution.RequestedName, explicit,
	}, execution.WorkspaceNames...))
	if err != nil {
		return fmt.Errorf("reserve project identity on owner: %w", err)
	}
	var response struct {
		ProjectID string                `json:"projectId"`
		Name      string                `json:"name"`
		Reserved  bool                  `json:"reserved"`
		Existing  *domain.ProjectRecord `json:"existing"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &response); err != nil {
		return fmt.Errorf("decode owner project reservation: %w", err)
	}
	if response.Existing == nil && !response.Reserved {
		return errors.New("owner returned neither an existing project nor a reservation")
	}
	if response.Existing != nil && (execution.Record.Mode == domain.ProjectSync || execution.Record.Mode == domain.ProjectGit) {
		return errors.New("owner does not support independent project copies; update the owner runtime")
	}
	if response.Existing != nil {
		if err := response.Existing.Validate(response.ProjectID); err != nil ||
			response.Existing.ProjectID != response.ProjectID ||
			response.Existing.Name != response.Name {
			if err == nil {
				err = errors.New("owner project identity does not match its admission")
			}
			return fmt.Errorf("validate owner project admission: %w", err)
		}
		if response.Existing.Mode != execution.Record.Mode {
			return errors.New("owner project admission returned a different mode")
		}
	} else if !domain.SafeProjectName(response.ProjectID) ||
		response.ProjectID != response.Name {
		if response.Reserved {
			_, _ = cli.remoteProjectStateCall(ctx, execution.Loaded.Context, []string{
				"abort", execution.OperationID,
			})
		}
		return errors.New("owner returned an invalid canonical project identity")
	}
	stale := response.ProjectID != execution.Record.ProjectID ||
		response.Name != execution.Record.Name ||
		(response.Existing == nil) != (execution.PreviewExisting == nil)
	if !stale && response.Existing != nil {
		stale = *response.Existing != *execution.PreviewExisting
	}
	if stale && execution.automaticCopy() && response.Reserved && response.Existing == nil {
		execution.setCopyIdentity(response.ProjectID, response.Name)
		stale = false
	}
	if stale {
		if response.Reserved {
			_, _ = cli.remoteProjectStateCall(ctx, execution.Loaded.Context, []string{
				"abort", execution.OperationID,
			})
		}
		return fmt.Errorf("%w: project identity changed after confirmation", domain.ErrPlanStale)
	}
	if execution.Reservation != nil {
		if err := execution.Store.AbortAdmission(
			ctx, execution.Reservation.OperationID,
		); err != nil {
			if response.Reserved {
				_, _ = cli.remoteProjectStateCall(ctx, execution.Loaded.Context, []string{
					"abort", execution.OperationID,
				})
			}
			return fmt.Errorf("release controller project reservation: %w", err)
		}
		execution.Reservation = nil
	}
	execution.RemoteReserved = response.Reserved
	return nil
}

func (cli *CLI) reserveProjectExecution(
	ctx context.Context,
	execution *projectExecution,
) error {
	if execution == nil || execution.Commit != projectCommitPut || execution.remoteCopy {
		return nil
	}
	if execution.cloneUnborn {
		if err := execution.checkPreparedSource(ctx, cli); err != nil {
			return err
		}
	}
	if execution.Loaded.Context.AccessKind == domain.AccessRemote {
		return cli.reserveRemoteProject(ctx, execution)
	}
	admission, err := execution.Store.Admit(
		ctx, execution.OperationID, execution.Record.HostPath, execution.Record.Mode,
		execution.RequestedName, execution.ExplicitName, execution.WorkspaceNames...,
	)
	if err != nil {
		return fmt.Errorf("reserve project identity: %w", err)
	}
	stale := admission.ProjectID != execution.Record.ProjectID ||
		admission.Name != execution.Record.Name ||
		(admission.Existing == nil) != (execution.PreviewExisting == nil)
	if !stale && admission.Existing != nil {
		stale = *admission.Existing != *execution.PreviewExisting
	}
	if stale && execution.automaticCopy() && admission.Reservation != nil && admission.Existing == nil {
		execution.setCopyIdentity(admission.ProjectID, admission.Name)
		stale = false
	}
	if stale {
		if admission.Reservation != nil {
			_ = execution.Store.AbortAdmission(ctx, admission.Reservation.OperationID)
		}
		return fmt.Errorf("%w: project identity changed after confirmation", domain.ErrPlanStale)
	}
	execution.Reservation = admission.Reservation
	return nil
}

func (cli *CLI) remoteProjectStateCall(
	ctx context.Context,
	yard domain.Context,
	arguments []string,
) ([]byte, error) {
	remote := []string{"yard"}
	if yard.OwnerYardName != "" {
		remote = append(remote, "-Y", yard.OwnerYardName)
	}
	remote = append(remote, "_project-state")
	remote = append(remote, arguments...)
	parts := make([]string, len(remote))
	for index, argument := range remote {
		parts[index] = shellquote.Word(argument)
	}
	line := "SUBYARD_OPERATION_ID=" + shellquote.Word(cli.env["SUBYARD_OPERATION_ID"]) +
		" " + strings.Join(parts, " ")
	sshArguments, err := cli.sshArguments(ctx, yard.OwnerEndpoint,
		[]string{"-T", yard.OwnerEndpoint, "--", "bash", "-lc", shellquote.Word(line)})
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "ssh", sshArguments...)
	command.Dir = cli.options.WorkingDir
	command.Env = environmentList(cli.env, nil)
	command.Stderr = cli.options.Stderr
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

func projectSnapshot(record domain.ProjectRecord, exists bool) map[string]string {
	existsValue := "0"
	if exists {
		existsValue = "1"
	}
	return map[string]string{
		"SUBYARD_PROJECT_SNAPSHOT":  "1",
		"SUBYARD_PROJECT_ID":        record.ProjectID,
		"SUBYARD_PROJECT_NAME":      record.Name,
		"SUBYARD_PROJECT_HOST_PATH": record.HostPath,
		"SUBYARD_PROJECT_YARD_PATH": record.YardPath,
		"SUBYARD_PROJECT_MODE":      string(record.Mode),
		"SUBYARD_PROJECT_SSH_HOST":  record.SSHHost,
		"SUBYARD_PROJECT_TARGET":    record.Target,
		"SUBYARD_PROJECT_PROFILE":   record.Profile,
		"SUBYARD_PROJECT_DEVICE":    state.WorkspaceDeviceFor(record),
		"SUBYARD_PROJECT_EXISTS":    existsValue,
	}
}

func parseProjectImportArguments(arguments []string) (path, target, yard, name string, err error) {
	path = "."
	positional := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-y" || argument == "--yes":
		case argument == "--target":
			index++
			if index >= len(arguments) {
				return "", "", "", "", errors.New("--target needs yard or a profile")
			}
			target = arguments[index]
		case strings.HasPrefix(argument, "--target="):
			target = strings.TrimPrefix(argument, "--target=")
		case argument == "--name":
			index++
			if index >= len(arguments) {
				return "", "", "", "", errors.New("--name needs a project name")
			}
			name = arguments[index]
			if name == "" {
				return "", "", "", "", errors.New("--name needs a project name")
			}
		case strings.HasPrefix(argument, "--name="):
			name = strings.TrimPrefix(argument, "--name=")
			if name == "" {
				return "", "", "", "", errors.New("--name needs a project name")
			}
		case strings.HasPrefix(argument, "@") && len(argument) > 1:
			yard = strings.TrimPrefix(argument, "@")
		case strings.HasPrefix(argument, "-"):
			return "", "", "", "", fmt.Errorf("unknown option %q", argument)
		default:
			if positional {
				return "", "", "", "", errors.New("only one project path may be selected")
			}
			path, positional = argument, true
		}
	}
	return path, target, yard, name, nil
}

func parseProjectCloneArguments(arguments []string) (
	url, name, target, yard string, explicitName bool, err error,
) {
	target = "yard"
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-y" || argument == "--yes":
		case argument == "--target":
			index++
			if index >= len(arguments) {
				return "", "", "", "", false, errors.New("--target needs yard or a profile")
			}
			target = arguments[index]
		case strings.HasPrefix(argument, "--target="):
			target = strings.TrimPrefix(argument, "--target=")
		case argument == "--name":
			index++
			if index >= len(arguments) {
				return "", "", "", "", false, errors.New("--name needs a project name")
			}
			name, explicitName = arguments[index], true
			if name == "" {
				return "", "", "", "", false, errors.New("--name needs a project name")
			}
		case strings.HasPrefix(argument, "--name="):
			name, explicitName = strings.TrimPrefix(argument, "--name="), true
			if name == "" {
				return "", "", "", "", false, errors.New("--name needs a project name")
			}
		case strings.HasPrefix(argument, "@") && len(argument) > 1:
			yard = strings.TrimPrefix(argument, "@")
		case strings.HasPrefix(argument, "-"):
			return "", "", "", "", false, fmt.Errorf("unknown option %q", argument)
		case url == "":
			url = argument
		case name == "":
			name, explicitName = argument, true
		default:
			return "", "", "", "", false, errors.New("too many clone arguments")
		}
	}
	if url == "" {
		return "", "", "", "", false, errors.New("clone needs a git URL")
	}
	return url, name, target, yard, explicitName, nil
}

func parseProjectSelector(name string, arguments []string) (string, bool, error) {
	selector := ""
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		switch argument {
		case "-y", "--yes", "--soft", "--purge", "--root", "--rebuild":
			continue
		case "-h", "--help":
			return "", false, nil
		}
		if strings.HasPrefix(argument, "-") {
			return "", false, fmt.Errorf("unknown option %q", argument)
		}
		if selector != "" {
			return "", false, errors.New("only one project may be selected")
		}
		selector = argument
	}
	if selector == "" {
		if name == "shell" {
			return "", false, nil
		}
		selector = "."
	}
	return selector, true, nil
}

func replaceProjectSelector(name string, arguments []string, id string) []string {
	result := append([]string(nil), arguments...)
	for index, argument := range result {
		if argument == "--" {
			break
		}
		if argument == "-y" || argument == "--yes" || argument == "--root" || argument == "--rebuild" {
			continue
		}
		if !strings.HasPrefix(argument, "-") {
			result[index] = id
			return result
		}
	}
	if name != "shell" {
		result = append(result, id)
	}
	return result
}

func validateProjectTarget(root string, loaded config.Loaded, target string) error {
	if target == "yard" {
		return nil
	}
	if !domain.SafeName(target) {
		return fmt.Errorf("invalid project target %q", target)
	}
	// Remote plans validate selection and package availability on the owner.
	// Inventory routes carry controller configuration, not owner profile state.
	if loaded.Context.AccessKind == domain.AccessRemote {
		return nil
	}
	if !slices.Contains(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]), target) {
		return fmt.Errorf("project target %q is not active on yard %q", target, loaded.Context.YardName)
	}
	pkg, err := profile.ResolvePackage(root, target)
	if err != nil || pkg.Config == nil {
		return fmt.Errorf("unknown project target %q", target)
	}
	return nil
}

func stripCurrentYardQualifier(selector, yard string) string {
	prefix, rest, qualified := strings.Cut(selector, "/")
	if qualified && prefix == yard && rest != "" {
		return rest
	}
	return selector
}
