package releasetransition

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const localSSHPortMigration = "local-ssh-port-settings-v2-to-v3"

// Pin the old persistent effective value, leaving Git source/cache ownership to
// explicit config sync. No command override participates in this migration.
func (transition *V2Transition) inspectLocalSSHPorts(view V2SettingsSnapshotView) (settingsV2Plan, error) {
	if view == nil {
		view = posixV2SettingsSnapshotView{configHome: transition.options.ConfigHome}
	}
	names, err := config.YardNames(transition.options.CandidateConfigDir, transition.options.ConfigHome)
	if err != nil {
		return settingsV2Plan{}, err
	}
	projected, err := view.ListYards()
	if err != nil {
		return settingsV2Plan{}, err
	}
	names = append(names, projected...)
	slices.Sort(names)
	names = slices.Compact(names)
	legacy := false
	paths := []string{filepath.Join(transition.options.ConfigHome, config.GitSettingsRelativePath, "config.env")}
	for _, yard := range names {
		paths = append(paths, filepath.Join(transition.options.ConfigHome, config.GitSettingsRelativePath, "yards", yard, "config.env"))
	}
	for _, path := range paths {
		snapshot, err := view.ReadSnapshot(path)
		if err != nil {
			return settingsV2Plan{}, err
		}
		assignments, err := config.ParsePersistentAssignments(path, snapshot.Content)
		if err != nil {
			return settingsV2Plan{}, err
		}
		for _, assignment := range assignments {
			if assignment.Name == "SSH_PORT" {
				legacy = true
			}
		}
	}
	if !legacy {
		return settingsV2Plan{}, nil
	}
	if len(names) > maximumSettingsV2Yards {
		return settingsV2Plan{}, errors.New("too many persistent SSH port migration targets")
	}
	files := &config.ProspectiveFiles{Root: transition.options.ConfigHome, Yards: names, ReadSnapshot: view.ReadSnapshot}
	plan := settingsV2Plan{}
	ports := map[int]string{}
	for _, yard := range names {
		if !domain.SafeName(yard) {
			return plan, errors.New("unsafe SSH port migration target")
		}
		loaded, err := transition.loadSSHPortYard(yard, files)
		if err != nil {
			return plan, err
		}
		if loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		if previous, exists := ports[loaded.Context.SSHPort]; exists && loaded.Context.SSHPort != 0 {
			return plan, fmt.Errorf("SSH port is used by both yard %s and yard %s", previous, yard)
		}
		ports[loaded.Context.SSHPort] = yard
		_, _, group := config.ComponentSelectionSource(loaded.Settings["SSH_PORT"])
		if group != "Git settings" {
			continue
		}
		expected, path, inheritance, err := transition.localSSHPortSnapshot(yard, view, loaded)
		if err != nil {
			return plan, err
		}
		value := strconv.Itoa(loaded.Context.SSHPort)
		content, err := config.EditPersistentAssignmentContent(path, expected.Content, "SSH_PORT", &value)
		if err != nil {
			return plan, err
		}
		file := settingsV2FilePlan{Yard: yard, Path: path, Expected: expected, Desired: content, Inheritance: inheritance,
			ExpectedFingerprint: settingsV2ResourceFingerprint(expected, inheritance),
			DesiredFingerprint:  settingsV2ResourceFingerprint(config.PersistentFileSnapshot{Exists: true, Content: content}, inheritance),
			Decision:            DecisionCanonicalize, LocalSSHPort: true, Before: []settingsV2BeforeValue{{Setting: "SSH_PORT", Present: false}}}
		plan.Files = append(plan.Files, file)
		plan.Decisions = append(plan.Decisions, RedactedDecision{Resource: "yard." + yard, Scope: "yard", Decision: DecisionCanonicalize,
			Result: "preserve existing effective SSH port in local yard settings; source and cache remain protected"})
	}
	return plan, nil
}

func (transition *V2Transition) loadSSHPortYard(yard string, files *config.ProspectiveFiles) (config.Loaded, error) {
	operator := transition.options.OperatorHome
	if operator == "" {
		operator = transition.options.ConfigHome
	}
	return config.Load(config.LoadOptions{RepositoryRoot: filepath.Dir(transition.options.CandidateConfigDir), OperatorHome: operator,
		YardName: yard, DisablePrivate: true, ProspectiveFiles: files,
		Environment: map[string]string{"SUBYARD_CONFIG_HOME": transition.options.ConfigHome, "SUBYARD_CONFIG_DIR": transition.options.CandidateConfigDir}})
}

func (transition *V2Transition) localSSHPortSnapshot(yard string, view V2SettingsSnapshotView, loaded config.Loaded) (config.PersistentFileSnapshot, string, Fingerprint, error) {
	path := filepath.Join(transition.options.ConfigHome, "yards", yard, "config.env")
	expected, err := view.ReadSnapshot(path)
	if err != nil {
		return expected, path, "", err
	}
	legacy := filepath.Join(transition.options.ConfigHome, "yards", yard+".env")
	flat, err := view.ReadSnapshot(legacy)
	if err != nil || expected.Exists && flat.Exists {
		return expected, path, "", errors.New("SSH port migration requires unambiguous local yard settings")
	}
	if flat.Exists {
		expected, path = flat, legacy
	}
	inputs := map[string]Fingerprint{}
	for _, layer := range loaded.ConfigurationLayers {
		if !strings.HasPrefix(layer.Path, transition.options.ConfigHome+string(filepath.Separator)) ||
			strings.HasPrefix(layer.Path, filepath.Join(transition.options.ConfigHome, "yards")+string(filepath.Separator)) ||
			!strings.Contains(layer.Role, "scalar settings") {
			continue
		}
		snapshot, err := view.ReadSnapshot(layer.Path)
		if err != nil {
			return expected, path, "", err
		}
		inputs[layer.Path] = settingsV2SnapshotFingerprint(snapshot)
	}
	payload, err := json.Marshal(inputs)
	return expected, path, fingerprintPayload(payload), err
}

