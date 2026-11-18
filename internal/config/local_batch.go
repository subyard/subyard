package config

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// LocalBatchAssignment carries the facts established for one occurrence. A
// writable setting can have an unavailable replacement, including ambiguity.
type LocalBatchAssignment struct {
	PersistentAssignment
	Writable, Valid bool
}

type localBatch struct {
	catalog             Catalog
	path                string
	scope               SettingScope
	baseline            *PersistentFileSnapshot
	assignments         []LocalBatchAssignment
	replacements        []PersistentAssignment
	valid               map[string]bool
	unavailable         map[string]bool
	templateUnavailable bool
	templateRefusal     error
	unavailableProfiles map[string]bool
	assignmentRefusals  []error
}

// CheckLocalBatchConstraints evaluates the proposed inputs for one yard in
// resolver order. Assignment findings are returned at their argument indices.
func (settings Catalog) CheckLocalBatchConstraints(options LoadOptions, scope SettingScope, path string, baseline *PersistentFileSnapshot, assignments []LocalBatchAssignment) ([]error, error) {
	batch := &localBatch{catalog: settings, scope: scope, path: path, baseline: baseline, assignments: assignments,
		valid: map[string]bool{}, unavailable: map[string]bool{}, unavailableProfiles: map[string]bool{}, assignmentRefusals: make([]error, len(assignments))}
	for _, assignment := range assignments {
		if !assignment.Writable {
			continue
		}
		if _, present := batch.valid[assignment.Name]; !present {
			batch.replacements = append(batch.replacements, assignment.PersistentAssignment)
		}
		batch.valid[assignment.Name] = assignment.Valid
	}
	configDir := options.Environment["SUBYARD_CONFIG_DIR"]
	if configDir == "" {
		configDir = filepath.Join(options.RepositoryRoot, "config")
	}
	for index, assignment := range assignments {
		if assignment.Writable && assignment.Name == "YARD_TEMPLATE" && assignment.Value != "" && domain.SafeName(assignment.Value) {
			_, batch.assignmentRefusals[index] = yardTemplateFile(configDir, options.YardName, assignment.Value, path)
		}
	}
	tracker := newSettingTracker()
	tracker.catalog, tracker.batch = settings, batch
	editErr := batch.checkTargetEdits()
	_, values, err := load(options, tracker)
	return batch.assignmentRefusals, domain.CollectRefusals(editErr, err, batch.checkDependencies(options, tracker, values))
}

// Check known persistent inputs even when the draft cannot reach prospective
// binding. The final prospective inspection also binds their snapshots.
func (batch *localBatch) checkDependencies(options LoadOptions, tracker *settingTracker, values environment) error {
	home := options.Environment["SUBYARD_CONFIG_HOME"]
	seen := map[string]bool{}
	var refusals []error
	check := func(path string) {
		if path == batch.path || seen[path] {
			return
		}
		if relative, err := filepath.Rel(home, path); err != nil || !filepath.IsLocal(relative) || relative == "." {
			return
		}
		seen[path] = true
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return
		}
		_, err := ReadPersistentFileSnapshot(home, path)
		if !errors.Is(err, os.ErrNotExist) {
			refusals = append(refusals, err)
		}
	}
	configDir := options.Environment["SUBYARD_CONFIG_DIR"]
	traces := tracker.traces(values)
	mappings := SyncableFileMappings(Loaded{Catalog: batch.catalog, Settings: traces,
		Context: domain.Context{Paths: domain.RuntimePaths{ConfigDir: configDir}}})
	for _, setting := range traces {
		for _, resolution := range setting.Resolutions {
			check(resolution.Path)
		}
	}
	for _, layer := range tracker.configurationLayers() {
		switch layer.Role {
		case "scalar settings", "Git scalar settings", "shipped profile":
			check(layer.Path)
		case "file settings", "Git file settings":
			for _, mapping := range mappings {
				check(filepath.Join(layer.Path, filepath.FromSlash(mapping.Relative)))
			}
		}
	}
	for _, path := range tracker.assetValidationInputs {
		check(path)
	}
	for _, path := range YardFileCandidates(configDir, home, options.YardName) {
		check(path)
	}
	check(YardResetMarkerPath(home, options.YardName))
	check(filepath.Join(home, SourceRecordRelativePath))
	return domain.CollectRefusals(refusals...)
}

