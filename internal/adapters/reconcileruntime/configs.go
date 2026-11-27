package reconcileruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

type guestConfigFile struct {
	readAsset       func(config.MaterializedAsset) ([]byte, error)
	integration     string
	setting         string
	label           string
	source          string
	destination     string
	approvalSources []string
	ownedFormat     string
}

// PreparedConfigs retains the exact component source/payload/destination set.
// Its private fields cannot be reconstructed from a binding supplied over IPC.
type PreparedConfigs struct {
	files             []guestConfigFile
	payloads          map[string][]byte
	entries           []integrationArtifact
	binding           string
	runtime           string
	instance          string
	sources           map[configSourceKey]configSourceBytes
	packageSources    []config.MaterializedAsset
	packageIdentities map[string]string
}

type configSourceKey struct {
	path string
}
type configSourceBytes struct {
	payload []byte
	exists  bool
}

func configBindingEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		if strings.HasPrefix(entry, "SUBYARD_OPERATION_ID=") || strings.HasPrefix(entry, "SUBYARD_SUDO_PREAUTHORIZED=") {
			continue
		}
		result = append(result, entry)
	}
	return result
}
func (runtime Runtime) configRuntimeBinding() string {
	raw, _ := json.Marshal(struct {
		Project, Instance, User string
		Environment             []string
	}{runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, runtime.devUser(), runtime.bindingEnvironment(runtime.Environment)})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// RefreshConfigs writes each guest config file only over its consent token.
// Agent skills keep their own receipts in refresh-agent-skills.sh and are not
// compare-and-write files.
func (runtime Runtime) RefreshConfigs(ctx context.Context, consent *ports.ConfigConsent) error {
	prepared, err := runtime.PrepareConfigs(ctx)
	if err != nil {
		return err
	}
	paths, err := runtime.agentSkillSources()
	if err != nil {
		return err
	}
	if len(paths) != 0 {
		if err := runtime.runScript(ctx, runtime.Stderr, "refresh-agent-skills.sh", "apply"); err != nil {
			return err
		}
	}
	return runtime.applyPreparedConfigs(ctx, prepared, consent)
}
func (runtime Runtime) PrepareConfigs(ctx context.Context) (*PreparedConfigs, error) {
	if runtime.Executor == nil {
		return nil, errors.New("Incus executor is required")
	}
	if !domain.SafeName(runtime.devUser()) {
		return nil, errors.New("invalid developer user")
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return nil, firstError(err, errors.New("yard instance is missing"))
	}
	if !strings.EqualFold(state.Instance.Status, "running") {
		return nil, errors.New("yard is not running")
	}
	return runtime.prepareConfigPayloads(state.Instance.Config["volatile.uuid"])
}

// prepareConfigPayloads captures only the fixed owning file selection and its
// original bytes. Future owner children use it without inventing a guest UUID.
func (runtime Runtime) prepareConfigPayloads(instance string) (*PreparedConfigs, error) {
	files, err := runtime.guestConfigFiles()
	if err != nil {
		return nil, err
	}
	sources := map[configSourceKey]configSourceBytes{}
	reader := func(asset config.MaterializedAsset) ([]byte, error) {
		key := configSourceKey{asset.Source}
		if value, exists := sources[key]; exists {
			if !value.exists {
				return nil, os.ErrNotExist
			}
			return value.payload, nil
		}
		var payload []byte
		var err error
		if runtime.SourceReader != nil {
			payload, err = runtime.SourceReader(asset)
		} else {
			payload, err = asset.ReadSource()
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		sources[key] = configSourceBytes{payload, err == nil}
		return payload, err
	}
	packageSources, err := runtime.artifactPackageSources()
	if err != nil {
		return nil, err
	}
	packageIdentities := map[string]string{}
	for _, source := range packageSources {
		if _, err := reader(source); err != nil {
			return nil, err
		}
		identity, err := materializedSourceIdentity(source.Source)
		if err != nil {
			return nil, err
		}
		packageIdentities[source.Source] = identity
	}
	for index := range files {
		files[index].readAsset = reader
	}
	output := runtime.Stdout
	if output == nil {
		output = io.Discard
	}
	fmt.Fprintf(output, "Refresh agent configs in %s\n", runtime.Yard.YardInstanceName)
	// Read once so the inventory intent describes exactly the bytes applied.
	payloads := make(map[string][]byte)
	entries := []integrationArtifact{}
	if len(files) > 2048 {
		return nil, errors.New("prepared config file bound")
	}
	bytes := 0
	var refusals []error
	for _, file := range files {
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			refusals = append(refusals, owned(file.integration, fmt.Errorf("%s: %w", file.label, err)))
			continue
		}
		bytes += len(payload)
		if bytes > 32<<20 {
			return nil, errors.New("prepared config payload bound")
		}
		payloads[file.destination] = payload
		entry := integrationArtifact{ID: file.integration, Kind: "file", Path: file.destination, Digest: fmt.Sprintf("%x", sha256.Sum256(payload))}
		if file.ownedFormat != "" {
			entry.Kind, entry.Format = "structured", file.ownedFormat
		}
		entries = append(entries, entry)
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return nil, err
	}

	bindings := map[string]string{}
	for destination, payload := range payloads {
		bindings[destination] = fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	for _, source := range packageSources {
		bindings["package:"+runtime.bindingPath(source.Source)] = fmt.Sprintf("%x", sha256.Sum256(sources[configSourceKey{source.Source}].payload))
	}
	type sourceBinding struct {
		Integration, Label, Source, Destination, OwnedFormat string
	}
	bindingsSources := make([]sourceBinding, 0, len(files))
	for _, file := range files {
		bindingsSources = append(bindingsSources, sourceBinding{file.integration, file.label, runtime.bindingPath(file.source), file.destination, file.ownedFormat})
	}
	raw, err := json.Marshal(struct {
		Payloads map[string]string
		Entries  []integrationArtifact
		Runtime  string
		Sources  []sourceBinding
		Instance string
	}{bindings, entries, runtime.configRuntimeBinding(), bindingsSources, instance})
	if err != nil {
		return nil, err
	}
	return &PreparedConfigs{files: files, payloads: payloads, entries: entries, binding: fmt.Sprintf("%x", sha256.Sum256(raw)), runtime: runtime.configRuntimeBinding(), instance: instance, sources: sources, packageSources: packageSources, packageIdentities: packageIdentities}, nil
}

// configFiles are the guest files a refresh compares with: each file's
// confirmed consent token, and for a refresh that plans itself, or a file the
// confirmation did not cover, the bytes this writer observes right before it writes.
func (runtime Runtime) configFiles(ctx context.Context, prepared *PreparedConfigs, consent *ports.ConfigConsent) (map[string]configmaterial.File, error) {
	files := map[string]configmaterial.File{}
	if consent != nil {
		if consent.InstanceUUID != prepared.instance {
			return nil, fmt.Errorf("%w: the yard instance changed after its configuration refresh was confirmed", domain.ErrPlanStale)
		}
		for _, file := range consent.Files {
			files[file.Destination] = configmaterial.File{Destination: file.Destination, Token: file.Token}
		}
	}
	var unconsented []guestConfigFile
	for _, file := range prepared.files {
		if _, known := files[file.destination]; !known && !slices.ContainsFunc(unconsented, func(other guestConfigFile) bool { return other.destination == file.destination }) {
			unconsented = append(unconsented, file)
		}
	}
	observed, err := runtime.observeConfigFiles(ctx, unconsented, func(file guestConfigFile) ([]byte, error) {
		if payload, exists := prepared.payloads[file.destination]; exists {
			return payload, nil
		}
		return nil, os.ErrNotExist
	})
	for _, file := range observed {
		files[file.Destination] = file
	}
	return files, err
}

// observeConfigFiles is the one guest observation of the files with a source.
func (runtime Runtime) observeConfigFiles(ctx context.Context, files []guestConfigFile, read func(guestConfigFile) ([]byte, error)) ([]configmaterial.File, error) {
	var refusals []error
	var desired []configmaterial.Desired
	for _, file := range files {
		payload, err := read(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			refusals = append(refusals, fmt.Errorf("observe %s: %w", file.label, err))
			continue
		}
		desired = append(desired, configmaterial.Desired{Label: file.label, Destination: file.destination, Format: file.ownedFormat, Payload: payload})
	}
	if err := domain.CollectRefusals(refusals...); err != nil || len(desired) == 0 {
		return nil, err
	}
	yard := runtime.Yard
	yard.DevUser = runtime.devUser()
	return configmaterial.Observe(ctx, runtime.Executor, yard, desired)
}

func (runtime Runtime) applyPreparedConfigs(ctx context.Context, prepared *PreparedConfigs, consent *ports.ConfigConsent) error {
	if prepared == nil || prepared.runtime != runtime.configRuntimeBinding() {
		return domain.ErrPlanStale
	}
	var refusals []error
	for _, entry := range prepared.entries {
		payload, exists := prepared.payloads[entry.Path]
		if !exists || fmt.Sprintf("%x", sha256.Sum256(payload)) != entry.Digest {
			refusals = append(refusals, fmt.Errorf("%s: recorded configuration payload missing or changed: %w", entry.Path, domain.ErrPlanStale))
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return err
	}
	files, payloads, entries := prepared.files, prepared.payloads, prepared.entries
	evidence, err := runtime.prepareConfigDiagnostics(ctx, prepared)
	if err != nil {
		return err
	}
	output := runtime.Stdout
	if output == nil {
		output = io.Discard
	}
	observed, err := runtime.configFiles(ctx, prepared, consent)
	if err != nil {
		return err
	}
	if _, err := runtime.integrationInventory(ctx, "configs-prepare", entries, ""); err != nil {
		return err
	}
	// Each file is its own compare-and-write: a refused file keeps the others
	// going, and only a yard whose every file converged commits the inventory.
	copied := make(map[string]bool)
	for _, file := range files {
		payload, exists := payloads[file.destination]
		if !exists {
			fmt.Fprintf(output, "  [ ok ] %s: no source — skipping\n", file.label)
			continue
		}
		guest := observed[file.destination]
		guest.Label = file.label
		if state := guest.State(); state != "converged" && state != "drift" {
			err = configmaterial.FileRefusal{Yard: runtime.Yard.YardName, File: guest}
		} else {
			err = runtime.applyGuestConfig(ctx, file, payload, guest.Token)
		}
		if refusal := new(configmaterial.FileRefusal); errors.As(err, refusal) {
			refusals = append(refusals, *refusal)
			continue
		}
		if err != nil {
			return domain.CollectRefusals(append(refusals, owned(file.integration, fmt.Errorf("apply %s: %w", file.label, err)))...)
		}
		copied[file.label] = true
		fmt.Fprintf(output, "  [ ok ] %s -> ~%s/%s\n",
			file.label, runtime.devUser(), strings.TrimPrefix(file.destination, "/home/"+runtime.devUser()+"/"))
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return err
	}
	if _, err := runtime.integrationInventory(ctx, "configs-commit", entries, "", integrationInventoryCapture{config: evidence}); err != nil {
		return err
	}
	for _, agent := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		if !copied[agent+" config"] && !copied[agent+" rules"] {
			fmt.Fprintf(output, "  [ ok ] %s: no default config — skipping\n", agent)
		}
	}
	fmt.Fprintln(output, "  [ ok ] Agent configs refreshed.")
	return nil
}

func (runtime Runtime) prepareConfigDiagnostics(ctx context.Context, prepared *PreparedConfigs) (*configDiagnosticCapture, error) {
	catalog, err := config.LoadCatalog(runtime.RepositoryRoot)
	if err != nil {
		return nil, err
	}
	hasSetting := false
	for _, file := range prepared.files {
		definition, ok := catalog.LookupSetting(file.setting)
		hasSetting = hasSetting || ok && !definition.Sensitive && definition.Kind == config.SettingFile && definition.Application == config.SettingConfigApply
	}
	if !hasSetting {
		return nil, nil
	}
	baseline, err := runtime.integrationInventory(ctx, "settings", nil, "")
	if err != nil {
		return nil, err
	}
	var key []byte
	if baseline.Diagnostics == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
	} else {
		key, _ = hex.DecodeString(baseline.Diagnostics.Key)
	}
	result := &configDiagnosticCapture{Key: hex.EncodeToString(key), Files: map[string]configDiagnosticCaptureFile{}}
	for _, file := range prepared.files {
		definition, ok := catalog.LookupSetting(file.setting)
		if !ok || definition.Sensitive || definition.Kind != config.SettingFile || definition.Application != config.SettingConfigApply {
			continue
		}
		source, ok := prepared.sources[configSourceKey{file.source}]
		if !ok || !source.exists {
			continue
		}
		result.Files[file.setting] = configDiagnosticCaptureFile{Path: file.source, Destination: file.destination, Digest: config.ConfigurationInputDigest(file.setting, definition.Kind, key, source.payload)}
	}
	return result, nil
}

func (runtime Runtime) ConfigsConverged(ctx context.Context) (bool, error) {
	if runtime.Executor == nil {
		return false, errors.New("Incus executor is required")
	}
	if !domain.SafeName(runtime.devUser()) {
		return false, errors.New("invalid developer user")
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return false, firstError(err, errors.New("yard instance is missing"))
	}
	if !strings.EqualFold(state.Instance.Status, "running") {
		return false, errors.New("yard is not running")
	}
	files, err := runtime.guestConfigFiles()
	if err != nil {
		return false, err
	}
	ready, err := runtime.guestConfigsConverged(ctx, files)
	if err != nil || !ready {
		return ready, err
	}
	paths, err := runtime.agentSkillSources()
	if err != nil {
		return false, err
	}
	if len(paths) != 0 {
		return runtime.agentSkillsConverged(ctx, paths)
	}
	return true, nil
}

// guestConfigsConverged is the observation that config status and release
// readiness use too: an unreadable or unassessable file is not converged, so
// init refreshes it and its refusal names the file.
func (runtime Runtime) guestConfigsConverged(ctx context.Context, files []guestConfigFile) (bool, error) {
	observed, err := runtime.observeConfigFiles(ctx, files, guestConfigFile.readSource)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(observed, func(file configmaterial.File) bool { return file.State() != "converged" }), nil
}

func (runtime Runtime) applyGitIdentity(ctx context.Context) error {
	if runtime.Executor == nil {
		return errors.New("Incus executor is required")
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return firstError(err, errors.New("yard instance is missing"))
	}
	if !strings.EqualFold(state.Instance.Status, "running") {
		return errors.New("yard is not running")
	}
	user := runtime.devUser()
	if !domain.SafeName(user) {
		return errors.New("invalid developer user")
	}
	name := runtime.environmentValue("GIT_USER_NAME")
	email := runtime.environmentValue("GIT_USER_EMAIL")
	if name == "" {
		name = runtime.hostGitValue("user.name")
	}
	if email == "" {
		email = runtime.hostGitValue("user.email")
	}
	if name != "" {
		if err := runtime.runGuestAsDev(ctx,
			[]string{"git", "config", "--global", "--replace-all", "user.name", name}); err != nil {
			return err
		}
	}
	if email != "" {
		if err := runtime.runGuestAsDev(ctx,
			[]string{"git", "config", "--global", "--replace-all", "user.email", email}); err != nil {
			return err
		}
	}
	if err := runtime.runGuestAsDev(ctx,
		[]string{"git", "config", "--global", "--replace-all", "safe.directory", "*"}); err != nil {
		return err
	}
	if runtime.Stdout != nil {
		fmt.Fprintf(runtime.Stdout, "  [ ok ] git config ready for %s\n", user)
	}
	return nil
}

func (runtime Runtime) guestConfigFiles() ([]guestConfigFile, error) {
	return runtime.guestConfigFilesWithSourceReader(runtime.SourceReader)
}

func (runtime Runtime) guestConfigFilesWithSourceReader(reader func(config.MaterializedAsset) ([]byte, error)) ([]guestConfigFile, error) {
	user := runtime.devUser()
	files := []guestConfigFile{}
	values := runtimeEnvironment(runtime.Environment)
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	sources, err := config.SelectMaterializationSources(values, user, config.MaterializationRuntime, definitions...)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		asset := source.Asset
		files = append(files, guestConfigFile{readAsset: reader, integration: source.Integration, setting: asset.Setting, label: source.Label, source: asset.Source, destination: asset.Destination, approvalSources: asset.ApprovalSources, ownedFormat: asset.OwnedFormat})
	}

	return files, nil
}

