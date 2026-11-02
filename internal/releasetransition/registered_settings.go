package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func (t *V2Transition) registeredSettingsSnapshot(yard string, view V2SettingsSnapshotView) (config.PersistentFileSnapshot, string, Fingerprint, error) {
	if !domain.SafeName(yard) {
		return config.PersistentFileSnapshot{}, "", "", invalid("unsafe registered settings yard")
	}
	if view == nil {
		view = posixV2SettingsSnapshotView{configHome: t.options.ConfigHome}
	}
	path := filepath.Join(t.options.ConfigHome, "yards", yard, "config.env")
	snapshot, err := view.ReadSnapshot(path)
	if err != nil {
		return snapshot, path, "", err
	}
	flatPath := filepath.Join(t.options.ConfigHome, "yards", yard+".env")
	flat, err := view.ReadSnapshot(flatPath)
	if err != nil || snapshot.Exists && flat.Exists {
		return snapshot, path, "", invalid("ambiguous registered local settings")
	}
	if flat.Exists {
		snapshot, path = flat, flatPath
	}
	inputs := map[string]Fingerprint{}
	for _, relative := range []string{"host.env", "config.env", filepath.Join(config.GitSettingsRelativePath, "host.env"), filepath.Join(config.GitSettingsRelativePath, "config.env"), filepath.Join(config.GitSettingsRelativePath, "yards", yard, "config.env")} {
		name := filepath.Join(t.options.ConfigHome, relative)
		input, err := view.ReadSnapshot(name)
		if err != nil {
			return snapshot, path, "", err
		}
		inputs[name] = settingsV2SnapshotFingerprint(input)
	}
	payload, err := json.Marshal(inputs)
	return snapshot, path, fingerprintPayload(payload), err
}

func observeRegisteredOwner(ctx context.Context, owner V2ActivationReconciler, releases ReleasePair, links ReleaseLinks, view V2SettingsSnapshotView) (V2ActivationObservation, error) {
	if prospective, ok := owner.(V2ProspectiveActivationObserver); ok && view != nil {
		return prospective.ObserveProspective(ctx, releases, links, view)
	}
	return owner.Observe(ctx, releases, links)
}

func (t *V2Transition) inspectRegisteredSettings(ctx context.Context, m MigrationDefinitionV2, view V2SettingsSnapshotView, links ReleaseLinks) (settingsV2Plan, error) {
	plan := settingsV2Plan{}
	if m.Kind != registeredYardsMigration {
		return plan, nil
	}
	for _, owner := range t.nativeOwners(m) {
		if _, err := observeRegisteredOwner(ctx, owner, t.options.Releases, links, view); err != nil {
			return plan, err
		}
		paths, ok := owner.(V2PreparedInputPaths)
		if !ok {
			continue
		}
		reader, ok := owner.(V2PreparedInputReader)
		if !ok {
			continue
		}
		names, err := paths.PreparedInputPaths()
		if err != nil {
			return plan, err
		}
		for _, path := range names {
			yard := strings.TrimPrefix(owner.ID(), "yard.")
			before, expectedPath, inheritance, err := t.registeredSettingsSnapshot(yard, view)
			if err != nil {
				return plan, err
			}
			if path != expectedPath {
				return plan, invalid("registered endpoint output is outside its local yard")
			}
			desired, matched, err := reader.ReadPreparedInput(path)
			if err != nil {
				return plan, err
			}
			if !matched || !desired.Exists {
				return plan, invalid("registered endpoint output is missing")
			}
			if bytes.Equal(before.Content, desired.Content) && before.Exists {
				continue
			}
			assignments, err := config.ParsePersistentAssignments(path, desired.Content)
			if err != nil {
				return plan, err
			}
			existing, err := config.ParsePersistentAssignments(path, before.Content)
			if err != nil {
				return plan, err
			}
			present := map[string]bool{}
			for _, a := range existing {
				present[a.Name] = true
			}
			content := slices.Clone(before.Content)
			values := []settingsV2BeforeValue{}
			for _, a := range assignments {
				if present[a.Name] {
					continue
				}
				if !slices.Contains(t.options.RegisteredEndpointSettings, a.Name) || !a.Direct || a.Dynamic {
					return plan, invalid("registered endpoint output names an undeclared setting")
				}
				value := a.Value
				content, err = config.EditPersistentAssignmentContent(path, content, a.Name, &value)
				if err != nil {
					return plan, err
				}
				values = append(values, settingsV2BeforeValue{Setting: a.Name})
			}
			if len(values) == 0 || len(values) > 32 || !bytes.Equal(content, desired.Content) {
				return plan, invalid("registered endpoint output changes unrelated settings")
			}
			file := settingsV2FilePlan{RegisteredEndpoints: true, Yard: yard, Path: path, Expected: before, Desired: content, Inheritance: inheritance, Before: values, Decision: DecisionCanonicalize, ExpectedFingerprint: settingsV2ResourceFingerprint(before, inheritance), DesiredFingerprint: settingsV2ResourceFingerprint(config.PersistentFileSnapshot{Exists: true, Content: content}, inheritance)}
			if err := file.validate(); err != nil {
				return plan, err
			}
			plan.Files = append(plan.Files, file)
		}
	}
	return plan, nil
}

func settingsRecoveryKind(m MigrationDefinitionV2) string {
	if m.Kind == localSSHPortMigration || m.Kind == registeredYardsMigration {
		return m.Kind
	}
	return ""
}
func validateEndpointRecovery(recovery settingsRecoveryV1, allowed []string) error {
	if recovery.Kind != registeredYardsMigration {
		return nil
	}
	for _, before := range recovery.Before {
		if !slices.Contains(allowed, before.Setting) {
			return invalid("registered endpoint recovery names an undeclared setting")
		}
	}
	return nil
}