// Use the editor's empty environment and record order. Invalid replacements
// stay in their slots, so expansions that depend on them remain unavailable.
func (batch *localBatch) checkTargetEdits() error {
	if batch.baseline == nil || len(batch.replacements) == 0 {
		return nil
	}
	probe := batch.fork()
	observed := map[string]PersistentAssignment{}
	var refusals []error
	observe := func(name, value string, line int) {
		canonical := CanonicalSettingName(name)
		if canonical != name && batch.valid[canonical] && probe.available(name) {
			refusals = append(refusals, persistentAssignmentPreserved(batch.path, canonical, line))
		}
		observed[canonical] = PersistentAssignment{Name: name, Value: value, Line: line}
	}
	if err := probe.applyTarget(bytes.NewReader(batch.baseline.Content), environment{}, observe, observe, operatorDiscardPolicy(nil, batch.catalog)); err != nil {
		return err
	}
	for _, edit := range batch.replacements {
		if actual := observed[edit.Name]; batch.valid[edit.Name] && probe.available(actual.Name) {
			refusals = append(refusals, persistentAssignmentChanged(batch.path, edit.Name, edit.Value, actual))
		}
	}
	return domain.CollectRefusals(refusals...)
}

func (batch *localBatch) fork() *localBatch {
	probe := *batch
	probe.unavailable = maps.Clone(batch.unavailable)
	return &probe
}

func (batch *localBatch) available(names ...string) bool {
	for _, name := range names {
		name = CanonicalSettingName(name)
		if unavailable, tracked := batch.unavailable[name]; tracked {
			if unavailable {
				return false
			}
		} else if batch.templateUnavailable {
			if definition, found := batch.catalog.LookupSetting(name); found && definition.allows(ScopeShipped) {
				return false
			}
		}
	}
	return true
}

func (batch *localBatch) setAvailable(name string, available bool) {
	batch.unavailable[CanonicalSettingName(name)] = !available
}

func (tracker *settingTracker) applyScalarSource(path, source string, values environment, scope SettingScope, syncable bool, observe assignmentObserver, ignore ignoredObserver) error {
	batch := tracker.batch
	if batch == nil {
		return tracker.catalog.applyEnvFileValidatedFrom(path, source, values, scope, syncable, observe, ignore, tracker.prospectiveReader())
	}
	var reader io.Reader
	if path == batch.path && batch.baseline != nil {
		reader = bytes.NewReader(batch.baseline.Content)
	} else if path == batch.path {
		// A protected snapshot can fail for a readable target, such as a
		// hard-linked file. Open it without following links or waiting on a FIFO.
		root := values["SUBYARD_CONFIG_HOME"]
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, exists, err := openPersistentFileAt(root, relative)
		if err != nil {
			return sourceAccessError(path, "scalar settings", err)
		}
		if exists {
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return sourceAccessError(path, "scalar settings", err)
			}
			if !info.Mode().IsRegular() {
				return sourceError(path, 0, "", "scalar settings", "target must be a regular file", nil)
			}
			reader = file
		} else {
			reader = bytes.NewReader(nil)
		}
	} else {
		file, err := os.Open(source)
		if err != nil {
			return sourceAccessError(path, "scalar settings", err)
		}
		defer file.Close()
		reader = file
	}
	return tracker.catalog.evaluateScalarSource(reader, path, values, scope, syncable, observe, ignore, batch)
}

// applyTarget replays the editor's record order with typed replacements. An
// unavailable replacement occupies its slot without supplying a scalar value.
func (batch *localBatch) applyTarget(reader io.Reader, values environment, observe, discard assignmentObserver, policy discardPolicy) error {
	content, err := io.ReadAll(io.LimitReader(reader, (8<<20)+1))
	if err != nil {
		return sourceAccessError(batch.path, "scalar settings", err)
	}
	if len(content) > 8<<20 {
		return sourceError(batch.path, 0, "", "scalar settings", "target exceeds its size bound", nil)
	}
	records, err := batch.catalog.persistentEditRecords(batch.path, content, batch.replacements, false)
	if err != nil {
		return err
	}
	policy.batch = batch
	line := 1
	for _, record := range records {
		if record.replacement != nil {
			valid := batch.valid[record.name]
			batch.setAvailable(record.name, valid)
			if valid {
				values[record.name] = record.replacement.Value
			} else {
				delete(values, record.name)
			}
			observe(record.name, values[record.name], line)
			line += 1 + strings.Count(record.replacement.Value, "\n")
			continue
		}
		if text := strings.TrimSpace(string(record.content)); text != "" && !strings.HasPrefix(text, "#") {
			policy.report = func(name, value string) { discard(name, value, line) }
			if err := applyRecordRetired(text, values, func(name, value string) { observe(name, value, line) }, policy); err != nil {
				return sourceContext(batch.path, line, recordSettingKey(text), "scalar settings", err)
			}
		}
		line += bytes.Count(record.content, []byte{'\n'})
		if !bytes.HasSuffix(record.content, []byte{'\n'}) {
			line++
		}
	}
	return nil
}