func (file guestConfigFile) readSourceAsset(asset config.MaterializedAsset) ([]byte, error) {
	if file.readAsset != nil {
		return file.readAsset(asset)
	}
	return asset.ReadSource()
}

func (file guestConfigFile) readSource() ([]byte, error) {
	asset := config.MaterializedAsset{Source: file.source, ApprovalSources: file.approvalSources}
	payload, sourceErr := asset.ReadContent(file.readSourceAsset)
	if sourceErr != nil && !os.IsNotExist(sourceErr) {
		return nil, sourceErr
	}
	payload, present, err := config.PrepareMaterializationContent(config.MaterializationRuntime, config.MaterializationContent{Present: sourceErr == nil, Payload: payload})
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, sourceErr
	}
	return payload, nil
}

func (runtime Runtime) preserveInvalidConfigCommand(path string) string {
	return runtime.preserveSiblingCommand(path, "invalid")
}

// preserveSiblingCommand renames path within its directory to a sibling named
// <name>.subyard-<label>-<digest>, or only .subyard-<label>-<digest> when the
// name leaves no room. The rename keeps owner, mode and parent access, and a
// repeat keeps the earlier copy as a numbered backup within the name limit.
func (runtime Runtime) preserveSiblingCommand(path, label string) string {
	name := filepath.Base(path)
	digest := sha256.Sum256([]byte(name))
	sibling := fmt.Sprintf("%s.subyard-%s-%x", name, label, digest[:4])
	// Leave room for the numbered backup suffix .~NNN~.
	if len(sibling) > 248 {
		sibling = fmt.Sprintf(".subyard-%s-%x", label, digest[:8])
	}
	return shellquote.Command([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "mv", "-T", "--backup=numbered", "--", path, filepath.Join(filepath.Dir(path), sibling)})
}

