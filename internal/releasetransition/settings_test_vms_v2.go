package releasetransition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const (
	maximumSettingsV2Yards           = 128
	maximumSettingsV2InspectionBytes = 32 << 20
)

type settingsV2SettingLookup func(string) (config.SettingDefinition, bool)

type testVMSettingsV2Capability struct {
	configHome         string
	candidateConfigDir string
	inherited          map[string]struct{}
	lookupSetting      settingsV2SettingLookup
	snapshotView       V2SettingsSnapshotView
	parseAssignment    func(string, []byte) ([]config.PersistentAssignment, error)
}

type settingsV2Plan struct {
	Files     []settingsV2FilePlan
	Decisions []RedactedDecision
	Blockers  []Blocker
}

type settingsV2FilePlan struct {
	LocalSSHPort        bool
	Yard                string
	Path                string
	Expected            config.PersistentFileSnapshot
	Desired             []byte
	Inheritance         Fingerprint
	ExpectedFingerprint Fingerprint
	DesiredFingerprint  Fingerprint
	Decision            Decision
	Before              []settingsV2BeforeValue
}

type settingsV2BeforeValue struct {
	Setting string `json:"setting"`
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

type settingsV2OverlayView struct {
	base      V2SettingsSnapshotView
	snapshots map[string]config.PersistentFileSnapshot
}

func settingsViewAfterPlan(
	configHome string,
	base V2SettingsSnapshotView,
	files []settingsV2FilePlan,
) V2SettingsSnapshotView {
	if len(files) == 0 {
		return base
	}
	if base == nil {
		base = posixV2SettingsSnapshotView{configHome: configHome}
	}
	view := &settingsV2OverlayView{
		base: base, snapshots: make(map[string]config.PersistentFileSnapshot, len(files)),
	}
	for _, file := range files {
		view.snapshots[file.Path] = config.PersistentFileSnapshot{
			Exists: true, Content: slices.Clone(file.Desired),
		}
	}
	return view
}

func (view *settingsV2OverlayView) ListYards() ([]string, error) {
	return view.base.ListYards()
}

func (view *settingsV2OverlayView) PreparedInputPaths() ([]string, error) {
	var paths []string
	if inventory, ok := view.base.(V2PreparedInputPaths); ok {
		var err error
		paths, err = inventory.PreparedInputPaths()
		if err != nil {
			return nil, err
		}
	}
	for path := range view.snapshots {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func (view *settingsV2OverlayView) ReadSnapshot(
	path string,
) (config.PersistentFileSnapshot, error) {
	if snapshot, exists := view.snapshots[path]; exists {
		snapshot.Content = slices.Clone(snapshot.Content)
		return snapshot, nil
	}
	return view.base.ReadSnapshot(path)
}

func (view *settingsV2OverlayView) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	if snapshot, exists := view.snapshots[path]; exists {
		snapshot.Content = slices.Clone(snapshot.Content)
		return snapshot, true, nil
	}
	if reader, ok := view.base.(V2PreparedInputReader); ok {
		return reader.ReadPreparedInput(path)
	}
	return config.PersistentFileSnapshot{}, false, nil
}

func newTestVMSettingsV2Capability(
	configHome string,
	inheritedSettingIDs []string,
	views ...V2SettingsSnapshotView,
) *testVMSettingsV2Capability {
	inherited := make(map[string]struct{}, len(inheritedSettingIDs))
	for _, setting := range inheritedSettingIDs {
		inherited[setting] = struct{}{}
	}
	view := V2SettingsSnapshotView(posixV2SettingsSnapshotView{configHome: configHome})
	if len(views) != 0 && views[0] != nil {
		view = views[0]
	}
	return &testVMSettingsV2Capability{
		configHome: configHome, inherited: inherited,
		lookupSetting:   config.LookupSetting,
		snapshotView:    view,
		parseAssignment: config.ParsePersistentAssignments,
	}
}

func (capability *testVMSettingsV2Capability) Inspect() (settingsV2Plan, error) {
	if capability == nil || capability.configHome == "" {
		return settingsV2Plan{}, errors.New("absolute settings migration config home is required")
	}
	if _, native := capability.snapshotView.(posixV2SettingsSnapshotView); native && !filepath.IsAbs(capability.configHome) {
		return settingsV2Plan{}, errors.New("absolute settings migration config home is required")
	}
	if blocker := capability.validateCatalog(); blocker != nil {
		return settingsV2Plan{Blockers: []Blocker{*blocker}}, nil
	}
	inherited, inheritance, blocker, err := capability.observeInheritedSettings()
	if err != nil {
		return settingsV2Plan{}, err
	}
	if blocker != nil {
		return settingsV2Plan{Blockers: []Blocker{*blocker}}, nil
	}
	for setting := range capability.inherited {
		if affectedSettingsV2ID(setting) {
			inherited[setting] = struct{}{}
		}
	}

	yards, err := capability.persistentYardNames()
	if err != nil {
		return settingsV2Plan{}, err
	}
	var plan settingsV2Plan
	for _, yard := range yards {
		filePlan, decisions, yardBlocker := capability.inspectYard(yard, inherited, inheritance)
		plan.Decisions = append(plan.Decisions, decisions...)
		if yardBlocker != nil {
			plan.Blockers = append(plan.Blockers, *yardBlocker)
			continue
		}
		if filePlan != nil {
			plan.Files = append(plan.Files, *filePlan)
			if settingsV2PlanBytes(plan.Files) > maximumSettingsV2InspectionBytes {
				return settingsV2Plan{Blockers: []Blocker{{
					Code: CodePreconditionBlocked, Resource: "settings.total",
					Message: "persistent yard settings exceed the bounded migration inspection size",
					Retry:   "reduce the persistent yard settings size, then run yard update",
				}}}, nil
			}
		}
	}
	sort.Slice(plan.Decisions, func(left, right int) bool {
		if plan.Decisions[left].Scope != plan.Decisions[right].Scope {
			return plan.Decisions[left].Scope < plan.Decisions[right].Scope
		}
		return plan.Decisions[left].Resource < plan.Decisions[right].Resource
	})
	return plan, nil
}

func settingsV2PlanBytes(files []settingsV2FilePlan) int {
	total := 0
	for index := range files {
		total += len(files[index].Expected.Content) + len(files[index].Desired)
	}
	return total
}

func (capability *testVMSettingsV2Capability) validateCatalog() *Blocker {
	type expectedSetting struct {
		name   string
		owner  string
		kind   config.SettingKind
		value  config.SettingValueType
		scopes []config.SettingScope
	}
	expected := []expectedSetting{
		{
			name: "YARD_TEMPLATE", owner: "environment-profile",
			kind: config.SettingScalar, value: config.SettingName,
			scopes: []config.SettingScope{config.ScopeYard, config.ScopeCommand},
		},
		{
			name: "NESTED_E2E_VMS", owner: "yard-security",
			kind: config.SettingScalar, value: config.SettingBoolean,
			scopes: []config.SettingScope{
				config.ScopeShipped, config.ScopeHost, config.ScopeYard, config.ScopeCommand,
			},
		},
	}
	for _, contract := range expected {
		definition, exists := capability.lookupSetting(contract.name)
		if !exists || definition.Name != contract.name || definition.Sensitive ||
			definition.Owner != contract.owner || definition.Kind != contract.kind ||
			definition.Type != contract.value || definition.Merge != "replace" ||
			!slices.Equal(definition.Scopes, contract.scopes) {
			return &Blocker{
				Code: CodeUnsupportedKind, Resource: "setting." + contract.name,
				Message: "the compiled settings capability no longer matches the setting catalog",
				Retry:   "run yard update --check",
			}
		}
	}
	return nil
}

func (capability *testVMSettingsV2Capability) observeInheritedSettings() (
	map[string]struct{},
	Fingerprint,
	*Blocker,
	error,
) {
	resources := []settingsV2InheritedSnapshot{
		{Role: "shared", Label: "overrides/shared/config.env"},
		{Role: "host", Label: "config.env"},
	}
	for index := range resources {
		resource := &resources[index]
		path := filepath.Join(capability.configHome, filepath.FromSlash(resource.Label))
		snapshot, err := capability.snapshotView.ReadSnapshot(path)
		if err != nil {
			// Only the native view may inspect root identity. Synthetic/prospective
			// observers must never fall through to the physical config home.
			if _, native := capability.snapshotView.(posixV2SettingsSnapshotView); native {
				if _, rootErr := os.Lstat(capability.configHome); rootErr != nil {
					return nil, "", nil, rootErr
				}
			}
			return nil, "", &Blocker{Code: CodePreconditionBlocked, Resource: "settings.inherited", Message: capability.sourceDiagnostic(path, "", 0, "inherited persistent settings cannot be observed safely", err), Retry: "run yard -Y default config status"}, nil
		}
		resource.Snapshot = snapshot
		// Preserve source-order failure precedence before observing the next
		// resource. Classification stays pure and bounded to two sources.
		_, _, blocker, classifyErr := classifySettingsV2Inheritance(resources[:index+1], nil)
		if classifyErr != nil || blocker != nil {
			return nil, "", blocker, classifyErr
		}
	}
	command := make([]string, 0, len(capability.inherited))
	for setting := range capability.inherited {
		command = append(command, setting)
	}
	return classifySettingsV2Inheritance(resources, command)
}

func (capability *testVMSettingsV2Capability) persistentYardNames() ([]string, error) {
	yards, err := capability.snapshotView.ListYards()
	if err != nil {
		return nil, err
	}
	if len(yards) > maximumSettingsV2Yards {
		return nil, errors.New("too many persistent yards")
	}
	yards = slices.Clone(yards)
	for _, yard := range yards {
		if !domain.SafeName(yard) {
			return nil, errors.New("prospective persistent yard name is unsafe")
		}
	}
	sort.Strings(yards)
	yards = slices.Compact(yards)
	return yards, nil
}

type posixV2SettingsSnapshotView struct{ configHome string }

func (view posixV2SettingsSnapshotView) ReadSnapshot(
	path string,
) (config.PersistentFileSnapshot, error) {
	return config.ReadPersistentFileSnapshot(view.configHome, path)
}

func (view posixV2SettingsSnapshotView) ListYards() ([]string, error) {
	yardsRoot := filepath.Join(view.configHome, "yards")
	directory, err := os.OpenFile(
		yardsRoot,
		os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW,
		0,
	)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !ok ||
		stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("persistent yards directory is unsafe")
	}
	entries, err := directory.ReadDir((maximumSettingsV2Yards * 2) + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maximumSettingsV2Yards*2 {
		return nil, errors.New("too many persistent yard settings entries")
	}
	names := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".env") {
			name = strings.TrimSuffix(name, ".env")
		}
		if domain.SafeName(name) {
			names[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	if len(result) > maximumSettingsV2Yards {
		return nil, errors.New("too many persistent yards")
	}
	return result, nil
}

func (capability *testVMSettingsV2Capability) inspectYard(
	yard string, inherited map[string]struct{}, inheritance Fingerprint,
) (*settingsV2FilePlan, []RedactedDecision, *Blocker) {
	input, errPath, err := observeSettingsV2Yard(capability.configHome, capability.snapshotView, yard)
	if err != nil {
		return nil, nil, capability.sourceBlocker(yard, errPath, "", 0, "persistent yard settings are unsafe", err)
	}
	// The native template loader stays at the observation boundary. The planner
	// receives only its support fact, including failure to validate candidate bytes.
	snapshot, label, selectErr := selectSettingsV2Yard(input)
	if selectErr == nil {
		assignments, parseErr := capability.parseAssignment(label, snapshot.Content)
		if parseErr == nil {
			var templates []config.PersistentAssignment
			for _, assignment := range assignments {
				if assignment.Name == "YARD_TEMPLATE" {
					templates = append(templates, assignment)
				}
			}
			template, present, safe := exactDirectSettingsV2Value(templates)
			if safe && present && template != "" && template != "e2e-vms" && template != "test-vms" {
				input.TemplateSupported = config.IsSupportedYardTemplate(capability.candidateConfigDir, template)
			}
		}
	}
	file, decisions, blocker := planSettingsV2Yard(input, inherited, inheritance)
	if file != nil {
		file.Path = filepath.Join(capability.configHome, filepath.FromSlash(file.Path))
	}
	return file, decisions, blocker
}

func exactDirectSettingsV2Value(
	assignments []config.PersistentAssignment,
) (string, bool, bool) {
	if len(assignments) == 0 {
		return "", false, true
	}
	if len(assignments) != 1 || !assignments[0].Direct || assignments[0].Dynamic {
		return "", true, false
	}
	return assignments[0].Value, true, true
}

func affectedSettingsV2ID(name string) bool {
	return name == "YARD_TEMPLATE" || name == "NESTED_E2E_VMS"
}

func (capability *testVMSettingsV2Capability) sourceDiagnostic(path, key string, line int, reason string, cause error) string {
	if diagnostic, ok := config.SourceDiagnostic(capability.configHome, cause); ok {
		return diagnostic
	}
	diagnostic, _ := config.SourceDiagnostic(capability.configHome, &config.SourceError{Path: path, Key: key, Line: line, Role: "scalar settings", Reason: reason})
	return diagnostic
}

func (capability *testVMSettingsV2Capability) sourceBlocker(yard, path, key string, line int, reason string, cause error) *Blocker {
	return &Blocker{
		Code: CodePreconditionBlocked, Resource: "yard." + yard,
		Message: "yard " + yard + ": " + capability.sourceDiagnostic(path, key, line, reason, cause),
		Retry:   "run yard -Y " + yard + " config status",
	}
}

func settingsV2Decision(
	yard string,
	setting string,
	decision Decision,
	result string,
) RedactedDecision {
	return RedactedDecision{
		Resource: "setting." + setting, Scope: "yard." + yard,
		Decision: decision, Result: result,
	}
}

func settingsV2SnapshotFingerprint(snapshot config.PersistentFileSnapshot) Fingerprint {
	digest := sha256.Sum256(snapshot.Content)
	payload, _ := json.Marshal(struct {
		Exists bool   `json:"exists"`
		SHA256 string `json:"sha256"`
	}{
		Exists: snapshot.Exists,
		SHA256: hex.EncodeToString(digest[:]),
	})
	return fingerprintPayload(payload)
}

func settingsV2ResourceFingerprint(
	snapshot config.PersistentFileSnapshot,
	inheritance Fingerprint,
) Fingerprint {
	payload, _ := json.Marshal(struct {
		File        Fingerprint `json:"file"`
		Inheritance Fingerprint `json:"inheritance"`
	}{settingsV2SnapshotFingerprint(snapshot), inheritance})
	return fingerprintPayload(payload)
}

func (plan settingsV2FilePlan) validate() error {
	if !domain.SafeName(plan.Yard) || !filepath.IsAbs(plan.Path) || (!plan.Expected.Exists && !plan.LocalSSHPort) ||
		len(plan.Desired) == 0 || !validFingerprint(plan.ExpectedFingerprint) ||
		!validFingerprint(plan.DesiredFingerprint) || !validFingerprint(plan.Inheritance) ||
		plan.ExpectedFingerprint == plan.DesiredFingerprint ||
		(plan.Decision != DecisionCanonicalize && plan.Decision != DecisionReset) {
		return fmt.Errorf("invalid typed settings file plan for yard %q", plan.Yard)
	}
	return nil
}

// observeSettingsV2Yard is the single native/prospective layout observation adapter.
func observeSettingsV2Yard(home string, view V2SettingsSnapshotView, yard string) (settingsV2YardSnapshot, string, error) {
	if !domain.SafeName(yard) {
		return settingsV2YardSnapshot{}, "", invalid("journal yard is unsafe")
	}
	if view == nil {
		view = posixV2SettingsSnapshotView{configHome: home}
	}
	nestedLabel, legacyLabel := settingsV2YardLabels(yard)
	nestedPath, legacyPath := filepath.Join(home, filepath.FromSlash(nestedLabel)), filepath.Join(home, filepath.FromSlash(legacyLabel))
	nested, nestedErr := view.ReadSnapshot(nestedPath)
	legacy, legacyErr := view.ReadSnapshot(legacyPath)
	if nestedErr != nil {
		return settingsV2YardSnapshot{}, nestedPath, errors.Join(nestedErr, legacyErr)
	}
	if legacyErr != nil {
		return settingsV2YardSnapshot{}, legacyPath, legacyErr
	}
	return settingsV2YardSnapshot{Yard: yard, Nested: nested, Legacy: legacy}, "", nil
}
