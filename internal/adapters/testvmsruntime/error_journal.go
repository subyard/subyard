package testvmsruntime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The pool error journal is the broker's failures as every controller may see
// them: typed fields only. Lease attribution, error text and guest output stay
// in the owning controller's archive and in the operator's host log.
const (
	JournalSchemaVersion   = 1
	ErrorJournalCapability = "error-journal-v1"
	maxJournalRecordBytes  = 2 << 10
	maxJournalReadEvents   = 200
	maxJournalReadBytes    = 256 << 10
	maxJournalRecords      = 10000
	// A successful stop this slow is recorded as a slow release.
	slowStopDuration = 30 * time.Second
)

type JournalRecord struct {
	SchemaVersion      int       `json:"schema_version"`
	EventID            string    `json:"event_id"`
	Timestamp          time.Time `json:"timestamp"`
	Sequence           uint64    `json:"sequence"`
	Kind               string    `json:"kind"`
	SlotID             string    `json:"slot_id,omitempty"`
	ResourceGeneration uint64    `json:"resource_generation,omitempty"`
	LeaseEpoch         uint64    `json:"lease_epoch,omitempty"`
	FromState          SlotState `json:"from_state,omitempty"`
	ToState            SlotState `json:"to_state,omitempty"`
	Step               string    `json:"step,omitempty"`
	Outcome            string    `json:"outcome"`
	Operation          string    `json:"operation,omitempty"`
	DurationMS         int64     `json:"duration_ms,omitempty"`
	IncidentID         string    `json:"incident_id,omitempty"`
	RecoveryAttempt    uint64    `json:"recovery_attempt,omitempty"`
	// Own is computed for the reader and never stored.
	Own bool `json:"own,omitempty"`
}

// Closed vocabularies of the typed event fields.
var (
	journalSteps      = []string{"fence-and-stop", "delete-allocation", "readiness", "recovery"}
	journalOutcomes   = []string{"error", "timeout", "slow"}
	journalOperations = []string{
		"start", "stop", "delete", "restart", "list", "query", "info", "config",
		"project", "network", "profile", "operation", "console", "storage",
	}
)

func closedValue(value string, allowed []string) bool {
	if value == "" {
		return true
	}
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// journalRecordFor selects the events that report a broker failure or a slow
// operation and projects them onto the public typed fields.
func journalRecordFor(event BrokerEvent) (JournalRecord, bool) {
	outcome := event.Outcome
	switch event.Kind {
	case "lease.stop_failed", "slot.quarantined", "recovery.failed", "vm.force_stop_planned",
		"vm.force_stop_failed", "reaper.fatal", "incus.slow":
		if outcome == "" {
			outcome = "error"
		}
	case "lease.stop_succeeded":
		if event.DurationMS < slowStopDuration.Milliseconds() {
			return JournalRecord{}, false
		}
		outcome = "slow"
	default:
		return JournalRecord{}, false
	}
	return JournalRecord{
		SchemaVersion: JournalSchemaVersion, EventID: event.EventID, Timestamp: event.Timestamp,
		Sequence: event.Sequence, Kind: event.Kind, SlotID: event.SlotID,
		ResourceGeneration: event.ResourceGeneration, LeaseEpoch: event.LeaseEpoch,
		FromState: event.FromState, ToState: event.ToState, Step: event.Step, Outcome: outcome,
		Operation: event.Operation, DurationMS: event.DurationMS, IncidentID: event.IncidentID,
		RecoveryAttempt: event.RecoveryAttempt,
	}, true
}

func validateJournalRecord(record JournalRecord) error {
	if record.SchemaVersion != JournalSchemaVersion || !brokerRecordID.MatchString(record.EventID) ||
		record.Timestamp.IsZero() || record.Sequence == 0 || !safeEventKind(record.Kind) {
		return errors.New("invalid error journal identity")
	}
	if (record.SlotID != "" && !brokerSlotID.MatchString(record.SlotID)) ||
		(record.IncidentID != "" && !brokerRecordID.MatchString(record.IncidentID)) ||
		record.Outcome == "" || !closedValue(record.Outcome, journalOutcomes) ||
		!closedValue(record.Step, journalSteps) || !closedValue(record.Operation, journalOperations) ||
		record.DurationMS < 0 || record.Own {
		return errors.New("invalid error journal fields")
	}
	return nil
}

func (recorder EventRecorder) journalDirectory() string {
	return filepath.Join(recorder.archiveRoot(), "pool", "errors")
}

// Called by Record after the spool write, under the same archive lock and
// retention as the controllers' own records.
func (recorder EventRecorder) archiveJournalRecord(record JournalRecord) error {
	if err := validateJournalRecord(record); err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(payload) > maxJournalRecordBytes {
		return errors.New("error journal record exceeds the size limit")
	}
	return withFileLock(filepath.Join(recorder.archiveRoot(), ".lock"), func() error {
		if err := writeImmutableDurable(filepath.Join(recorder.journalDirectory(), record.EventID+".json"), append(payload, '\n'), 0o600); err != nil {
			return err
		}
		return recorder.trimJournal()
	})
}

// trimJournal bounds the journal while the host sink cannot acknowledge, which
// is the only time the shared archive is pruned.
func (recorder EventRecorder) trimJournal() error {
	entries, err := os.ReadDir(recorder.journalDirectory())
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok && brokerRecordID.MatchString(id) {
			names = append(names, entry.Name())
		}
	}
	// Record IDs begin with the zero-padded creation time.
	sort.Strings(names)
	for len(names) > maxJournalRecords {
		if err := os.Remove(filepath.Join(recorder.journalDirectory(), names[0])); err != nil && !os.IsNotExist(err) {
			return err
		}
		names = names[1:]
	}
	return nil
}