func (batch *localBatch) checkConstraints(options LoadOptions, root, configDir, yard string, values environment, tracker *settingTracker, defaults, normalization settingLayerID, prior []error) error {
	refusals := append(slices.Clone(prior), normalizeAgentAssetPaths(values, tracker))
	templateRefusal := batch.templateRefusal
	for index, assignment := range batch.assignments {
		if !assignment.Writable {
			continue
		}
		switch assignment.Name {
		case "YARD_TEMPLATE":
			batch.assignmentRefusals[index] = domain.CollectRefusals(batch.assignmentRefusals[index], templateRefusal)
			templateRefusal = nil
		case "CODING_TOOL_INTEGRATIONS":
			requested := strings.Fields(assignment.Value)
			names := checkUnknownIntegrations(values, requested, batch.catalog.RetiredIntegration, batch.integrationMetadataKnown)
			batch.assignmentRefusals[index] = names
			if batch.available("ALLOWS_CODING_TOOLS") && codingToolsForbidden(values) &&
				len(requested) != 0 && batch.scope == ScopeYard {
				refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigRoleForbidsCodingTools))
			}
			for _, id := range requested {
				if knownIntegration(values, id) && !batch.catalog.RetiredIntegration(id) &&
					batch.integrationDependenciesAvailable(values, []string{id}) {
					_, err := batch.catalog.ResolveIntegrationSelection(values, []string{id})
					refusals = append(refusals, err)
				}
			}
		case "ENVIRONMENT_PROFILES":
			if !assignment.Valid || values[assignment.Name] != assignment.Value {
				authored := cloneEnvironment(values)
				profiles := slices.DeleteFunc(strings.Fields(assignment.Value), func(name string) bool { return !domain.SafeName(name) })
				authored[assignment.Name] = strings.Join(profiles, " ")
				refusals = append(refusals, checkProfileConstraints(configDir, yard, authored, batch.available, true))
			}
		}
	}
	refusals = append(refusals, templateRefusal)
	// Role checks use the fact of a nonempty explicit selection. Closure and
	// listener activation additionally require the selection and its metadata.
	profilesAvailable := batch.available("ENVIRONMENT_PROFILES")
	if batch.available("ALLOWS_CODING_TOOLS") {
		refusals = append(refusals, integrationRoleRefusal(values, tracker))
	}
	if batch.available("CODING_TOOL_INTEGRATIONS", "ALLOWS_CODING_TOOLS") &&
		batch.integrationDependenciesAvailable(values, strings.Fields(values["CODING_TOOL_INTEGRATIONS"])) {
		_, err := resolveLoadedIntegrations(values, tracker)
		refusals = append(refusals, err)
		batch.setAvailable("CODING_TOOL_INTEGRATIONS", err == nil)
	} else {
		batch.setAvailable("CODING_TOOL_INTEGRATIONS", false)
	}
	if batch.available("ALLOWS_HOST_ACCESS") && values["ALLOWS_HOST_ACCESS"] == "false" {
		for _, owner := range batch.catalog.Integrations() {
			for _, artifact := range owner.Artifacts {
				if artifact.SourceSetting != "" && batch.available(artifact.SourceSetting) && strings.TrimSpace(values[artifact.SourceSetting]) != "" {
					refusals = append(refusals, namedRefusal(domain.ConfigRoleForbidsSetting, domain.ConfigSettingName, artifact.SourceSetting))
				}
			}
		}
	}
	refusals = append(refusals, checkProfileConstraints(configDir, yard, values, batch.available, profilesAvailable))
	if profilesAvailable {
		refusals = append(refusals, batch.resolveProfiles(configDir, yard, values))
	}

	ctx, err := contextFrom(root, yard, values, tracker, defaults, normalization)
	refusals = append(refusals, err)
	refusals = append(refusals, batch.checkPorts(root, options, ctx, values, tracker))
	return domain.CollectRefusals(refusals...)
}

func (batch *localBatch) integrationMetadataKnown(values map[string]string, id string) bool {
	if knownIntegration(values, id) || !domain.SafeName(id) {
		return true
	}
	// An invalid declaration can make a custom name undecidable.
	for _, suffix := range integrationMetadataSuffixes {
		if batch.unavailable["AGENT_"+id+"_"+suffix] {
			return false
		}
	}
	return true
}

func (batch *localBatch) integrationDependenciesAvailable(values environment, requested []string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if seen[id] {
			return true
		}
		seen[id] = true
		key := "AGENT_" + id + "_DEPENDS"
		if !batch.integrationMetadataKnown(values, id) || !batch.available(key) {
			return false
		}
		return !slices.ContainsFunc(strings.Fields(values[key]), func(dependency string) bool { return !visit(dependency) })
	}
	return !slices.ContainsFunc(requested, func(id string) bool { return !visit(id) })
}

