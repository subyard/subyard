package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testyardmigration"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

// RunReleaseOwnerNative is an inert private component entrypoint. It constructs
// no release transition/reducer and consumes no public authorization grant.
func RunReleaseOwnerNative(ctx context.Context, input *os.File, output io.Writer) error {
	return releaseowner.Serve(ctx, input, output, newReleaseOwnerComponents)
}

func newReleaseOwnerComponents(ctx context.Context, c releaseowner.Context, root *os.File) (releaseowner.Components, error) {
	program, err := New(Options{RepositoryRoot: c.PhysicalRepositoryRoot, Program: "yard-native", Environment: append(os.Environ(), "SUBYARD_HOME="+c.DataHome, "SUBYARD_CONFIG_HOME="+c.ConfigHome), WorkingDir: c.PhysicalRepositoryRoot, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		return nil, err
	}
	uninstall.UseArtifactGuard(c.PhysicalRepositoryRoot)
	return &releaseOwnerComponents{cli: program, context: c, root: root, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, prepared: map[string]releaseSettingsPrepared{}, activation: map[string]releaseActivationPrepared{}}, nil
}

type releaseSettingsPrepared struct {
	value    releaseowner.PreparedSettings
	desired  []byte
	expected config.PersistentFileSnapshot
	files    []config.PersistentPrecondition
}
type releaseActivationPrepared struct {
	originalTarget     *releaseowner.Target
	value              releaseowner.Target
	platform           ports.InitPlatform
	runtime            ports.RuntimeObservation
	route              routeConsumerActivationPort
	before             string
	configs            *configApplyExecution
	nativeConfigs      *reconcileruntime.PreparedConfigs
	nativeIntegrations *reconcileruntime.PreparedIntegrations
	nativeInstalled    *reconcileruntime.PreparedInstalledRuntime
	hostPower          *reconcileruntime.PreparedHostPower
	broker             *reconcileruntime.PreparedTestVMBroker
	configTarget       configTarget
	integration        reconcileruntime.IntegrationPlan
	stage              ports.ReconcileStageID
}
type releaseOwnerComponents struct {
	afterOwnerOriginals  map[string]releaseAfterOwner
	afterOwnerViews      map[string]releaseAfterOwner
	afterOwnerCompletion *releaseowner.AfterOwnerCompletion
	ownerResultsRecipe   string
	ownerResultReader    interface {
		ReadOwnerCreated(ports.NativeOwnerCreateRef) (ports.NativeOwnerCreated, error)
	}
	ownerResults     []ports.NativeOwnerCreated
	ownerChildren    *preparedOwnerChildren
	cli              *CLI
	context          releaseowner.Context
	root             *os.File
	inputs           map[string]config.PersistentFileSnapshot
	overlays         map[string]config.PersistentFileSnapshot
	loaded           map[string]config.Loaded
	yards            []string
	prepared         map[string]releaseSettingsPrepared
	activation       map[string]releaseActivationPrepared
	ingress          releasetransition.V2Ingress
	ingressRust      bool
	prospective      releasetransition.V2SettingsSnapshotView
	activationDraft  *sourceActivationDraft
	activationOwner  *releasetransition.OwnerRegistrationObservation
	activationActual bool
	closers          []io.Closer
	sourceBytes      map[string][]byte
	sourceSize       int
	installedRecipes map[string]*reconcileruntime.PreparedInstalledRuntime
	installedBound   map[string]bool
}

func (c *releaseOwnerComponents) Close() error {
	var err error
	for _, closer := range c.closers {
		err = errors.Join(err, closer.Close())
	}
	c.closers = nil
	return err
}
func (c *releaseOwnerComponents) QueryOperation(ctx context.Context, operation ports.NativeOperation) (ports.NativeOperation, error) {
	incus, _ := c.cli.statusPorts()
	reader, ok := incus.(interface {
		NativeOperationState(context.Context, ports.NativeOperation) (ports.NativeOperation, error)
	})
	if !ok {
		return ports.NativeOperation{}, errors.New("native operation reader unavailable")
	}
	return reader.NativeOperationState(ctx, operation)
}

func (c *releaseOwnerComponents) CaptureRollbackSupport(ctx context.Context, source, target string, owners []string) (ports.RollbackSupport, error) {
	check := func() error {
		for path, expected := range c.inputs {
			actual, err := config.ReadPersistentSnapshotAt(c.root, path)
			if err != nil || config.BindPersistentSnapshot(actual) != config.BindPersistentSnapshot(expected) {
				return domain.ErrPlanStale
			}
		}
		return nil
	}
	if err := check(); err != nil {
		return ports.RollbackSupport{}, err
	}
	if c.yards == nil {
		if _, _, err := c.CaptureInventory(ctx); err != nil {
			return ports.RollbackSupport{}, err
		}
	}
	configuration := c.cli.releaseRuntimeConfig(releaseruntime.RuntimeContext{
		OperatorHome: firstNonempty(c.cli.env["SUBYARD_OPERATOR_HOME"], c.cli.env["HOME"]), DataHome: c.context.DataHome,
		ConfigHome: c.context.ConfigHome, RuntimeRoot: c.context.RuntimeRoot,
	})
	platform := func(root string, loaded config.Loaded) ports.InitPlatform {
		operation := c.cli.releaseActivationOperation()
		operation.options.RepositoryRoot = root
		value := operation.initPlatformWithDispatcher(loaded, nil, filepath.Join(root, "bin", "yard-engine"))
		if runtime, ok := value.(reconcileruntime.Runtime); ok {
			release := string(c.context.Releases.From)
			if root == target {
				release = c.context.Target
			}
			runtime.LogicalRepositoryRoot = filepath.Join(c.context.RuntimeRoot, "releases", release)
			if loaded.ProspectiveFiles != nil {
				runtime.SourceReader = loaded.ProspectiveFiles.ReadAsset
			}
			return runtime
		}
		return value
	}
	configuration.InstalledRuntimePresence = func(ctx context.Context, root string, loaded config.Loaded, owner string) (ports.RuntimePresence, error) {
		return platform(root, loaded).InstalledRuntimePresence(ctx, owner)
	}
	configuration.ProfileRuntimeObservation = func(ctx context.Context, root string, loaded config.Loaded, owner string) (ports.RuntimeObservation, error) {
		value := platform(root, loaded)
		if observer, ok := value.(interface {
			ObserveProfileRuntime(context.Context, string) (ports.RuntimeObservation, error)
		}); ok {
			return observer.ObserveProfileRuntime(ctx, owner)
		}
		observations, err := value.ObserveProfileRuntimes(ctx)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		observation, ok := observations[owner]
		if !ok {
			return ports.RuntimeObservation{}, errors.New("profile runtime component is unavailable")
		}
		return observation, nil
	}
	result, err := releaseruntime.CaptureRollbackSupport(ctx, configuration, source, target, c.context.ConfigHome, owners, c.view())
	if err != nil {
		return ports.RollbackSupport{}, err
	}
	if err := check(); err != nil {
		return ports.RollbackSupport{}, err
	}
	return result, nil
}
func nativeDigest(raw []byte) string     { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }
func nativeValueDigest(value any) string { raw, _ := json.Marshal(value); return nativeDigest(raw) }
func (c *releaseOwnerComponents) read(relative string) (config.PersistentFileSnapshot, error) {
	if value, ok := c.overlays[relative]; ok {
		return value, nil
	}
	if value, ok := c.inputs[relative]; ok {
		return value, nil
	}
	var value config.PersistentFileSnapshot
	var err error
	if prepared, ok := c.prospective.(releasetransition.V2PreparedInputReader); ok && !c.activationActual {
		var matched bool
		value, matched, err = prepared.ReadPreparedInput(filepath.Join(c.context.ConfigHome, relative))
		if err == nil && !matched {
			value, err = config.ReadPersistentSnapshotAt(c.root, relative)
		}
	} else {
		value, err = config.ReadPersistentSnapshotAt(c.root, relative)
	}
	if err != nil {
		return value, err
	}
	total := len(value.Content)
	for _, input := range c.inputs {
		total += len(input.Content)
	}
	if len(c.inputs) >= 2048 || total > 32<<20 {
		return value, errors.New("native config input bound")
	}
	c.inputs[relative] = value
	return value, nil
}
func (c *releaseOwnerComponents) view() *config.ProspectiveFiles {
	return &config.ProspectiveFiles{Root: c.context.ConfigHome, Yards: slices.Clone(c.yards), PinnedRoot: c.root, ReadSnapshot: func(path string) (config.PersistentFileSnapshot, error) {
		relative, err := filepath.Rel(c.context.ConfigHome, path)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
			return config.PersistentFileSnapshot{}, errors.New("native config input scope")
		}
		return c.read(filepath.ToSlash(relative))
	}}
}
func (c *releaseOwnerComponents) CaptureInventory(ctx context.Context) ([]string, []string, error) {
	if err := c.Close(); err != nil {
		return nil, nil, err
	}
	c.inputs = map[string]config.PersistentFileSnapshot{}
	c.overlays = map[string]config.PersistentFileSnapshot{}
	c.loaded = map[string]config.Loaded{}
	c.prepared = map[string]releaseSettingsPrepared{}
	c.activation = map[string]releaseActivationPrepared{}
	c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
	c.installedBound = map[string]bool{}
	c.ownerChildren = nil
	c.afterOwnerOriginals = nil
	c.afterOwnerViews = nil
	c.afterOwnerCompletion = nil
	c.prospective = nil
	c.activationDraft = nil
	c.activationOwner = nil
	c.ingress = nil
	names := map[string]bool{"default": true}
	for _, path := range []string{"yards", config.GitSettingsRelativePath + "/yards"} {
		directory, err := config.ReadPersistentDirectoryAt(c.root, path)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if len(directory.Entries) > 256 {
			return nil, nil, errors.New("native yard inventory bound")
		}
		for _, entry := range directory.Entries {
			name := strings.TrimSuffix(entry, ".env")
			if domain.SafeName(name) {
				names[name] = true
			}
		}
	}
	c.yards = nil
	for name := range names {
		c.yards = append(c.yards, name)
	}
	slices.Sort(c.yards)
	if len(c.yards) > 128 {
		return nil, nil, errors.New("native yard bound")
	}
	owners := c.cli.profileRuntimeIDs()
	return slices.Clone(c.yards), owners, nil
}
func (c *releaseOwnerComponents) load(yard string) (config.Loaded, error) {
	root := c.context.PhysicalRepositoryRoot
	if root == "" {
		root = c.context.RepositoryRoot
	}
	operatorHome := c.cli.env["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = c.cli.env["HOME"]
	}
	if operatorHome == "" {
		operatorHome = c.context.ConfigHome
	}
	return config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: operatorHome, YardName: yard, DisablePrivate: true, ConfigLocked: true, ProspectiveFiles: c.view(), Environment: map[string]string{"SUBYARD_HOME": c.context.DataHome, "SUBYARD_CONFIG_HOME": c.context.ConfigHome, "SUBYARD_CONFIG_DIR": filepath.Join(root, "config")}})
}

