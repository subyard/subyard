package testvmsruntime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const agentLogRetention = 30 * 24 * time.Hour
const agentLogMaxBytes = int64(512 << 20)
const maxAgentLogEvents = 200
const maxAgentLogBytes = 1 << 20
const agentLogOwnerMarker = "user.subyard.agent-log-owner"

// The controller's private random client ID is a read capability, never attribution.
var agentLogClientID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (recorder EventRecorder) archiveRoot() string {
	return filepath.Join(recorder.StateDir, "agent-logs")
}

func (recorder EventRecorder) agentLogDirectory(clientID, kind string) string {
	return filepath.Join(recorder.archiveRoot(), capabilityDigest(clientID), kind)
}

// Ownership is stamped from the captured private slot at record creation, never
// reconstructed from counters: resize and corrupt-state recovery can reuse them.
func (recorder EventRecorder) archiveAgentRecord(clientID, kind, id string, payload []byte) error {
	if !agentLogClientID.MatchString(clientID) {
		return nil // Legacy callers do not supply a private random client ID.
	}
	return withFileLock(filepath.Join(recorder.archiveRoot(), ".lock"), func() error {
		return writeImmutableDurable(filepath.Join(recorder.agentLogDirectory(clientID, kind), id+".json"), payload, 0o600)
	})
}

func readAgentLogRecord(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maximum {
		return nil, errors.New("unsafe agent log record")
	}
	return os.ReadFile(path)
}

func (recorder EventRecorder) readAgentEvents(clientID, slot string, count int) ([]BrokerEvent, error) {
	if !agentLogClientID.MatchString(clientID) || count < 1 || count > maxAgentLogEvents ||
		(slot != "-" && !brokerSlotID.MatchString(slot)) {
		return nil, errors.New("invalid agent log selection")
	}
	events := make([]BrokerEvent, 0)
	directory := recorder.agentLogDirectory(clientID, "events")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return events, nil
	}
	if err != nil {
		return nil, err
	}
	// IDs have random suffixes; sequence breaks ties in timestamps. Scan all
	// records, retaining only the bounded tail of this controller's own archive.
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() != id+".json" || !brokerRecordID.MatchString(id) {
			continue
		}
		payload, err := readAgentLogRecord(filepath.Join(directory, entry.Name()), maxBrokerEventBytes)
		if os.IsNotExist(err) {
			continue // Concurrent acknowledgement can prune expired records.
		}
		if err != nil {
			return nil, err
		}
		var event BrokerEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, err
		}
		if err := validateBrokerEvent(event); err != nil {
			return nil, err
		}
		if event.EventID != id || (slot != "-" && slot != event.SlotID) ||
			recorder.now().Sub(event.Timestamp) >= agentLogRetention {
			continue
		}
		event.Error = strings.ReplaceAll(event.Error, clientID, "[redacted]")
		events = append(events, event)
		sortBrokerEvents(events)
		if len(events) > count {
			events = events[1:]
		}
	}
	for total, index := 2, len(events)-1; index >= 0; index-- {
		payload, _ := json.Marshal(events[index])
		total += len(payload) + 1
		if total > maxAgentLogBytes {
			events = events[index+1:]
			break
		}
	}
	return events, nil
}

func (recorder EventRecorder) readAgentIncident(clientID, id string) (*IncidentArtifact, error) {
	if !agentLogClientID.MatchString(clientID) || !brokerRecordID.MatchString(id) {
		return nil, errors.New("invalid agent incident selection")
	}
	payload, err := readAgentLogRecord(filepath.Join(recorder.agentLogDirectory(clientID, "incidents"), id+".json"), maxIncidentBytes)
	if err != nil {
		return nil, err
	}
	var incident IncidentArtifact
	if err := json.Unmarshal(payload, &incident); err != nil {
		return nil, err
	}
	if err := validateIncidentArtifact(incident); err != nil {
		return nil, err
	}
	if incident.IncidentID != id || recorder.now().Sub(incident.CreatedAt) >= agentLogRetention {
		return nil, errors.New("incident is unavailable for this controller")
	}
	return &incident, nil
}

func agentIncident(artifact IncidentArtifact, clientID string) IncidentArtifact {
	artifact.FailureReason = strings.ReplaceAll(artifact.FailureReason, clientID, "[redacted]")
	if artifact.Command != nil {
		command := *artifact.Command
		command.Command = strings.ReplaceAll(command.Command, clientID, "[redacted]")
		command.Error = strings.ReplaceAll(command.Error, clientID, "[redacted]")
		artifact.Command = &command
	}
	diagnostics := make(map[string]string)
	for name, value := range artifact.Diagnostics {
		switch name {
		case "capacity", "force_stop_vm", "force_stop_state", "original_incident_id":
			diagnostics[name] = strings.ReplaceAll(value, clientID, "[redacted]")
		case "vm_1_state", "vm_2_state", "vm_1_info_log", "vm_2_info_log",
			"vm_1_console_log", "vm_2_console_log", "vm_1_qmp_log", "vm_2_qmp_log":
			if artifact.Diagnostics[name[:4]+"_allocation_verified"] == "true" {
				diagnostics[name] = strings.ReplaceAll(value, clientID, "[redacted]")
			}
		}
	}
	artifact.Diagnostics = diagnostics
	return artifact
}

// Called under the archive lock by acknowledgement; read operations never prune.
func (recorder EventRecorder) pruneAgentLogs() error {
	type record struct {
		path string
		at   time.Time
		size int64
	}
	var records []record
	var directories []string
	var total int64
	err := filepath.WalkDir(recorder.archiveRoot(), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != recorder.archiveRoot() {
				directories = append(directories, path)
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("unsafe agent log archive")
		}
		records = append(records, record{path, info.ModTime(), info.Size()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].at.Before(records[j].at) })
	for _, record := range records {
		if recorder.now().Sub(record.at) < agentLogRetention && total <= agentLogMaxBytes {
			continue
		}
		if err := os.Remove(record.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		total -= record.size
	}
	for index := len(directories) - 1; index >= 0; index-- {
		entries, err := os.ReadDir(directories[index])
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(directories[index]); err != nil {
				return err
			}
		}
	}
	return nil
}