func (batch *localBatch) resolveProfiles(configDir, yard string, values environment) error {
	selection, err := resolveEnvironmentProfilesWith(yard, values, batch.catalog, func(name string) (bool, error) {
		eligible, _, err := EnvironmentProfileEligibility(configDir, yard, values, batch.catalog, name)
		if err != nil || !batch.profilePolicyAvailable(name) {
			batch.unavailableProfiles[name] = true
			return false, err
		}
		return eligible, nil
	})
	values["ENVIRONMENT_PROFILES"] = strings.Join(selection.Effective, " ")
	return err
}

func (batch *localBatch) profilePolicyAvailable(name string) bool {
	if !batch.available("EXCLUSIVE_ENVIRONMENT_PROFILE") {
		return false
	}
	for _, definition := range batch.catalog.Profiles() {
		if definition.Name == name {
			for setting := range definition.DisabledWhen {
				if !batch.available(setting) {
					return false
				}
			}
		}
	}
	return true
}

func (batch *localBatch) checkPorts(root string, options LoadOptions, ctx domain.Context, values environment, tracker *settingTracker) error {
	collisions := newPortCollisions()
	unchecked := func(string) bool { return false }
	var endpointErr error
	if batch.available("ACCESS_KIND", "SUBYARD_HOME", "ENVIRONMENT_PROFILES") && filepath.IsAbs(ctx.Paths.DataHome) {
		unchecked, endpointErr = applySavedResourceEndpoints(root, options, ctx, values, tracker)
	}
	if ctx.AccessKind != domain.AccessRemote || !batch.available("ACCESS_KIND") {
		for _, definition := range batch.catalog.SettingCatalog() {
			if owner, profile := strings.CutPrefix(definition.Owner, "profile:"); profile && definition.HostListener && values[definition.Name] == "" &&
				(!batch.available("ACCESS_KIND", "SUBYARD_HOME", "ENVIRONMENT_PROFILES") || batch.unavailableProfiles[owner]) {
				batch.setAvailable(definition.Name, false)
			}
		}
	}
	collisions.unchecked = func(name string) bool {
		definition, _ := batch.catalog.LookupSetting(name)
		owner, profile := strings.CutPrefix(definition.Owner, "profile:")
		return unchecked(name) || !batch.available(name) ||
			(strings.HasPrefix(definition.Owner, "integration:") && !batch.available("CODING_TOOL_INTEGRATIONS")) ||
			(profile && (!batch.available("ENVIRONMENT_PROFILES") || batch.unavailableProfiles[owner]))
	}
	derivedErr := normalizeDerivedPortValues(values, tracker, func(name string) bool { return !unchecked(name) && batch.available(name) })
	addDerivedPortCollisions(values, tracker, collisions)
	const web = "WEB_PREVIEW_HOST_PORT"
	var webErr error
	if batch.available(web) && (values[web] != "" || batch.available("SSH_PORT")) {
		webErr = normalizeWebPreviewPort(values, tracker, ctx.SSHPort, collisions)
	}
	return domain.CollectRefusals(append(collisions.refusals, endpointErr, derivedErr, webErr)...)
}

func (tracker *settingTracker) inputsAvailable(names ...string) bool {
	return tracker.batch == nil || tracker.batch.available(names...)
}

// Context normalization is pure. Only keep findings whose source settings
// were available, including both sides of context-dependent checks.
func (batch *localBatch) contextRefusalAvailable(cause error) bool {
	var refusal *domain.ConfigRefusal
	if !errors.As(cause, &refusal) {
		return true
	}
	switch refusal.Code() {
	case domain.ConfigAccessKindInvalid:
		return batch.available("ACCESS_KIND")
	case domain.ConfigYardKindInvalid:
		return batch.available("YARD_KIND")
	case domain.ConfigNestedE2EVMsRequireContainer:
		return batch.available("NESTED_E2E_VMS", "YARD_KIND")
	case domain.ConfigContextIdentityRequired:
		return batch.available("YARD_INSTANCE_NAME", "INCUS_PROJECT", "SSH_HOST", "DEV_USER")
	case domain.ConfigShiftModeInvalid:
		return batch.available("SHIFT_MODE")
	case domain.ConfigDevUIDNegative:
		return batch.available("DEV_UID")
	case domain.ConfigSSHPortRange:
		return batch.available("ACCESS_KIND", "SSH_PORT")
	case domain.ConfigOwnerEndpointRequired:
		return batch.available("ACCESS_KIND", "OWNER_ENDPOINT")
	case domain.ConfigOwnerYardNameRequired:
		return batch.available("ACCESS_KIND", "OWNER_YARD_NAME")
	case domain.ConfigPathNotAbsolute, domain.ConfigPathNotNormalized, domain.ConfigPathTooBroad:
		for name, setting := range map[string]string{"host base": "HOST_BASE", "data home": "SUBYARD_HOME", "storage path": "STORAGE_PATH", "state dir": "SUBYARD_STATE_DIR"} {
			if strings.HasPrefix(refusal.Error(), name+" ") {
				return batch.available(setting)
			}
		}
	}
	return true
}
