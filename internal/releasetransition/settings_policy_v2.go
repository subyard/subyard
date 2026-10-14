package releasetransition

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// settingsV2YardSnapshot carries logical layout and already observed template support.
// Its snapshots contain bytes, not filesystem capabilities. Output paths are labels.
type settingsV2YardSnapshot struct {
	Yard              string
	Nested, Legacy    config.PersistentFileSnapshot
	TemplateSupported bool
}

func planSettingsV2Yard(input settingsV2YardSnapshot, inherited map[string]struct{}, inheritance Fingerprint) (*settingsV2FilePlan, []RedactedDecision, *Blocker) {
	yard := input.Yard
	if !domain.SafeName(yard) {
		return nil, nil, &Blocker{Code: CodePreconditionBlocked, Resource: "settings.yard", Message: "persistent yard name is unsafe", Retry: "run yard update --check"}
	}
	nestedPath, legacyPath := settingsV2YardLabels(yard)
	nested, legacy := input.Nested, input.Legacy
	if nested.Exists && legacy.Exists {
		return nil, nil, &Blocker{
			Code: CodePreconditionBlocked, Resource: "yard." + yard,
			Message: fmt.Sprintf("yard %s has duplicate registrations: yards/%s/config.env and yards/%s.env", yard, yard, yard),
			Retry:   fmt.Sprintf("run the verified candidate's yard -Y default config repair-registration %s, then run yard update --check", yard),
		}
	}
	if !nested.Exists && !legacy.Exists {
		return nil, nil, nil
	}
	path, snapshot := nestedPath, nested
	if legacy.Exists {
		path, snapshot = legacyPath, legacy
	}
	assignments, err := config.ParsePersistentAssignments(path, snapshot.Content)
	if err != nil {
		return nil, nil, settingsV2LogicalSourceBlocker(yard, path, "", 0, "persistent yard settings cannot be parsed safely", err)
	}
	byName := map[string][]config.PersistentAssignment{}
	for _, assignment := range assignments {
		if affectedSettingsV2ID(assignment.Name) {
			byName[assignment.Name] = append(byName[assignment.Name], assignment)
		}
	}
	blocker := func(key, reason string) *Blocker {
		line := 0
		if values := byName[key]; len(values) != 0 {
			line = values[0].Line
		}
		return settingsV2LogicalSourceBlocker(yard, path, key, line, reason, nil)
	}
	template, templatePresent, safe := exactDirectSettingsV2Value(byName["YARD_TEMPLATE"])
	if !safe {
		return nil, []RedactedDecision{settingsV2Decision(yard, "YARD_TEMPLATE", DecisionBlock, "blocked")},
			blocker("YARD_TEMPLATE", "YARD_TEMPLATE is ambiguous")
	}
	if !templatePresent {
		if _, exists := inherited["YARD_TEMPLATE"]; exists {
			return nil, nil, blocker("YARD_TEMPLATE", "YARD_TEMPLATE has inherited ownership")
		}
		return nil, nil, nil
	}
	if template == "" {
		return nil, []RedactedDecision{settingsV2Decision(yard, "YARD_TEMPLATE", DecisionPreserve, "preserved")}, nil
	}
	if template != "e2e-vms" && template != "test-vms" {
		if input.TemplateSupported {
			return nil, []RedactedDecision{settingsV2Decision(yard, "YARD_TEMPLATE", DecisionPreserve, "preserved")}, nil
		}
		return nil, []RedactedDecision{settingsV2Decision(yard, "YARD_TEMPLATE", DecisionBlock, "blocked")},
			blocker("YARD_TEMPLATE", "YARD_TEMPLATE is not supported by this migration")
	}
	if _, exists := inherited["YARD_TEMPLATE"]; exists {
		return nil, nil, blocker("YARD_TEMPLATE", "YARD_TEMPLATE has inherited ownership")
	}
	nestedValue, nestedPresent, safe := exactDirectSettingsV2Value(byName["NESTED_E2E_VMS"])
	if !safe {
		return nil, []RedactedDecision{settingsV2Decision(yard, "NESTED_E2E_VMS", DecisionBlock, "blocked")},
			blocker("NESTED_E2E_VMS", "NESTED_E2E_VMS is ambiguous")
	}
	if _, exists := inherited["NESTED_E2E_VMS"]; exists {
		return nil, nil, blocker("NESTED_E2E_VMS", "NESTED_E2E_VMS has inherited ownership")
	}
	if nestedPresent && nestedValue != "0" && nestedValue != "1" {
		return nil, []RedactedDecision{settingsV2Decision(yard, "NESTED_E2E_VMS", DecisionBlock, "blocked")},
			blocker("NESTED_E2E_VMS", "NESTED_E2E_VMS is not supported by this migration")
	}

	decisions := make([]RedactedDecision, 0, 2)
	desired := slices.Clone(snapshot.Content)
	changed := false
	fileDecision := DecisionCanonicalize
	if template == "e2e-vms" {
		value := "test-vms"
		desired, err = config.EditPersistentAssignmentContent(path, desired, "YARD_TEMPLATE", &value)
		if err != nil {
			return nil, nil, blocker("YARD_TEMPLATE", "YARD_TEMPLATE cannot be edited safely")
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
			return nil, nil, blocker("NESTED_E2E_VMS", "NESTED_E2E_VMS cannot be reset safely")
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
	snapshot.Content = slices.Clone(snapshot.Content)
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

type settingsV2InheritedSnapshot struct {
	Role, Label string
	Snapshot    config.PersistentFileSnapshot
}

func classifySettingsV2Inheritance(resources []settingsV2InheritedSnapshot, commandIDs []string) (map[string]struct{}, Fingerprint, *Blocker, error) {
	result := make(map[string]struct{})
	type inheritedResource struct {
		Role     string   `json:"role"`
		Settings []string `json:"settings"`
	}
	observed := make([]inheritedResource, 0, len(resources))
	for _, resource := range resources {
		snapshot := resource.Snapshot
		entry := inheritedResource{Role: resource.Role, Settings: []string{}}
		if !snapshot.Exists {
			observed = append(observed, entry)
			continue
		}
		assignments, err := config.ParsePersistentAssignments(resource.Label, snapshot.Content)
		if err != nil {
			return nil, "", &Blocker{
				Code: CodePreconditionBlocked, Resource: "settings.inherited",
				Message: settingsV2LogicalSourceDiagnostic(resource.Label, "", 0, "inherited persistent settings cannot be classified safely", err),
				Retry:   "run yard -Y default config status",
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
	command := make([]string, 0, len(commandIDs))
	for _, setting := range commandIDs {
		if affectedSettingsV2ID(setting) {
			if !slices.Contains(command, setting) {
				command = append(command, setting)
			}
			result[setting] = struct{}{}
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

func settingsV2YardLabels(yard string) (string, string) {
	return "yards/" + yard + "/config.env", "yards/" + yard + ".env"
}

func selectSettingsV2Yard(input settingsV2YardSnapshot) (config.PersistentFileSnapshot, string, error) {
	if input.Nested.Exists == input.Legacy.Exists {
		return config.PersistentFileSnapshot{}, "", errors.New("journaled yard settings ownership is ambiguous")
	}
	nested, legacy := settingsV2YardLabels(input.Yard)
	if input.Nested.Exists {
		return input.Nested, nested, nil
	}
	return input.Legacy, legacy, nil
}

func settingsV2AwaitSourceImport(expected config.PersistentFileSnapshot, nestedLayout bool, observed settingsV2YardSnapshot) bool {
	return expected.Exists && nestedLayout && !observed.Nested.Exists && !observed.Legacy.Exists
}

func settingsV2LogicalSourceDiagnostic(label, key string, line int, reason string, cause error) string {
	if diagnostic, ok := config.SourceDiagnostic(".", cause); ok {
		return diagnostic
	}
	diagnostic, _ := config.SourceDiagnostic(".", &config.SourceError{Path: label, Key: key, Line: line, Role: "scalar settings", Reason: reason})
	return diagnostic
}
func settingsV2LogicalSourceBlocker(yard, label, key string, line int, reason string, cause error) *Blocker {
	return &Blocker{Code: CodePreconditionBlocked, Resource: "yard." + yard, Message: "yard " + yard + ": " + settingsV2LogicalSourceDiagnostic(label, key, line, reason, cause), Retry: "run yard -Y " + yard + " config status"}
}