// unownedInvalidConfig keeps its local text and gives activation the single
// preservation action; its message names the step, owner and the follow-up.
type unownedInvalidConfig struct{ text, message, retry string }

func (err unownedInvalidConfig) Error() string { return err.text }
func (err unownedInvalidConfig) ActivationDiagnostic() (string, string) {
	return err.message, err.retry
}

func (runtime Runtime) applyGuestConfig(ctx context.Context, file guestConfigFile, payload []byte, token string) error {
	if file.ownedFormat == "" {
		return runtime.writeGuestFile(ctx, file.label, file.destination, payload, token)
	}
	mode := configmaterial.ModeApply
	if runtime.appliedIntegrationInspection {
		// An installed-view repair never takes back released ownership.
		mode = configmaterial.ModeApplyInstalled
	}
	request, err := configmaterial.ApplyRequest(file.ownedFormat, mode, runtime.devUser(), file.destination, runtime.Yard.DevUID, payload, token)
	if err != nil {
		return err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if refused, ok := configmaterial.Refused(configmaterial.File{Label: file.label, Destination: file.destination, Token: token}, result, err); ok {
		return configmaterial.FileRefusal{Yard: runtime.Yard.YardName, File: refused}
	}
	if err != nil || result.ExitCode != 0 {
		var completed *ports.GuestExitError
		if result.ExitCode == 2 && (err == nil || errors.As(err, &completed) && completed.Code == 2) && configmaterial.UnownedInvalidDestination(result.Stderr, file.ownedFormat) {
			preserve := runtime.preserveInvalidConfigCommand(file.destination)
			repair := "yard config apply"
			if domain.SafeName(runtime.Yard.YardName) {
				repair = "yard -Y " + runtime.Yard.YardName + " config apply"
			}
			scope := "integration"
			if owner := runtime.declaredOwner(file.integration); owner != "" {
				scope += " " + owner
			}
			scope += " apply " + string(stepConfigs)
			if yard := publicToken(runtime.Yard.YardName); yard != "" {
				scope += " in yard " + yard
			}
			return unownedInvalidConfig{
				text:    fmt.Sprintf("unowned invalid configuration preserved; next: run %s; then run %s", preserve, repair),
				message: fmt.Sprintf("%s: unowned invalid configuration at %q preserved; after the next command, rerun the pending release activation", scope, file.destination),
				retry:   preserve,
			}
		}
		return errors.New("structured configuration apply failed")
	}
	return nil
}

func (runtime Runtime) writeGuestFile(ctx context.Context, label, destination string, payload []byte, token string) error {
	request, err := configmaterial.RawRequest(runtime.devUser(), destination, runtime.Yard.DevUID, payload, token)
	if err != nil {
		return err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if refused, ok := configmaterial.Refused(configmaterial.File{Label: label, Destination: destination, Token: token}, result, err); ok {
		return configmaterial.FileRefusal{Yard: runtime.Yard.YardName, File: refused}
	}
	if err == nil && result.ExitCode != 0 {
		err = errors.New("guest config write failed")
	}
	return err
}

func (runtime Runtime) runGuestAsDev(ctx context.Context, command []string) error {
	if !domain.SafeName(runtime.devUser()) {
		return errors.New("invalid developer user")
	}
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	return runtime.runGuest(ctx, ports.InstanceExecRequest{
		Command: command, User: uint32(uid), Group: uint32(uid),
		Environment: map[string]string{"HOME": "/home/" + runtime.devUser()},
	})
}

func (runtime Runtime) runGuest(ctx context.Context, request ports.InstanceExecRequest) error {
	result, err := runtime.Executor.Exec(
		ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if err == nil && result.ExitCode == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("guest command exited with status %d", result.ExitCode)
}

func (runtime Runtime) hostGitValue(key string) string {
	git, err := runtime.executableFromPath("git")
	if err != nil {
		return ""
	}
	command := exec.Command(git, "config", "--global", "--get", key)
	command.Env = runtime.Environment
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (runtime Runtime) devUser() string {
	if runtime.Yard.DevUser != "" {
		return runtime.Yard.DevUser
	}
	return runtime.environmentDefault("DEV_USER", "dev")
}

func safeGuestRelativePath(value string) (string, error) {
	if value == "" || filepath.IsAbs(value) {
		return "", errors.New("path must be relative")
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path leaves the developer home")
	}
	return clean, nil
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

// Artifact declarations enter the existing captured-source grammar. This adds no
// recipe fields and leaves descriptorless source/fingerprint bytes unchanged.
func (runtime Runtime) artifactPackageSources() ([]config.MaterializedAsset, error) {
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	var sources []config.MaterializedAsset
	for _, definition := range definitions {
		if len(definition.Artifacts) == 0 {
			continue
		}
		paths := []string{"integration.json"}
		for _, artifact := range definition.Artifacts {
			paths = append(paths, artifact.PackageSources()...)
		}
		for _, source := range paths {
			path, err := definition.ArtifactSourcePath(source)
			if err != nil {
				return nil, err
			}
			sources = append(sources, config.MaterializedAsset{Source: path})
		}
	}
	return sources, nil
}
