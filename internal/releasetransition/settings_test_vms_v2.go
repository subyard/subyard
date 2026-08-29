package releasetransition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	configHome      string
	inherited       map[string]struct{}
	lookupSetting   settingsV2SettingLookup
	snapshotView    V2SettingsSnapshotView
	parseAssignment func(string, []byte) ([]config.PersistentAssignment, error)
}

type settingsV2Plan struct {
	Files     []settingsV2FilePlan
	Decisions []RedactedDecision
	Blockers  []Blocker
}

type settingsV2FilePlan struct {
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

func (view *settingsV2OverlayView) ReadSnapshot(
	path string,
) (config.PersistentFileSnapshot, error) {
	if snapshot, exists := view.snapshots[path]; exists {
		snapshot.Content = slices.Clone(snapshot.Content)
		return snapshot, nil
	}
	return view.base.ReadSnapshot(path)
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
	if capability == nil || capability.configHome == "" ||
		!filepath.IsAbs(capability.configHome) {
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
	result := make(map[string]struct{})
	type inheritedResource struct {
		Role     string   `json:"role"`
		Settings []string `json:"settings"`
	}
	resources := []struct {
		role string
		path string
	}{
		{"shared", filepath.Join(capability.configHome, "overrides", "shared", "config.env")},
		{"host", filepath.Join(capability.configHome, "config.env")},
	}
	observed := make([]inheritedResource, 0, len(resources))
	for _, resource := range resources {
		snapshot, err := capability.snapshotView.ReadSnapshot(resource.path)
		if err != nil {
			if _, rootErr := os.Lstat(capability.configHome); rootErr != nil {
				return nil, "", nil, rootErr
			}
			return nil, "", &Blocker{
				Code: CodePreconditionBlocked, Resource: "settings.inherited",
				Message: "inherited persistent settings cannot be observed safely",
				Retry:   "repair the inherited settings file, then run yard update",
			}, nil
		}
		entry := inheritedResource{Role: resource.role, Settings: []string{}}
		if !snapshot.Exists {
			observed = append(observed, entry)
			continue
		}
		assignments, err := capability.parseAssignment(resource.path, snapshot.Content)
		if err != nil {
			return nil, "", &Blocker{
				Code: CodePreconditionBlocked, Resource: "settings.inherited",
				Message: "inherited persistent settings cannot be classified safely",
				Retry:   "repair the inherited settings file, then run yard update",
			}, nil
		}
		for _, assignment := range assignments {
			if affectedSettingsV2ID(assignment.Name) {
				result[assignment.Name] = struct{}{}
				entry.Settings = append(entry.Settings, assignment.Name)
			}
		}
		sort.Strings(entry.Settings)
		entry.Settings = slices.Compact(entry.Settings)
		observed = append(observed, entry)
	}
	command := make([]string, 0, len(capability.inherited))
	for setting := range capability.inherited {
		if affectedSettingsV2ID(setting) {
			command = append(command, setting)
		}
	}
	sort.Strings(command)
	payload, err := json.Marshal(struct {
		SchemaVersion int                 `json:"schemaVersion"`
		Resources     []inheritedResource `json:"resources"`
		Command       []string            `json:"commandSettingIds"`
	}{1, observed, command})
	if err != nil {
		return nil, "", nil, err
	}
	return result, fingerprintPayload(payload), nil, nil
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
	if err != nil {
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
	yard string,
	inherited map[string]struct{},
	inheritance Fingerprint,
) (*settingsV2FilePlan, []RedactedDecision, *Blocker) {
	nestedPath := filepath.Join(capability.configHome, "yards", yard, "config.env")
	legacyPath := filepath.Join(capability.configHome, "yards", yard+".env")
	nested, nestedErr := capability.snapshotView.ReadSnapshot(nestedPath)
	legacy, legacyErr := capability.snapshotView.ReadSnapshot(legacyPath)
	if nestedErr != nil || legacyErr != nil {
		return nil, nil, settingsV2YardBlocker(yard, "persistent yard settings are unsafe")
	}
	if nested.Exists && legacy.Exists {
		return nil, nil, settingsV2YardBlocker(yard, "persistent yard setting ownership is ambiguous")
	}
	if !nested.Exists && !legacy.Exists {
		return nil, nil, nil
	}
	path, snapshot := nestedPath, nested
	if legacy.Exists {
		path, snapshot = legacyPath, legacy
	}
	assignments, err := capability.parseAssignment(path, snapshot.Content)
	if err != nil {
		return nil, nil, settingsV2YardBlocker(yard, "persistent yard settings cannot be parsed safely")
	}
	byName := map[string][]config.PersistentAssignment{}
	for _, assignment := range assignments {
		if affectedSettingsV2ID(assignment.Name) {
			byName[assignment.Name] = append(byName[assignment.Name], assignment)
		}
	}
	template, templatePresent, safe := exactDirectSettingsV2Value(byName["YARD_TEMPLATE"])
	if !safe {
		return nil, []RedactedDecision{settingsV2Decision(yard, "YARD_TEMPLATE", DecisionBlock, "blocked")},
			settingsV2YardBlocker(yard, "the yard template setting is ambiguous")
	}
	if _, exists := inherited["YARD_TEMPLATE"]; exists {
		return nil, nil, settingsV2YardBlocker(yard, "the yard template has inherited ownership")
	}
	if !templatePresent {
		return nil, nil, nil
	}
	if template != "e2e-vms" && template != "test-vms" {
		return nil, []RedactedDecision{settingsV2Decision(yard, "YARD_TEMPLATE", DecisionBlock, "blocked")},
			settingsV2YardBlocker(yard, "the yard template is not supported by this migration")
	}
	nestedValue, nestedPresent, safe := exactDirectSettingsV2Value(byName["NESTED_E2E_VMS"])
	if !safe {
		return nil, []RedactedDecision{settingsV2Decision(yard, "NESTED_E2E_VMS", DecisionBlock, "blocked")},
			settingsV2YardBlocker(yard, "the nested VM setting is ambiguous")
	}
	if _, exists := inherited["NESTED_E2E_VMS"]; exists {
		return nil, nil, settingsV2YardBlocker(yard, "the nested VM setting has inherited ownership")
	}
	if nestedPresent && nestedValue != "0" && nestedValue != "1" {
		return nil, []RedactedDecision{settingsV2Decision(yard, "NESTED_E2E_VMS", DecisionBlock, "blocked")},
			settingsV2YardBlocker(yard, "the nested VM setting is not supported by this migration")
	}

	decisions := make([]RedactedDecision, 0, 2)
	desired := slices.Clone(snapshot.Content)
	changed := false
	fileDecision := DecisionCanonicalize
	if template == "e2e-vms" {
		value := "test-vms"
		desired, err = config.EditPersistentAssignmentContent(path, desired, "YARD_TEMPLATE", &value)
		if err != nil {
			return nil, nil, settingsV2YardBlocker(yard, "the yard template cannot be edited safely")
		}
		changed = true
		decisions = append(decisions, settingsV2Decision(
			yard, "YARD_TEMPLATE", DecisionCanonicalize, "test-vms",
		))
	} else {
		decisions = append(decisions, settingsV2Decision(
			yard, "YARD_TEMPLATE", DecisionPreserve, "preserved",
		))
	}
	if nestedPresent && nestedValue == "0" {
		desired, err = config.EditPersistentAssignmentContent(path, desired, "NESTED_E2E_VMS", nil)
		if err != nil {
			return nil, nil, settingsV2YardBlocker(yard, "the nested VM setting cannot be reset safely")
		}
		changed = true
		fileDecision = DecisionReset
		decisions = append(decisions, settingsV2Decision(
			yard, "NESTED_E2E_VMS", DecisionReset, "unset",
		))
	} else {
		decisions = append(decisions, settingsV2Decision(
			yard, "NESTED_E2E_VMS", DecisionPreserve, "preserved",
		))
	}
	if !changed {
		return nil, decisions, nil
	}
	desiredSnapshot := config.PersistentFileSnapshot{Exists: true, Content: desired}
	return &settingsV2FilePlan{
		Yard: yard, Path: path, Expected: snapshot, Desired: desired,
		Inheritance:         inheritance,
		ExpectedFingerprint: settingsV2ResourceFingerprint(snapshot, inheritance),
		DesiredFingerprint:  settingsV2ResourceFingerprint(desiredSnapshot, inheritance),
		Decision:            fileDecision,
		Before: []settingsV2BeforeValue{
			{Setting: "YARD_TEMPLATE", Present: templatePresent, Value: template},
			{Setting: "NESTED_E2E_VMS", Present: nestedPresent, Value: nestedValue},
		},
	}, decisions, nil
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

func settingsV2YardBlocker(yard, message string) *Blocker {
	return &Blocker{
		Code: CodePreconditionBlocked, Resource: "yard." + yard,
		Message: message, Retry: "repair the named yard settings, then run yard update",
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
	if !domain.SafeName(plan.Yard) || !filepath.IsAbs(plan.Path) || !plan.Expected.Exists ||
		len(plan.Desired) == 0 || !validFingerprint(plan.ExpectedFingerprint) ||
		!validFingerprint(plan.DesiredFingerprint) || !validFingerprint(plan.Inheritance) ||
		plan.ExpectedFingerprint == plan.DesiredFingerprint ||
		(plan.Decision != DecisionCanonicalize && plan.Decision != DecisionReset) {
		return fmt.Errorf("invalid typed settings file plan for yard %q", plan.Yard)
	}
	return nil
}