// readJournal returns the newest pool failures, oldest first. A reader that
// passes its private client ID learns which records belong to its own leases;
// "-" reads the same records anonymously.
func (recorder EventRecorder) readJournal(clientID, slot string, count int) ([]JournalRecord, error) {
	if (clientID != "-" && !agentLogClientID.MatchString(clientID)) || count < 1 ||
		count > maxJournalReadEvents || (slot != "-" && !brokerSlotID.MatchString(slot)) {
		return nil, errors.New("invalid error journal selection")
	}
	records := make([]JournalRecord, 0)
	entries, err := os.ReadDir(recorder.journalDirectory())
	if os.IsNotExist(err) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() != id+".json" || !brokerRecordID.MatchString(id) {
			continue
		}
		payload, err := readAgentLogRecord(filepath.Join(recorder.journalDirectory(), entry.Name()), maxJournalRecordBytes+1)
		if os.IsNotExist(err) {
			continue // Concurrent acknowledgement can prune expired records.
		}
		if err != nil {
			return nil, err
		}
		var record JournalRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, err
		}
		if err := validateJournalRecord(record); err != nil {
			return nil, err
		}
		if record.EventID != id || (slot != "-" && slot != record.SlotID) ||
			recorder.now().Sub(record.Timestamp) >= agentLogRetention {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].Timestamp.Equal(records[j].Timestamp) {
			return records[i].Timestamp.Before(records[j].Timestamp)
		}
		if records[i].Sequence != records[j].Sequence {
			return records[i].Sequence < records[j].Sequence
		}
		return records[i].EventID < records[j].EventID
	})
	if len(records) > count {
		records = records[len(records)-count:]
	}
	if clientID != "-" {
		own := recorder.agentLogDirectory(clientID, "events")
		for index := range records {
			info, err := os.Lstat(filepath.Join(own, records[index].EventID+".json"))
			records[index].Own = err == nil && info.Mode().IsRegular()
		}
	}
	for total, index := 2, len(records)-1; index >= 0; index-- {
		payload, _ := json.Marshal(records[index])
		total += len(payload) + 1
		if total > maxJournalReadBytes {
			records = records[index+1:]
			break
		}
	}
	return records, nil
}