func (transition *V2Transition) observeSettingsMigrationWork(migration MigrationDefinitionV2, yard string) (config.PersistentFileSnapshot, string, Fingerprint, error) {
	if migration.Kind != localSSHPortMigration {
		return transition.observeSettingsWork(yard)
	}
	view := posixV2SettingsSnapshotView{configHome: transition.options.ConfigHome}
	loaded, err := transition.loadSSHPortYard(yard, nil)
	if err != nil {
		return config.PersistentFileSnapshot{}, "", "", err
	}
	snapshot, path, inheritance, err := transition.localSSHPortSnapshot(yard, view, loaded)
	return snapshot, path, settingsV2ResourceFingerprint(snapshot, inheritance), err
}

// A later settings migration may publish to the same file. Verify the native
// successor against its authorization before viewing the verified predecessor;
// never remove a setting merely because it belongs to another migration.
func (transition *V2Transition) observeJournalSettingsWork(journal JournalRecord, step JournalStep, migration MigrationDefinitionV2) (config.PersistentFileSnapshot, string, Fingerprint, error) {
	snapshot, path, actual, err := transition.observeSettingsMigrationWork(migration, step.Resource)
	if err != nil || actual == step.Expected || actual == step.Desired ||
		migration.Kind != "test-vms-settings-v1-to-v2" || step.Checkpoint != StepVerified {
		return snapshot, path, actual, err
	}
	index := slices.IndexFunc(journal.Steps, func(candidate JournalStep) bool { return candidate.ID == step.ID })
	if index < 0 {
		return snapshot, path, actual, invalid("settings predecessor is not authorized")
	}
	for _, successor := range journal.Steps[index+1:] {
		definition, known := transition.migration(successor.Migration)
		if !known || definition.Kind != localSSHPortMigration || successor.Resource != step.Resource {
			continue
		}
		if successor.ID != definition.ID+"."+step.Resource || successor.Decision != DecisionCanonicalize ||
			successor.Checkpoint == StepIntent || successor.Evidence == nil {
			return snapshot, path, actual, invalid("settings successor has no authorized mutation evidence")
		}
		recoverySnapshot, err := transition.store.ReadRecovery(journal.Transaction, successor.ID)
		if err != nil || !recoverySnapshot.Exists {
			return snapshot, path, actual, invalid("settings successor recovery is unavailable")
		}
		var recovery settingsRecoveryV1
		if err := decodeBoundedRecord(recoverySnapshot.Payload, 4096, &recovery); err != nil {
			return snapshot, path, actual, err
		}
		if err := validateSettingsRecovery(recovery, journal, successor); err != nil {
			return snapshot, path, actual, err
		}
		if recovery.Kind != localSSHPortMigration || recovery.Before[0].Present || successor.Evidence.Recovery != recoverySnapshot.Fingerprint {
			return snapshot, path, actual, invalid("settings successor recovery does not bind an absent local SSH port")
		}
		captured, err := transition.store.ReadCheckpointEvidence(journal.Transaction, successor.ID, EvidenceCaptured)
		if err != nil || !captured.Exists {
			return snapshot, path, actual, invalid("settings successor captured evidence is unavailable")
		}
		evidence, err := ParseEvidence(captured.Payload)
		if err != nil || !evidenceRecordsEqual(evidence, evidenceFor(journal, successor, EvidenceCaptured, successor.Expected, recoverySnapshot.Fingerprint)) {
			return snapshot, path, actual, invalid("settings successor captured evidence differs from its authorization")
		}
		view := posixV2SettingsSnapshotView{configHome: transition.options.ConfigHome}
		loaded, err := transition.loadSSHPortYard(step.Resource, nil)
		if err != nil {
			return snapshot, path, actual, err
		}
		native, successorPath, inheritance, err := transition.localSSHPortSnapshot(step.Resource, view, loaded)
		if err != nil || successorPath != path || settingsV2ResourceFingerprint(native, inheritance) != successor.Desired {
			return snapshot, path, actual, invalid("settings successor differs from its authorized desired state")
		}
		content, err := config.EditPersistentAssignmentContent(path, native.Content, "SSH_PORT", nil)
		if err != nil {
			return snapshot, path, actual, err
		}
		predecessor := config.PersistentFileSnapshot{Exists: true, Content: content}
		// Appending the pin inserts one separator LF after an unterminated
		// record. Its inverse is accepted only by the exact authorized input.
		if settingsV2ResourceFingerprint(predecessor, inheritance) != successor.Expected && len(content) != 0 && content[len(content)-1] == '\n' {
			predecessor.Content = content[:len(content)-1]
		}
		if settingsV2ResourceFingerprint(predecessor, inheritance) != successor.Expected {
			return snapshot, path, actual, invalid("settings successor does not preserve its authorized input")
		}
		_, inheritance, blocker, err := newTestVMSettingsV2Capability(transition.options.ConfigHome, transition.options.InheritedSettingIDs).observeInheritedSettings()
		if err != nil || blocker != nil || settingsV2ResourceFingerprint(predecessor, inheritance) != step.Desired {
			return snapshot, path, actual, invalid("settings predecessor differs from its authorized desired state")
		}
		return predecessor, path, step.Desired, nil
	}
	return snapshot, path, actual, nil
}