func (c *releaseOwnerComponents) platform(loaded config.Loaded, powerYards []domain.Context) ports.InitPlatform {
	root := c.context.PhysicalRepositoryRoot
	if root == "" {
		root = c.context.RepositoryRoot
	}
	platform := c.cli.initPlatformWithDispatcher(loaded, powerYards, filepath.Join(root, "bin", "yard-engine"))
	if runtime, ok := platform.(reconcileruntime.Runtime); ok {
		runtime.LogicalRepositoryRoot = c.context.RepositoryRoot
		runtime.PreparedSources = c.preparedSourceFiles()
		runtime.SourceReader = func(asset config.MaterializedAsset) ([]byte, error) {
			payload, err := loaded.ProspectiveFiles.ReadAsset(asset)
			if err != nil {
				return nil, err
			}
			return c.retainPreparedSource(payload)
		}
		return runtime
	}
	return platform
}
func nativeAssignments(path string, value config.PersistentFileSnapshot) ([]releaseowner.Assignment, error) {
	assignments, err := config.ParsePersistentAssignments(path, value.Content)
	if err != nil {
		return nil, err
	}
	out := make([]releaseowner.Assignment, 0, len(assignments))
	for _, a := range assignments {
		out = append(out, releaseowner.Assignment{Name: a.Name, Value: a.Value, Line: a.Line, Direct: a.Direct, Dynamic: a.Dynamic})
	}
	return out, nil
}
func (c *releaseOwnerComponents) CaptureYard(ctx context.Context, yard string, resolved bool) (releaseowner.YardSettings, error) {
	if !slices.Contains(c.yards, yard) {
		return releaseowner.YardSettings{}, errors.New("uncaptured native yard")
	}
	canonical := "yards/" + yard + "/config.env"
	flat := "yards/" + yard + ".env"
	nested, err := c.read(canonical)
	if err != nil {
		return releaseowner.YardSettings{}, err
	}
	legacy, err := c.read(flat)
	if err != nil {
		return releaseowner.YardSettings{}, err
	}
	snapshot := nested
	label := canonical
	if legacy.Exists {
		snapshot = legacy
		label = flat
	}
	assignments, err := nativeAssignments(label, snapshot)
	if err != nil {
		return releaseowner.YardSettings{}, err
	}
	value := releaseowner.YardSettings{Yard: yard, CanonicalExists: nested.Exists, FlatExists: legacy.Exists, Assignments: assignments, Inherited: []string{}, Sources: []releaseowner.Source{}}
	var loaded config.Loaded
	if resolved {
		loaded, err = c.load(yard)
		if err != nil {
			return value, err
		}
		c.loaded[yard] = loaded
		_, _, group := config.ComponentSelectionSource(loaded.Settings["SSH_PORT"])
		value.Access = string(loaded.Context.AccessKind)
		value.SSHPort = loaded.Context.SSHPort
		value.SSHGroup = group
	}
	for _, fixed := range []struct{ role, path string }{{"shared", "overrides/shared/config.env"}, {"host", "config.env"}, {"git-shared", config.GitSettingsRelativePath + "/config.env"}, {"git-yard", config.GitSettingsRelativePath + "/yards/" + yard + "/config.env"}} {
		snapshot, err := c.read(fixed.path)
		if err != nil {
			return value, err
		}
		assignments, err := nativeAssignments(fixed.path, snapshot)
		if err != nil {
			return value, err
		}
		value.Sources = append(value.Sources, releaseowner.Source{Role: fixed.role, Label: fixed.path, Exists: snapshot.Exists, SHA256: nativeDigest(snapshot.Content), Assignments: assignments})
	}
	for _, layer := range loaded.ConfigurationLayers {
		if !strings.HasPrefix(layer.Path, c.context.ConfigHome+string(filepath.Separator)) || strings.HasPrefix(layer.Path, filepath.Join(c.context.ConfigHome, "yards")+string(filepath.Separator)) || !strings.Contains(layer.Role, "scalar settings") {
			continue
		}
		relative, _ := filepath.Rel(c.context.ConfigHome, layer.Path)
		snapshot, err := c.read(filepath.ToSlash(relative))
		if err != nil {
			return value, err
		}
		assignments, err := nativeAssignments(relative, snapshot)
		if err != nil {
			return value, err
		}
		value.Sources = append(value.Sources, releaseowner.Source{Role: layer.Role, Label: layer.Path, Exists: snapshot.Exists, SHA256: nativeDigest(snapshot.Content), Assignments: assignments})
	}
	// Catalog/template support is a component fact; conversion policy remains in
	// the owning Rust profile module.
	template := ""
	for _, a := range assignments {
		if a.Name == "YARD_TEMPLATE" {
			template = a.Value
		}
	}
	if template != "" {
		for _, definition := range c.cli.catalog.Profiles() {
			if definition.Name == template {
				value.TemplateSupported = true
			}
		}
	}
	return value, nil
}
func (c *releaseOwnerComponents) settingsSource() (string, []config.PersistentPrecondition) {
	files := []config.PersistentPrecondition{}
	for path, snapshot := range c.inputs {
		if strings.HasPrefix(path, "yards/") {
			continue
		}
		files = append(files, config.PersistentPrecondition{Relative: path, Binding: config.BindPersistentSnapshot(snapshot)})
	}
	slices.SortFunc(files, func(a, b config.PersistentPrecondition) int { return strings.Compare(a.Relative, b.Relative) })
	bindings := inputBindings(files)
	return nativeValueDigest(bindings), files
}
func (c *releaseOwnerComponents) PrepareSettings(ctx context.Context, yard string, edits []releaseowner.Edit) (releaseowner.PreparedSettings, error) {
	canonical := "yards/" + yard + "/config.env"
	flat := "yards/" + yard + ".env"
	expected, err := c.read(canonical)
	if err != nil {
		return releaseowner.PreparedSettings{}, err
	}
	legacy, err := c.read(flat)
	if err != nil || expected.Exists && legacy.Exists {
		return releaseowner.PreparedSettings{}, errors.New("native settings target ambiguous")
	}
	target := "canonical"
	path := canonical
	if legacy.Exists {
		target = "flat"
		path = flat
		expected = legacy
	}
	desired := slices.Clone(expected.Content)
	for _, edit := range edits {
		if !domain.SafeID(edit.Setting) {
			return releaseowner.PreparedSettings{}, errors.New("native setting ID")
		}
		desired, err = config.EditPersistentAssignmentContent(path, desired, edit.Setting, edit.Value)
		if err != nil {
			return releaseowner.PreparedSettings{}, err
		}
	}
	source, files := c.settingsSource()
	value := releaseowner.PreparedSettings{Yard: yard, Target: target, ExpectedExists: expected.Exists, ExpectedFile: nativeDigest(expected.Content), DesiredFile: nativeDigest(desired), Source: source, Inputs: inputBindings(files)}
	c.prepared[nativeValueDigest(value)] = releaseSettingsPrepared{value, desired, expected, files}
	c.overlays[path] = config.PersistentFileSnapshot{Exists: true, Content: desired}
	return value, nil
}
func settingsPath(recipe releaseowner.SettingsRecipe) (string, error) {
	if !domain.SafeName(recipe.Yard) {
		return "", errors.New("invalid native settings yard")
	}
	switch recipe.Target {
	case "canonical":
		return "yards/" + recipe.Yard + "/config.env", nil
	case "flat":
		return "yards/" + recipe.Yard + ".env", nil
	}
	return "", errors.New("invalid native target class")
}
func (c *releaseOwnerComponents) settingsInputs(recipe releaseowner.SettingsRecipe) (string, error) {
	path, err := settingsPath(recipe)
	if err != nil {
		return "", err
	}
	if nativeValueDigest(recipe.Inputs) != recipe.Source {
		return "conflict", nil
	}
	for _, input := range recipe.Inputs {
		if strings.HasPrefix(input.Relative, "yards/") {
			return "conflict", nil
		}
		actual, err := config.ReadPersistentSnapshotAt(c.root, input.Relative)
		expected := config.PersistentBinding{Exists: input.Exists, Digest: input.Digest, Identity: input.Identity}
		if err != nil || config.BindPersistentSnapshot(actual) != expected {
			return "conflict", nil
		}
	}
	return path, nil
}
func (c *releaseOwnerComponents) CaptureSettings(ctx context.Context, recipe releaseowner.SettingsRecipe) (string, error) {
	path, err := c.settingsInputs(recipe)
	if err != nil || path == "conflict" {
		return "", domain.ErrPlanStale
	}
	snapshot, err := config.ReadPersistentSnapshotAt(c.root, path)
	if err != nil {
		return "", err
	}
	return nativeValueDigest(struct {
		Exists bool   `json:"exists"`
		SHA256 string `json:"sha256"`
	}{snapshot.Exists, nativeDigest(snapshot.Content)}), nil
}
func (c *releaseOwnerComponents) ObserveSettings(ctx context.Context, recipe releaseowner.SettingsRecipe) (string, error) {
	path, err := c.settingsInputs(recipe)
	if err != nil || path == "conflict" {
		return "conflict", err
	}
	current, err := config.ReadPersistentSnapshotAt(c.root, path)
	if err != nil {
		return "", err
	}
	if current.Exists == recipe.ExpectedExists && nativeDigest(current.Content) == recipe.ExpectedFile {
		return "expected", nil
	}
	if current.Exists && nativeDigest(current.Content) == recipe.DesiredFile {
		return "desired", nil
	}
	return "conflict", nil
}

// ProjectSettings performs a read-only, exact-input assignment edit. Release
// predecessor/successor authorization remains in the Rust journal owner.
func (c *releaseOwnerComponents) ProjectSettings(ctx context.Context, recipe releaseowner.SettingsRecipe, edits []releaseowner.Edit) (string, error) {
	state, err := c.ObserveSettings(ctx, recipe)
	if err != nil || state != "desired" {
		return "", domain.ErrPlanStale
	}
	path, err := settingsPath(recipe)
	if err != nil {
		return "", err
	}
	snapshot, err := config.ReadPersistentSnapshotAt(c.root, path)
	if err != nil {
		return "", err
	}
	content := snapshot.Content
	for _, edit := range edits {
		content, err = config.EditPersistentAssignmentContent(path, content, edit.Setting, edit.Value)
		if err != nil {
			return "", err
		}
	}
	if nativeDigest(content) != recipe.ExpectedFile && len(content) > 0 && content[len(content)-1] == '\n' {
		shorter := content[:len(content)-1]
		if nativeDigest(shorter) == recipe.ExpectedFile {
			content = shorter
		}
	}
	return nativeDigest(content), nil
}
func (c *releaseOwnerComponents) ApplySettings(ctx context.Context, recipe releaseowner.SettingsRecipe) error {
	path, err := settingsPath(recipe)
	if err != nil {
		return err
	}
	if nativeValueDigest(recipe.Inputs) != recipe.Source {
		return domain.ErrPlanStale
	}
	files := make([]config.PersistentPrecondition, 0, len(recipe.Inputs))
	for _, input := range recipe.Inputs {
		if strings.HasPrefix(input.Relative, "yards/") {
			return domain.ErrPlanStale
		}
		files = append(files, config.PersistentPrecondition{Relative: input.Relative, Binding: config.PersistentBinding{Exists: input.Exists, Digest: input.Digest, Identity: input.Identity}})
	}
	fd, err := unix.Openat(int(c.root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	root := os.NewFile(uintptr(fd), "native config guard")
	defer root.Close()
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	for _, input := range files {
		actual, e := config.ReadPersistentSnapshotAt(root, input.Relative)
		if e != nil || config.BindPersistentSnapshot(actual) != input.Binding {
			return domain.ErrPlanStale
		}
	}
	current, err := config.ReadPersistentSnapshotAt(root, path)
	if err != nil {
		return err
	}
	if current.Exists && nativeDigest(current.Content) == recipe.DesiredFile {
		return nil
	}
	if current.Exists != recipe.ExpectedExists || nativeDigest(current.Content) != recipe.ExpectedFile {
		return domain.ErrPlanStale
	}
	desired := slices.Clone(current.Content)
	for _, edit := range recipe.Edits {
		desired, err = config.EditPersistentAssignmentContent(path, desired, edit.Setting, edit.Value)
		if err != nil {
			return err
		}
	}
	if nativeDigest(desired) != recipe.DesiredFile {
		return domain.ErrPlanStale
	}
	return config.CompareAndSwapLocalSettingsAt(root, path, config.BindPersistentSnapshot(current), desired, files, nil, nil)
}
func (c *releaseOwnerComponents) CaptureOwner(ctx context.Context) (releaseowner.OwnerBefore, error) {
	request := c.request()
	port := c.cli.ownerRegistrationTransition(request)
	value, err := port.Prepare(ctx, c)
	before := releaseowner.OwnerBefore{State: string(value.State), Registration: string(value.Registration), Overrides: string(value.Overrides), Controller: string(value.Controller), SharedImages: value.SharedImages}
	if err != nil {
		return before, err
	}
	prepared, err := c.prepareOwnerChildren(ctx, before)
	if err != nil {
		return before, err
	}
	c.ownerChildren = prepared
	if prepared != nil {
		before.PreparedRecipe = prepared.binding()
	}
	return before, nil
}
func (c *releaseOwnerComponents) ApplyOwner(ctx context.Context, before releaseowner.OwnerBefore, token string) error {
	if before.State == string(testyardmigration.StateAbsent) || before.State == string(testyardmigration.StateCurrent) {
		if before.PreparedRecipe != "" {
			return domain.ErrPlanStale
		}
		return c.cli.ownerRegistrationTransition(c.request()).Commit(ctx, releasetransition.OwnerRegistrationObservation{State: releasetransition.OwnerRegistrationState(before.State), Registration: releasetransition.Fingerprint(before.Registration), Overrides: releasetransition.Fingerprint(before.Overrides), Controller: releasetransition.Fingerprint(before.Controller), SharedImages: before.SharedImages, RecoveryToken: token})
	}
	return c.applyOwnerChildren(ctx, before, token)
}

func (c *releaseOwnerComponents) ObserveOwner(ctx context.Context, before releaseowner.OwnerBefore, token string) (string, error) {
	progress, err := c.cli.ownerRegistrationTransition(c.request()).Observe(ctx, releasetransition.OwnerRegistrationObservation{State: releasetransition.OwnerRegistrationState(before.State), Registration: releasetransition.Fingerprint(before.Registration), Overrides: releasetransition.Fingerprint(before.Overrides), Controller: releasetransition.Fingerprint(before.Controller), SharedImages: before.SharedImages, RecoveryToken: token})
	return string(progress), err
}

// CleanupOwner preserves the existing terminal-only owning archive contract.
// Rust supplies the original journal preparation and token; this primitive
// chooses no release outcome and deletes no release/native proof records.
func (c *releaseOwnerComponents) CleanupOwner(ctx context.Context, before releaseowner.OwnerBefore, token string) error {
	return c.cli.ownerRegistrationTransition(c.request()).Commit(ctx, releasetransition.OwnerRegistrationObservation{State: releasetransition.OwnerRegistrationState(before.State), Registration: releasetransition.Fingerprint(before.Registration), Overrides: releasetransition.Fingerprint(before.Overrides), Controller: releasetransition.Fingerprint(before.Controller), SharedImages: before.SharedImages, RecoveryToken: token, TerminalCleanup: true})
}
func (c *releaseOwnerComponents) request() releasetransition.ProcessRequest {
	return releasetransition.ProcessRequest{SchemaVersion: 1, RuntimeRoot: c.context.RuntimeRoot, ConfigHome: c.context.ConfigHome, Yard: c.context.Yard, Target: releasetransition.ReleaseID(c.context.Target), ArtifactDigest: releasetransition.Fingerprint(c.context.Artifact), Direction: c.context.Direction, SourceIngress: c.context.SourceIngress}
}
func (c *releaseOwnerComponents) CaptureActivation(ctx context.Context, owner string, fresh bool) (values []releaseowner.Target, resultErr error) {
	defer func() {
		if resultErr == nil {
			values, resultErr = c.observeAfterOwnerViews(ctx, owner, values)
		}
	}()
	// Each component recipe binds only its own complete source read set.
	// Earlier migration/consumer reads must not make the initial capture differ
	// from a fresh reconstruction of the same approved component inputs.
	c.inputs = map[string]config.PersistentFileSnapshot{}
	c.loaded = map[string]config.Loaded{}
	previousActual := c.activationActual
	c.activationActual = fresh
	defer func() { c.activationActual = previousActual }()
	if fresh {
		c.overlays = map[string]config.PersistentFileSnapshot{}
	}
	for _, yard := range c.activationYards() {
		loaded, err := c.loadActivation(yard)
		if err != nil {
			return nil, err
		}
		c.loaded[yard] = loaded
	}
	if !slices.Contains(append(c.cli.profileRuntimeIDs(), "core-guest-runtime", "key-tools-runtime"), owner) {
		return c.captureCoreActivation(ctx, owner)
	}
	values = []releaseowner.Target{}
	for _, yard := range c.activationYards() {
		loaded, err := c.loadActivation(yard)
		if err != nil {
			return nil, err
		}
		if loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		platform := c.platform(loaded, nil)
		var actual ports.RuntimeObservation
		if coreInstalledRuntimeID(owner) {
			actual, err = platform.ObserveInstalledRuntime(ctx, owner)
		} else if selected, ok := platform.(interface {
			ObserveProfileRuntime(context.Context, string) (ports.RuntimeObservation, error)
		}); ok {
			actual, err = selected.ObserveProfileRuntime(ctx, owner)
		} else {
			return nil, errors.New("selected runtime capture unavailable")
		}
		if err != nil {
			return nil, err
		}
		sourcePort, ok := platform.(interface{ SelectedRuntimeInputs(string) (string, error) })
		if !ok {
			return nil, errors.New("runtime source binding unavailable")
		}
		source, err := sourcePort.SelectedRuntimeInputs(owner)
		if err != nil {
			return nil, err
		}
		instanceUUID := "absent"
		incus, _ := c.cli.statusPorts()
		instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
		if err == nil {
			instanceUUID = instance.Config["volatile.uuid"]
			if instanceUUID == "" {
				return nil, errors.New("missing instance UUID")
			}
		} else if !errors.Is(err, ports.ErrInstanceNotFound) {
			return nil, err
		}
		value := releaseowner.Target{Class: "profile-runtime", ReportedActual: actual.Actual, ReportedDesired: actual.Desired, Owner: owner, Yard: yard, Project: loaded.Context.IncusProject, Instance: loaded.Context.YardInstanceName, InstanceUUID: instanceUUID, State: string(actual.State), Actual: actual.Actual, Desired: actual.Desired, Source: source}
		var installed *reconcileruntime.PreparedInstalledRuntime
		if actual.State == ports.RuntimeStateCurrent || actual.State == ports.RuntimeStateStale {
			runtime, ok := platform.(reconcileruntime.Runtime)
			if !ok {
				return nil, errors.New("installed runtime preparation unavailable")
			}
			if coreInstalledRuntimeID(owner) {
				installed, err = runtime.PrepareInstalledRuntime(ctx, owner)
			} else {
				installed, err = runtime.PrepareProfileRuntimeSources(ctx, owner)
			}
			if err != nil {
				return nil, err
			}
			c.closers = append(c.closers, installed)
			value.PreparedRecipe = installed.Binding()
			value.Source = nativeValueDigest(struct{ Source, Recipe string }{source, value.PreparedRecipe})
			if c.installedRecipes == nil {
				c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
			}
			if _, exists := c.installedRecipes[value.PreparedRecipe]; !exists {
				c.installedRecipes[value.PreparedRecipe] = installed
			}
		}
		if value.Actual == "" {
			value.Actual = nativeValueDigest(struct {
				State string `json:"state"`
			}{value.State})
		}
		if value.Desired == "" {
			value.Desired = nativeValueDigest(struct {
				Owner  string `json:"owner"`
				Target string `json:"target"`
			}{owner, c.context.Target})
		}
		if !coreInstalledRuntimeID(owner) && (actual.State == ports.RuntimeStateCurrent || actual.State == ports.RuntimeStateStale) {
			if runtime, ok := platform.(reconcileruntime.Runtime); ok {
				generation, capable, err := runtime.CaptureProfileCustody(ctx, owner)
				if err != nil {
					return nil, err
				}
				if capable {
					value.Before, err = releaseowner.PrepareFenceRecipe(c.context.Session, value, generation)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		prepared := releaseActivationPrepared{value: value, platform: platform, runtime: actual, nativeInstalled: installed}
		if c.needsAfterOwner(yard) && actual.State != ports.RuntimeStateCurrent && actual.State != ports.RuntimeStateStale {
			prepared, err = c.prepareAfterOwnerRuntime(ctx, prepared)
			if err != nil {
				return nil, err
			}
			value = prepared.value
		}
		values = append(values, value)
		c.activation[activationKey(value)] = prepared
	}
	return values, nil
}
func (c *releaseOwnerComponents) ApplyActivation(ctx context.Context, target releaseowner.Target) error {
	prepared, ok := c.activation[target.Owner+":"+target.Yard+":"+target.Class]
	if ok && prepared.originalTarget != nil && target.AfterOwner == "" {
		resolved, err := c.ResolveOwnerTarget(ctx, *prepared.originalTarget)
		if err != nil || resolved != target {
			return domain.ErrPlanStale
		}
	}
	if !ok || (prepared.value != target && !c.rebindProfileCustody(prepared.value, target, "")) {
		return domain.ErrPlanStale
	}
	if target.Class != "profile-runtime" {
		if prepared.value != target {
			prepared.value = target
			c.activation[target.Owner+":"+target.Yard+":"+target.Class] = prepared
		}
		return c.applyCoreActivation(ctx, target)
	}
	if prepared.nativeInstalled != nil {
		runtime, ok := prepared.platform.(reconcileruntime.Runtime)
		if !ok || target.PreparedRecipe != prepared.nativeInstalled.Binding() || !c.installedBound[target.PreparedRecipe] {
			return domain.ErrPlanStale
		}
		incus, _ := c.cli.statusPorts()
		instance, err := incus.Instance(ctx, target.Project, target.Instance)
		instanceUUID := "absent"
		if err == nil {
			instanceUUID = instance.Config["volatile.uuid"]
		} else if !errors.Is(err, ports.ErrInstanceNotFound) {
			return domain.ErrPlanStale
		}
		if instanceUUID != target.InstanceUUID {
			return domain.ErrPlanStale
		}
		if coreInstalledRuntimeID(target.Owner) {
			return runtime.ApplyPreparedInstalledRuntime(ctx, prepared.nativeInstalled, c.context.Session[:32], prepared.runtime.Actual, prepared.runtime.Desired)
		}
		if target.Before != "" {
			before, err := releaseowner.ParseFenceRecipe(target.Before)
			if err != nil {
				return domain.ErrPlanStale
			}
			return runtime.ApplyPreparedProfileRuntimeSources(ctx, prepared.nativeInstalled, c.context.Session[:32], prepared.runtime.Actual, prepared.runtime.Desired, before.Expected, before.Generation)
		}
		return runtime.ApplyPreparedProfileRuntimeSources(ctx, prepared.nativeInstalled, c.context.Session[:32], prepared.runtime.Actual, prepared.runtime.Desired)
	}
	sourcePort, ok := prepared.platform.(interface{ SelectedRuntimeInputs(string) (string, error) })
	if !ok {
		return c.applyCoreActivation(ctx, target)
	}
	source, err := sourcePort.SelectedRuntimeInputs(target.Owner)
	if err != nil || source != target.Source {
		return domain.ErrPlanStale
	}
	if coreInstalledRuntimeID(target.Owner) {
		return prepared.platform.ApplyInstalledRuntime(ctx, target.Owner, c.context.Session[:32], prepared.runtime.Actual, prepared.runtime.Desired)
	}
	if target.Before != "" {
		before, err := releaseowner.ParseFenceRecipe(target.Before)
		if err != nil {
			return domain.ErrPlanStale
		}
		runtime, ok := prepared.platform.(reconcileruntime.Runtime)
		if !ok {
			return domain.ErrPlanStale
		}
		return runtime.ApplyProfileRuntimeCustody(ctx, target.Owner, c.context.Session[:32], prepared.runtime.Actual, prepared.runtime.Desired, before.Expected, before.Generation)
	}
	return prepared.platform.ApplyProfileRuntime(ctx, target.Owner, c.context.Session[:32], prepared.runtime.Actual, prepared.runtime.Desired)
}

func (c *releaseOwnerComponents) InstalledRuntimeRecipe(hash string) ([]byte, error) {
	if c.ownerChildren != nil && c.ownerChildren.binding() == hash {
		return bytes.Clone(c.ownerChildren.raw), nil
	}
	prepared, ok := c.installedRecipes[hash]
	if !ok {
		return nil, errors.New("unknown installed recipe")
	}
	return prepared.Recipe(), nil
}
func (c *releaseOwnerComponents) BindInstalledRuntimeRecipe(ctx context.Context, hash string, raw []byte) error {
	if fmtHash := sha256.Sum256(raw); hex.EncodeToString(fmtHash[:]) != hash {
		return domain.ErrPlanStale
	}
	{
		var header struct {
			Owner string `json:"owner"`
		}
		if json.Unmarshal(raw, &header) == nil && header.Owner == "owner-registration" {
			if err := c.bindOwnerChildren(ctx, hash, raw); err != nil {
				return err
			}
			c.ownerResults = nil
			c.ownerResultReader = nil
			c.ownerResultsRecipe = ""
			c.afterOwnerOriginals = nil
			c.afterOwnerViews = nil
			c.afterOwnerCompletion = nil
			return nil
		}
	}
	matched := false
	for key, prepared := range c.activation {
		if prepared.value.PreparedRecipe != hash {
			continue
		}
		runtime, ok := prepared.platform.(reconcileruntime.Runtime)
		if !ok {
			return domain.ErrPlanStale
		}
		p, err := runtime.BindInstalledRuntimeContext(ctx, raw)
		if err != nil {
			return err
		}
		if p.Binding() != hash {
			return domain.ErrPlanStale
		}
		if err := p.ShareSources(c.retainPreparedSource); err != nil {
			return err
		}
		prepared.nativeInstalled = p
		if original := p.PreparedBroker(); original != nil {
			prepared.broker = original
		}
		c.activation[key] = prepared
		c.closers = append(c.closers, p)
		matched = true
	}
	if !matched {
		return domain.ErrPlanStale
	}
	if c.installedBound == nil {
		c.installedBound = map[string]bool{}
	}
	c.installedBound[hash] = true
	return nil
}

func sameNativeActivationScope(a, b releaseowner.Target) bool {
	a.State, b.State = "", ""
	a.Actual, b.Actual = "", ""
	a.ReportedActual, b.ReportedActual = "", ""
	a.Before, b.Before = "", ""
	return a == b
}

func (c *releaseOwnerComponents) rebindProfileCustody(current, original releaseowner.Target, successor string) bool {
	if (original.Class != "profile-runtime" && original.Class != "stage") || !sameNativeActivationScope(current, original) {
		return false
	}
	if before, err := releaseowner.ParseFenceVectorRecipe(original.Before); err == nil {
		observed, err := releaseowner.ParseFenceVectorRecipe(current.Before)
		if err != nil || len(observed.Expected) != len(before.Expected) {
			return false
		}
		for index, entry := range before.Expected {
			actual := observed.Expected[index]
			if actual.Role != entry.Role || (actual.Value != entry.Value && actual.Value != before.Generation && (successor == "" || actual.Value != successor)) {
				return false
			}
		}
		return successor != "" || (current.State == original.State && current.Actual == original.Actual && current.ReportedActual == original.ReportedActual)
	}
	before, err := releaseowner.ParseFenceRecipe(original.Before)
	if err != nil {
		return false
	}
	observed, err := releaseowner.ParseFenceRecipe(current.Before)
	if err != nil {
		return false
	}
	allowed := observed.Expected == before.Expected || observed.Expected == before.Generation || successor != "" && observed.Expected == successor
	if successor == "" {
		allowed = allowed && current.State == original.State && current.Actual == original.Actual && current.ReportedActual == original.ReportedActual
	}
	return allowed
}

func (c *releaseOwnerComponents) FenceActivation(ctx context.Context, target releaseowner.Target, successor string) error {
	prepared, ok := c.activation[target.Owner+":"+target.Yard+":"+target.Class]
	if !ok || !c.rebindProfileCustody(prepared.value, target, successor) {
		return domain.ErrPlanStale
	}
	runtime, ok := prepared.platform.(reconcileruntime.Runtime)
	if !ok {
		return domain.ErrPlanStale
	}
	if target.Class == "stage" {
		before, err := releaseowner.ParseFenceVectorRecipe(target.Before)
		if err != nil {
			return domain.ErrPlanStale
		}
		if prepared.hostPower == nil {
			return domain.ErrPlanStale
		}
		if err := c.cli.releaseTransitionPrivilegeAuthorization(ctx); err != nil {
			return err
		}
		if prepared.nativeInstalled == nil || target.PreparedRecipe != prepared.nativeInstalled.Binding() || !c.installedBound[target.PreparedRecipe] {
			return domain.ErrPlanStale
		}
		return runtime.FencePreparedHostPowerRuntimeSources(ctx, prepared.hostPower, prepared.nativeInstalled, before.Expected, before.Generation, successor)
	}
	before, err := releaseowner.ParseFenceRecipe(target.Before)
	if err != nil {
		return domain.ErrPlanStale
	}
	source, err := runtime.SelectedRuntimeInputs(target.Owner)
	if err != nil {
		return err
	}
	if prepared.nativeInstalled != nil {
		source = nativeValueDigest(struct{ Source, Recipe string }{source, prepared.nativeInstalled.Binding()})
		if !c.installedBound[target.PreparedRecipe] {
			return domain.ErrPlanStale
		}
	}
	if source != target.Source {
		return domain.ErrPlanStale
	}
	incus, _ := c.cli.statusPorts()
	instance, err := incus.Instance(ctx, target.Project, target.Instance)
	if err != nil {
		return err
	}
	if instance.Config["volatile.uuid"] != target.InstanceUUID {
		return domain.ErrPlanStale
	}
	var fenceErr error
	if prepared.nativeInstalled != nil {
		fenceErr = runtime.FencePreparedProfileRuntimeSources(ctx, prepared.nativeInstalled, before.Expected, before.Generation, successor)
	} else {
		fenceErr = runtime.FenceProfileRuntime(ctx, target.Owner, before.Expected, before.Generation, successor)
	}
	if fenceErr != nil {
		return fenceErr
	}
	instance, err = incus.Instance(ctx, target.Project, target.Instance)
	if err != nil {
		return err
	}
	if instance.Config["volatile.uuid"] != target.InstanceUUID {
		return domain.ErrPlanStale
	}
	return nil
}

func inputBindings(files []config.PersistentPrecondition) []releaseowner.InputBinding {
	result := make([]releaseowner.InputBinding, 0, len(files))
	for _, f := range files {
		result = append(result, releaseowner.InputBinding{Relative: f.Relative, Exists: f.Binding.Exists, Digest: f.Binding.Digest, Identity: f.Binding.Identity})
	}
	return result
}

func (c *releaseOwnerComponents) coreTarget(owner, class, yard, state, scope, stage, actual, desired, source string) releaseowner.Target {
	return releaseowner.Target{Owner: owner, Class: class, Yard: yard, Project: "host", Instance: "host", InstanceUUID: "host", State: state, ScopeState: scope, Stage: stage, Actual: actual, Desired: desired, Source: source}
}

func (c *releaseOwnerComponents) bindGuestTarget(ctx context.Context, loaded config.Loaded, target releaseowner.Target) (releaseowner.Target, error) {
	incus, _ := c.cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		target.Project, target.Instance, target.InstanceUUID = loaded.Context.IncusProject, loaded.Context.YardInstanceName, "absent"
		return target, nil
	}
	if err != nil {
		return target, err
	}
	uuid := instance.Config["volatile.uuid"]
	if uuid == "" {
		return target, errors.New("missing native target instance UUID")
	}
	target.Project, target.Instance, target.InstanceUUID = loaded.Context.IncusProject, loaded.Context.YardInstanceName, uuid
	return target, nil
}
func (c *releaseOwnerComponents) captureCoreActivation(ctx context.Context, owner string) ([]releaseowner.Target, error) {
	values := []releaseowner.Target{}
	switch owner {
	case "test-yard-route-consumers":
		options, err := c.cli.releaseTransitionTestYardOptions(c.request())
		if err != nil {
			return nil, err
		}
		options.ProspectiveFiles = c.activationFiles()
		port := &testYardRouteConsumerActivation{options: options}
		state, err := port.PrepareOwner(ctx)
		if c.activationOwner != nil && !c.activationActual {
			_, projected, e := c.cli.projectedOwnerApplicability(ctx, c.request(), releasetransition.V2SourceActivationView{Settings: c, OwnerRegistration: c.activationOwner})
			state, err = string(projected), e
		}
		if err != nil {
			return nil, err
		}
		if c.needsAfterOwner(testyardmigration.CurrentYard) {
			before, err := testyardmigration.PrepareRouteConsumers(ctx, options)
			if err != nil {
				return nil, err
			}
			loaded, err := c.loadActivation(testyardmigration.CurrentYard)
			if err != nil {
				return nil, err
			}
			runtime, ok := c.platform(loaded, nil).(reconcileruntime.Runtime)
			if !ok {
				return nil, domain.ErrPlanStale
			}
			native, _ := c.cli.statusPorts()
			reader, ok := native.(testyardmigration.RouteDevicePreparer)
			if !ok {
				return nil, domain.ErrPlanStale
			}
			routes, err := testyardmigration.PrepareOwnerRouteDevices(ctx, options, before, reader)
			if err != nil {
				return nil, err
			}
			p, err := runtime.PrepareAfterOwnerRoutes(ctx, routes, afterOwnerTemplate("routes", before))
			if err != nil {
				return nil, err
			}
			if err = p.ShareSources(c.retainPreparedSource); err != nil {
				p.Close()
				return nil, err
			}
			source := nativeValueDigest(struct{ Input, Scope, Component string }{c.activationSource(), before, p.RouteSourceBinding()})
			desired := nativeValueDigest(struct{ Owner, State string }{owner, "verified"})
			target := c.coreTarget(owner, "route-consumers", testyardmigration.CurrentYard, "deferred", state, "", source, desired, source)
			target.Before = before
			target.PreparedRecipe = p.Binding()
			target.AfterOwner = "v1:routes:" + p.Binding()
			target, err = c.bindGuestTarget(ctx, loaded, target)
			if err != nil {
				p.Close()
				return nil, err
			}
			if c.installedRecipes == nil {
				c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
			}
			c.installedRecipes[p.Binding()] = p
			c.closers = append(c.closers, p)
			c.activation[activationKey(target)] = releaseActivationPrepared{value: target, platform: runtime, route: port, before: before, nativeInstalled: p}
			return []releaseowner.Target{target}, nil
		}
		before := ""
		converged := state == string(testyardmigration.StateAbsent)
		if state == string(testyardmigration.StateCurrent) {
			before, err = port.Prepare(ctx)
			if err != nil {
				return nil, err
			}
			err = port.Verify(ctx, before)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			converged = err == nil
		}
		var scope struct {
			SchemaVersion int  `json:"schemaVersion"`
			Active        bool `json:"active"`
			Consumers     []struct {
				Project  string `json:"project"`
				Instance string `json:"instance"`
				Yard     string `json:"yard"`
				Mounted  bool   `json:"mounted"`
			} `json:"consumers,omitempty"`
		}
		if before != "" {
			if err := json.Unmarshal([]byte(before), &scope); err != nil {
				return nil, err
			}
			for n := range scope.Consumers {
				scope.Consumers[n].Mounted = false
			}
		}
		source := nativeValueDigest(struct {
			Input string
			Scope any
		}{c.activationSource(), scope})
		actual := source
		desired := nativeValueDigest(struct{ Owner, State string }{owner, "verified"})
		status := "stale"
		if converged {
			status = "current"
			actual = desired
		}
		target := c.coreTarget(owner, "route-consumers", "test-yard", status, state, "", actual, desired, source)
		target.Before = before
		prepared := releaseActivationPrepared{value: target, route: port, before: before}
		if before != "" {
			loaded, err := c.loadActivation(testyardmigration.CurrentYard)
			if err != nil {
				return nil, err
			}
			platform := c.platform(loaded, nil)
			runtime, ok := platform.(reconcileruntime.Runtime)
			if !ok {
				return nil, errors.New("prepared route component unavailable")
			}
			native, _ := c.cli.statusPorts()
			reader, ok := native.(testyardmigration.RouteDevicePreparer)
			if !ok {
				return nil, errors.New("prepared route SDK unavailable")
			}
			routes, err := testyardmigration.PrepareRouteDevices(ctx, options, before, reader)
			if err != nil {
				return nil, err
			}
			var broker *reconcileruntime.PreparedTestVMBroker
			if scope.Active {
				broker, err = runtime.PrepareTestVMBrokerRuntime(ctx)
				if err != nil {
					return nil, err
				}
				c.closers = append(c.closers, broker)
			}
			installed, err := runtime.PrepareRouteConsumerRecipe(ctx, routes, broker)
			if err != nil {
				return nil, err
			}
			if err := installed.ShareSources(c.retainPreparedSource); err != nil {
				return nil, err
			}
			if c.installedRecipes == nil {
				c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
			}
			c.installedRecipes[installed.Binding()] = installed
			c.closers = append(c.closers, installed)
			target.PreparedRecipe = installed.Binding()
			target.Source = nativeValueDigest(struct{ Source, Component string }{target.Source, installed.RouteSourceBinding()})
			prepared.value = target
			prepared.platform = platform
			prepared.nativeInstalled = installed
		}
		c.activation[owner+":test-yard:route-consumers"] = prepared
		values = append(values, target)
	case "host-power", "test-vm-broker":
		state := "absent"
		applies := false
		yard := "default"
		stage := ports.ReconcileStagePower
		if owner == "host-power" {
			fact, err := inspectPowerActivationApplicability(c.cli.env)
			if err != nil {
				return nil, err
			}
			state, applies = fact.state, fact.applies
		} else {
			options, err := c.cli.releaseTransitionTestYardOptions(c.request())
			if err != nil {
				return nil, err
			}
			options.ProspectiveFiles = c.activationFiles()
			fact, target, err := testyardmigration.PrepareBrokerRuntimeTarget(ctx, options)
			if c.activationOwner != nil && !c.activationActual {
				projected, _, e := c.cli.projectedOwnerApplicability(ctx, c.request(), releasetransition.V2SourceActivationView{Settings: c, OwnerRegistration: c.activationOwner})
				fact, target, err = projected, testyardmigration.CurrentYard, e
			}
			if err != nil {
				return nil, err
			}
			state = string(fact)
			applies = fact == testyardmigration.BrokerRuntimeActive
			if target != "" {
				yard = target
			}
			stage = ports.ReconcileStageTestVMs
		}
		// Inactive component facts are complete before a yard/platform exists.
		// Match the owning stage observer: do not load an absent canonical
		// registration or construct native execution handles for skipped work.
		if !applies {
			source := nativeValueDigest(struct{ Inputs, State, Stage, NativeSource string }{c.activationSource(), state, string(stage), ""})
			desired := nativeValueDigest(struct{ Owner, State, Target string }{owner, state, c.context.Target})
			target := c.coreTarget(owner, "stage", yard, "current", state, string(stage), desired, desired, source)
			c.activation[owner+":"+yard+":stage"] = releaseActivationPrepared{value: target, stage: stage}
			return []releaseowner.Target{target}, nil
		}
		loaded, err := c.loadActivation(yard)
		if err != nil {
			return nil, err
		}
		var powerYards []domain.Context
		if owner == "host-power" {
			for _, name := range c.activationYards() {
				configuration, err := c.load(name)
				if err != nil {
					return nil, err
				}
				if configuration.Context.AccessKind == domain.AccessLocal {
					powerYards = append(powerYards, configuration.Context)
				}
			}
		} else {
			powerYards = []domain.Context{loaded.Context}
		}
		platform := c.platform(loaded, powerYards)
		if owner == "test-vm-broker" && c.needsAfterOwner(yard) {
			runtime, ok := platform.(reconcileruntime.Runtime)
			if !ok {
				return nil, domain.ErrPlanStale
			}
			p, err := runtime.PrepareAfterOwnerBroker(ctx, afterOwnerTemplate("broker"))
			if err != nil {
				return nil, err
			}
			if err = p.ShareSources(c.retainPreparedSource); err != nil {
				p.Close()
				return nil, err
			}
			nativeSource, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
			if err != nil {
				p.Close()
				return nil, err
			}
			source := nativeValueDigest(struct{ Inputs, State, Stage, NativeSource string }{c.activationSource(), state, string(stage), nativeSource})
			source = nativeValueDigest(struct{ Source, Recipe string }{source, p.BrokerSourceBinding()})
			desired := nativeValueDigest(struct{ Owner, State, Target string }{owner, state, c.context.Target})
			target := c.coreTarget(owner, "stage", yard, "deferred", state, string(stage), nativeValueDigest(struct{ State string }{"deferred"}), desired, source)
			target.PreparedRecipe = p.Binding()
			target.AfterOwner = "v1:broker:" + p.Binding()
			target, err = c.bindGuestTarget(ctx, loaded, target)
			if err != nil {
				p.Close()
				return nil, err
			}
			c.activation[activationKey(target)] = releaseActivationPrepared{value: target, platform: platform, stage: stage, nativeInstalled: p}
			if c.installedRecipes == nil {
				c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
			}
			c.installedRecipes[p.Binding()] = p
			c.closers = append(c.closers, p)
			return []releaseowner.Target{target}, nil
		}
		var hostPower *reconcileruntime.PreparedHostPower
		var broker *reconcileruntime.PreparedTestVMBroker
		var installed *reconcileruntime.PreparedInstalledRuntime
		nativeSource := ""
		if applies {
			runtime, ok := platform.(reconcileruntime.Runtime)
			if !ok {
				return nil, errors.New("prepared host component unavailable")
			}
			nativeSource, err = runtime.SelectedRuntimeInputs("core-guest-runtime")
			if err != nil {
				return nil, err
			}
			if owner == "host-power" {
				hostPower, err = runtime.PrepareHostPowerRuntime(ctx)
				if err != nil {
					return nil, err
				}
				c.closers = append(c.closers, hostPower)
			} else {
				broker, err = runtime.PrepareTestVMBrokerRuntime(ctx)
				if err != nil {
					return nil, err
				}
				c.closers = append(c.closers, broker)
			}
			sourceOwner := "host-power"
			if owner == "test-vm-broker" {
				sourceOwner = "test-vms-host-sink"
			}
			if owner == "host-power" || owner == "test-vm-broker" {
				if broker != nil {
					installed, err = runtime.PrepareTestVMBrokerRecipe(ctx, broker)
				} else {
					installed, err = runtime.PrepareHostInstallerSources(ctx, sourceOwner)
				}
				if err != nil {
					return nil, err
				}
				if err := installed.ShareSources(c.retainPreparedSource); err != nil {
					return nil, err
				}
				c.closers = append(c.closers, installed)
				if c.installedRecipes == nil {
					c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
				}
				c.installedRecipes[installed.Binding()] = installed
			}
		}
		converged := !applies
		if applies {
			converged, err = platform.VerifyStage(ctx, stage)
			if err != nil {
				return nil, err
			}
		}
		source := c.activationSource()
		source = nativeValueDigest(struct{ Inputs, State, Stage, NativeSource string }{source, state, string(stage), nativeSource})
		desired := nativeValueDigest(struct{ Owner, State, Target string }{owner, state, c.context.Target})
		actual := desired
		status := "current"
		if !converged {
			status = "stale"
			actual = nativeValueDigest(struct {
				Desired string
				Drift   bool
			}{desired, true})
		}
		target := c.coreTarget(owner, "stage", yard, status, state, string(stage), actual, desired, source)
		if installed != nil {
			target.PreparedRecipe = installed.Binding()
			component := target.PreparedRecipe
			if broker != nil {
				component = installed.BrokerSourceBinding()
			}
			target.Source = nativeValueDigest(struct{ Source, Recipe string }{target.Source, component})
		}
		if hostPower != nil {
			target.Source = nativeValueDigest(struct{ Inputs, Component string }{target.Source, hostPower.Binding()})
			target.ReportedActual = hostPower.NativeBefore()
			if target.State == "stale" {
				target.Actual = nativeValueDigest(struct{ Desired, NativeBefore string }{target.Desired, target.ReportedActual})
			}
			runtime := platform.(reconcileruntime.Runtime)
			generation, err := runtime.CaptureHostPowerCustody()
			if err != nil {
				return nil, err
			}
			target.Before, err = releaseowner.PrepareFenceVectorRecipe(c.context.Session, target, generation)
			if err != nil {
				return nil, err
			}
		}
		if broker != nil {
			target.Before = broker.Binding()
			target, err = c.bindGuestTarget(ctx, loaded, target)
			if err != nil {
				return nil, err
			}
		}
		c.activation[owner+":"+yard+":stage"] = releaseActivationPrepared{value: target, platform: platform, stage: stage, hostPower: hostPower, broker: broker, nativeInstalled: installed}
		values = append(values, target)
	case "materialized-config":
		for _, yard := range c.activationYards() {
			loaded, err := c.loadActivation(yard)
			if err != nil {
				return nil, err
			}
			if loaded.Context.AccessKind != domain.AccessLocal {
				continue
			}
			target := configTarget{Name: yard, Loaded: loaded}
			execution, err := c.cli.prepareConfigApply(ctx, []configTarget{target}, func() ([]configTarget, error) {
				fresh, err := c.load(yard)
				return []configTarget{{Name: yard, Loaded: fresh}}, err
			})
			if err != nil {
				return nil, err
			}
			assessment := execution.approved[0]
			state := "current"
			if assessment.Changed {
				state = "stale"
			}
			actual := assessment.MaterializedFingerprint
			desired := assessment.DesiredFingerprint
			if actual == "" {
				actual = nativeValueDigest(struct{ State string }{assessment.State})
			}
			if desired == "" {
				desired = actual
			}
			source := c.activationSource()
			value := c.coreTarget(owner, "materialized-files", yard, state, assessment.State, "", actual, desired, source)
			value, err = c.bindGuestTarget(ctx, loaded, value)
			if err != nil {
				return nil, err
			}
			value.ReportedActual = assessment.MaterializedFingerprint
			value.ReportedDesired = assessment.DesiredFingerprint
			scope, _, err := c.cli.materializedIntegrationScope(loaded, c.view().ReadAsset)
			if err != nil {
				return nil, err
			}
			value.Scope = scope
			c.activation[owner+":"+yard+":materialized-files"] = releaseActivationPrepared{value: value, configs: execution, configTarget: target, platform: c.platform(loaded, nil)}

			if assessment.State == "drift" || assessment.State == "converged" {
				prepared := c.activation[owner+":"+yard+":materialized-files"]
				runtime, ok := prepared.platform.(reconcileruntime.Runtime)
				if !ok {
					return nil, errors.New("prepared config component unavailable")
				}
				native, err := runtime.PrepareConfigs(ctx)
				if err != nil {
					return nil, err
				}
				prepared.nativeConfigs = native
				if assessment.Changed {
					value.Before = native.Binding()
				}
				installed, err := runtime.PrepareMaterializedConfigs(ctx, native)
				if err != nil {
					return nil, err
				}
				if err := installed.ShareSources(c.retainPreparedSource); err != nil {
					return nil, err
				}
				if c.installedRecipes == nil {
					c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
				}
				c.installedRecipes[installed.Binding()] = installed
				c.closers = append(c.closers, installed)
				prepared.nativeInstalled = installed
				value.PreparedRecipe = installed.Binding()
				value.Source = nativeValueDigest(struct{ Source, Component string }{value.Source, installed.MaterializedSourceBinding()})
				prepared.value = value
				c.activation[owner+":"+yard+":materialized-files"] = prepared
			}
			if c.needsAfterOwner(yard) && (assessment.State == "absent" || assessment.State == "stopped") {
				prepared := c.activation[owner+":"+yard+":materialized-files"]
				prepared, err = c.prepareAfterOwnerFiles(ctx, prepared)
				if err != nil {
					return nil, err
				}
				value = prepared.value
				c.activation[activationKey(value)] = prepared
			}
			values = append(values, value)
			platform := c.platform(loaded, nil)
			runtime, ok := platform.(reconcileruntime.Runtime)
			if ok && loaded.Integrations.AllowsCodingTools && c.needsAfterOwner(yard) && (assessment.State == "absent" || assessment.State == "stopped") {
				prepared, prepareErr := c.prepareAfterOwnerIntegration(ctx, target, runtime, source)
				if prepareErr != nil {
					return nil, prepareErr
				}
				c.activation[activationKey(prepared.value)] = prepared
				values = append(values, prepared.value)
				continue
			}
			if !ok || !loaded.Integrations.AllowsCodingTools || (assessment.State != "drift" && assessment.State != "converged") {
				continue
			}
			platform, _, err = prepareLegacyIntegrationAdoption(ctx, loaded.Integrations, platform)
			if err != nil {
				return nil, err
			}
			runtime = platform.(reconcileruntime.Runtime)
			plan, err := runtime.IntegrationPlan(ctx)
			if err != nil {
				return nil, err
			}
			state = "current"
			if plan.Changed {
				state = "stale"
			}
			desired = nativeValueDigest(struct{ Scope string }{plan.Scope})
			actual = desired
			if plan.Changed {
				actual = plan.Fingerprint
			}
			value = c.coreTarget(owner, "materialized-integrations", yard, state, "integrations", "", actual, desired, nativeValueDigest(struct{ Input, Scope string }{source, plan.Scope}))
			value, err = c.bindGuestTarget(ctx, loaded, value)
			if err != nil {
				return nil, err
			}
			value.Before = plan.StateBinding()
			value.ReportedActual = plan.Fingerprint
			value.ReportedDesired = plan.Scope
			native, err := runtime.PrepareIntegrations(ctx, plan)
			if err != nil {
				return nil, err
			}
			if plan.Changed {
				value.Before = native.Binding()
			}
			installed, err := runtime.PrepareMaterializedIntegrations(ctx, native)
			if err != nil {
				return nil, err
			}
			if err := installed.ShareSources(c.retainPreparedSource); err != nil {
				return nil, err
			}
			if c.installedRecipes == nil {
				c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
			}
			c.installedRecipes[installed.Binding()] = installed
			c.closers = append(c.closers, installed)
			value.PreparedRecipe = installed.Binding()
			value.Source = nativeValueDigest(struct{ Source, Component string }{value.Source, installed.MaterializedSourceBinding()})
			c.activation[owner+":"+yard+":materialized-integrations"] = releaseActivationPrepared{value: value, platform: runtime, configTarget: target, integration: plan, nativeIntegrations: native, nativeInstalled: installed}
			values = append(values, value)
		}
	default:
		return nil, errors.New("unknown activation component")
	}
	return values, nil
}
func (c *releaseOwnerComponents) applyCoreActivation(ctx context.Context, target releaseowner.Target) error {
	prepared, ok := c.activation[target.Owner+":"+target.Yard+":"+target.Class]
	if !ok || prepared.value != target {
		return domain.ErrPlanStale
	}
	switch target.Class {
	case "route-consumers":
		route, ok := prepared.route.(*testYardRouteConsumerActivation)
		if !ok || prepared.nativeInstalled == nil || target.PreparedRecipe == "" || prepared.nativeInstalled.Binding() != target.PreparedRecipe || !c.installedBound[target.PreparedRecipe] {
			return domain.ErrPlanStale
		}
		runtime, ok := prepared.platform.(reconcileruntime.Runtime)
		if !ok {
			return domain.ErrPlanStale
		}
		native, _ := c.cli.statusPorts()
		writer, ok := native.(interface {
			ApplyPreparedReadOnlyDisk(context.Context, ports.PreparedReadOnlyDisk) error
			CheckPreparedReadOnlyDisk(context.Context, ports.PreparedReadOnlyDisk) error
		})
		if !ok {
			return domain.ErrPlanStale
		}
		options := route.options
		options.RouteDevices = prepared.nativeInstalled.PreparedRoutes()
		if options.RouteDevices == nil || options.RouteDevices.Before != prepared.before {
			return domain.ErrPlanStale
		}
		options.ApplyRouteDevice = writer.ApplyPreparedReadOnlyDisk
		options.CheckRouteDevices = func(ctx context.Context, original *testyardmigration.PreparedRouteDevices) error {
			for _, device := range original.Devices {
				if err := writer.CheckPreparedReadOnlyDisk(ctx, device); err != nil {
					return err
				}
			}
			return nil
		}
		options.RunYard = func(ctx context.Context, yard string, _ io.Writer, args ...string) error {
			if yard != testyardmigration.CurrentYard || !slices.Equal(args, []string{"_migrate", "reconcile-test-vm-broker"}) {
				return domain.ErrPlanStale
			}
			return runtime.ApplyPreparedRouteBroker(ctx, prepared.nativeInstalled)
		}
		port := &testYardRouteConsumerActivation{options: options}
		if err := port.Commit(ctx, prepared.before); err != nil {
			return err
		}
		return port.Verify(ctx, prepared.before)
	case "stage":
		runtime, ok := prepared.platform.(reconcileruntime.Runtime)
		if !ok {
			return errors.New("native stage primitive unavailable")
		}
		if target.Owner == "host-power" {
			if prepared.hostPower == nil {
				return domain.ErrPlanStale
			}
			before, err := releaseowner.ParseFenceVectorRecipe(target.Before)
			if err != nil {
				return domain.ErrPlanStale
			}
			if err := c.cli.releaseTransitionPrivilegeAuthorization(ctx); err != nil {
				return err
			}
			if prepared.nativeInstalled == nil || target.PreparedRecipe != prepared.nativeInstalled.Binding() || !c.installedBound[target.PreparedRecipe] {
				return domain.ErrPlanStale
			}
			return runtime.ApplyPreparedHostPowerRuntimeCustodySources(ctx, prepared.hostPower, prepared.nativeInstalled, before.Expected, before.Generation)
		}
		if target.Owner == "test-vm-broker" {
			if prepared.broker == nil || prepared.broker.Binding() != target.Before {
				return domain.ErrPlanStale
			}
			if target.PreparedRecipe == "" || prepared.nativeInstalled == nil || target.PreparedRecipe != prepared.nativeInstalled.Binding() || !c.installedBound[target.PreparedRecipe] {
				return domain.ErrPlanStale
			}
			return runtime.ApplyPreparedBrokerRecipe(ctx, prepared.nativeInstalled)
		}
		return domain.ErrPlanStale
	case "materialized-files":
		if err := c.cli.refreshConfigApply(ctx, prepared.configs); err != nil {
			return err
		}
		if !prepared.configs.changed() {
			return nil
		}
		runtime, ok := prepared.platform.(reconcileruntime.Runtime)
		if !ok || prepared.nativeConfigs == nil || prepared.nativeConfigs.Binding() != target.Before || prepared.nativeInstalled == nil || prepared.nativeInstalled.Binding() != target.PreparedRecipe || !c.installedBound[target.PreparedRecipe] {
			return domain.ErrPlanStale
		}
		if err := runtime.ApplyPreparedMaterializedConfigs(ctx, prepared.nativeInstalled); err != nil {
			return err
		}
		assessment, err := c.cli.assessConfigTarget(ctx, prepared.configTarget, true)
		if err != nil {
			return err
		}
		if assessment.Changed || assessment.DesiredFingerprint != prepared.configs.approved[0].DesiredFingerprint {
			return domain.ErrPlanStale
		}
		return nil
	case "materialized-integrations":
		unlock, err := lockIntegrationYard(ctx, prepared.configTarget.Loaded)
		if err != nil {
			return err
		}
		defer unlock()
		runtime := prepared.platform.(reconcileruntime.Runtime)
		if prepared.nativeIntegrations == nil || prepared.nativeIntegrations.Binding() != target.Before || prepared.nativeInstalled == nil || prepared.nativeInstalled.Binding() != target.PreparedRecipe || !c.installedBound[target.PreparedRecipe] {
			return domain.ErrPlanStale
		}
		return runtime.ApplyPreparedMaterializedIntegrations(ctx, prepared.nativeInstalled)
	}
	return domain.ErrPlanStale
}

func (c *releaseOwnerComponents) ListYards() ([]string, error) { return slices.Clone(c.yards), nil }
func (c *releaseOwnerComponents) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	return c.view().ReadSnapshot(path)
}
func nativeIngressBinding(value *releaseowner.IngressBinding) *releasetransition.V2IngressBinding {
	if value == nil {
		return nil
	}
	out := &releasetransition.V2IngressBinding{Transaction: releasetransition.TransactionID(value.Transaction), Plan: releasetransition.PlanToken(value.Plan), Releases: value.Releases}
	for _, s := range value.Steps {
		out.Steps = append(out.Steps, releasetransition.V2IngressStepBinding{Kind: releasetransition.V2IngressOperationKind(s.Kind), Checkpoint: releasetransition.StepCheckpoint(s.Checkpoint), Expected: releasetransition.Fingerprint(s.Expected), Desired: releasetransition.Fingerprint(s.Desired), Static: releasetransition.Fingerprint(s.Static)})
	}
	return out
}
func nativeIngressStep(value releaseowner.IngressStep) releasetransition.V2IngressStep {
	return releasetransition.V2IngressStep{Binding: *nativeIngressBinding(&value.Binding), Kind: releasetransition.V2IngressOperationKind(value.Kind), Expected: releasetransition.Fingerprint(value.Expected), Desired: releasetransition.Fingerprint(value.Desired), Static: releasetransition.Fingerprint(value.Static)}
}
func (c *releaseOwnerComponents) CaptureIngress(ctx context.Context, binding *releaseowner.IngressBinding) (releaseowner.IngressFacts, error) {
	return c.captureIngress(ctx, binding, false)
}
func (c *releaseOwnerComponents) CaptureRustIngress(ctx context.Context, binding *releaseowner.IngressBinding) (releaseowner.IngressFacts, error) {
	return c.captureIngress(ctx, binding, true)
}
func (c *releaseOwnerComponents) captureIngress(ctx context.Context, binding *releaseowner.IngressBinding, rust bool) (releaseowner.IngressFacts, error) {
	var ingress releasetransition.V2Ingress
	var err error
	if rust {
		ingress, err = c.sourceOnlyIngress()
	} else {
		ingress, err = c.cli.releaseTransitionIngressFactory(c.request())(c.context.Releases)
	}
	if err != nil {
		return releaseowner.IngressFacts{}, err
	}
	facts, err := ingress.Inspect(ctx, nativeIngressBinding(binding))
	if err != nil {
		return releaseowner.IngressFacts{}, err
	}
	c.ingress = ingress
	c.ingressRust = rust
	c.prospective = facts.Prospective
	if c.prospective != nil {
		yards, err := c.prospective.ListYards()
		if err != nil {
			return releaseowner.IngressFacts{}, err
		}
		c.yards = append(c.yards, yards...)
		slices.Sort(c.yards)
		c.yards = slices.Compact(c.yards)
	}
	result := releaseowner.IngressFacts{Yards: slices.Clone(c.yards), Operations: []releaseowner.IngressOperation{}, Decisions: []releasetransition.RedactedDecision{}, Blockers: []releasetransition.Blocker{}}
	result.Decisions = append(result.Decisions, facts.Decisions...)
	result.Blockers = append(result.Blockers, facts.Blockers...)
	for _, op := range facts.Operations {
		result.Operations = append(result.Operations, releaseowner.IngressOperation{Kind: string(op.Kind), Decision: string(op.Decision), Expected: string(op.Expected), Desired: string(op.Desired), Static: string(op.Static)})
	}
	return result, nil
}
func (c *releaseOwnerComponents) ObserveIngress(ctx context.Context, step releaseowner.IngressStep) (string, error) {
	if c.ingress == nil {
		if _, err := c.captureIngress(ctx, &step.Binding, c.ingressRust); err != nil {
			return "", err
		}
	}
	value, err := c.ingress.Observe(ctx, nativeIngressStep(step))
	return string(value), err
}
func (c *releaseOwnerComponents) ApplyIngress(ctx context.Context, step releaseowner.IngressStep) error {
	if c.ingress == nil {
		if _, err := c.captureIngress(ctx, &step.Binding, c.ingressRust); err != nil {
			return err
		}
	}
	return c.ingress.Apply(ctx, nativeIngressStep(step))
}
func (c *releaseOwnerComponents) VerifyIngress(ctx context.Context, step releaseowner.IngressStep) error {
	if c.ingress == nil {
		if _, err := c.captureIngress(ctx, &step.Binding, c.ingressRust); err != nil {
			return err
		}
	}
	return c.ingress.Verify(ctx, nativeIngressStep(step))
}

func (c *releaseOwnerComponents) activationSource() string {
	_, files := c.settingsSource()
	type binding struct {
		Relative string `json:"relative"`
		Exists   bool   `json:"exists"`
		Digest   string `json:"digest"`
	}
	values := make([]binding, 0, len(files))
	for _, f := range files {
		values = append(values, binding{f.Relative, f.Binding.Exists, f.Binding.Digest})
	}
	return nativeValueDigest(values)
}

func (c *releaseOwnerComponents) retainPreparedSource(payload []byte) ([]byte, error) {
	hash := sha256.Sum256(payload)
	key := hex.EncodeToString(hash[:])
	if retained, ok := c.sourceBytes[key]; ok {
		return retained, nil
	}
	if c.sourceSize+len(payload) > 128<<20 {
		return nil, errors.New("native prepared source byte bound")
	}
	if c.sourceBytes == nil {
		c.sourceBytes = map[string][]byte{}
	}
	c.sourceBytes[key] = payload
	c.sourceSize += len(payload)
	return payload, nil
}
