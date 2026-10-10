package testvmsruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	BrokerEventSchemaVersion    = 1
	BrokerIncidentSchemaVersion = 1
	BrokerSpoolSchemaVersion    = 1
	maxBrokerEventBytes         = 32 << 10
	maxIncidentBytes            = 2 << 20
	maxSpoolBatchBytes          = 16 << 20
	maxSpoolBatchPayloadBytes   = maxSpoolBatchBytes - 64<<10
	maxSpoolBatchRecords        = 1000
)

var (
	brokerRecordID = regexp.MustCompile(`^[0-9]{20}-[0-9a-f]{16}$`)
	brokerSlotID   = regexp.MustCompile(`^slot-[0-9]{3}$`)
	sshKeyValue    = regexp.MustCompile(`\bssh-(?:ed25519|rsa)\s+[A-Za-z0-9+/=]+`)
	secretField    = regexp.MustCompile(`(?i)\b(capability(?:_hash)?|private_key|controller_fingerprint|token|password|secret)(\s*=\s*)[^\s,"']+`)
	secretJSON     = regexp.MustCompile(`(?i)("(?:capability(?:_hash)?|private_key|controller_fingerprint|token|password|secret)"\s*:\s*)"(?:\\.|[^"\\])*"`)
	secretYAML     = regexp.MustCompile(`(?im)^([ \t]*(?:capability(?:_hash)?|private_key|controller_fingerprint|token|password|secret)[ \t]*:[ \t]*).*$`)
	privateKeyPEM  = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
)

type BrokerEvent struct {
	SchemaVersion      int           `json:"schema_version"`
	Timestamp          time.Time     `json:"timestamp"`
	EventID            string        `json:"event_id"`
	Sequence           uint64        `json:"sequence"`
	Component          string        `json:"component"`
	Pool               string        `json:"pool"`
	Source             string        `json:"source,omitempty"`
	Kind               string        `json:"kind"`
	SlotID             string        `json:"slot_id,omitempty"`
	ResourceGeneration uint64        `json:"resource_generation,omitempty"`
	LeaseEpoch         uint64        `json:"lease_epoch,omitempty"`
	FromState          SlotState     `json:"from_state,omitempty"`
	ToState            SlotState     `json:"to_state,omitempty"`
	RecoveryAttempt    uint64        `json:"recovery_attempt,omitempty"`
	DurationMS         int64         `json:"duration_ms,omitempty"`
	Error              string        `json:"error,omitempty"`
	IncidentID         string        `json:"incident_id,omitempty"`
	Context            *LeaseContext `json:"context,omitempty"`
}

type CommandEvidence struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type IncidentArtifact struct {
	SchemaVersion      int               `json:"schema_version"`
	IncidentID         string            `json:"incident_id"`
	CreatedAt          time.Time         `json:"created_at"`
	Source             string            `json:"source,omitempty"`
	SlotID             string            `json:"slot_id"`
	ResourceGeneration uint64            `json:"resource_generation"`
	LeaseEpoch         uint64            `json:"lease_epoch"`
	FailureReason      string            `json:"failure_reason"`
	Context            *LeaseContext     `json:"context,omitempty"`
	Command            *CommandEvidence  `json:"command,omitempty"`
	Diagnostics        map[string]string `json:"diagnostics,omitempty"`
}

type SpoolBatch struct {
	SchemaVersion int                `json:"schema_version"`
	Events        []BrokerEvent      `json:"events,omitempty"`
	Incidents     []IncidentArtifact `json:"incidents,omitempty"`
}

type EventRecorder struct {
	StateDir string
	Source   string
	Now      func() time.Time
}

func (recorder EventRecorder) now() time.Time {
	if recorder.Now != nil {
		return recorder.Now().UTC()
	}
	return time.Now().UTC()
}

func (recorder EventRecorder) spoolRoot() string {
	return filepath.Join(recorder.StateDir, "spool")
}

func (recorder EventRecorder) eventDirectory() string {
	return filepath.Join(recorder.spoolRoot(), "events")
}

func (recorder EventRecorder) incidentDirectory() string {
	return filepath.Join(recorder.spoolRoot(), "incidents")
}

func (recorder EventRecorder) Record(event BrokerEvent) (BrokerEvent, error) {
	if recorder.StateDir == "" {
		return BrokerEvent{}, errors.New("broker event state directory is required")
	}
	event.SchemaVersion = BrokerEventSchemaVersion
	event.Timestamp = recorder.now()
	event.Component = "test-vms-broker"
	event.Pool = "agent-e2e/test-vms"
	event.Source = recorder.Source
	sequence, err := recorder.nextSequence()
	if err != nil {
		return BrokerEvent{}, err
	}
	event.Sequence = sequence
	if event.EventID == "" {
		event.EventID, err = newBrokerRecordID(event.Timestamp)
		if err != nil {
			return BrokerEvent{}, err
		}
	}
	event.Error = redactBrokerText(event.Error, 8192)
	if event.Context != nil {
		current := *event.Context
		if err := validateLeaseContext(current); err != nil {
			event.Context = nil
		} else {
			event.Context = &current
		}
	}
	if err := validateBrokerEvent(event); err != nil {
		return BrokerEvent{}, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return BrokerEvent{}, err
	}
	payload = append(payload, '\n')
	if len(payload) > maxBrokerEventBytes {
		return BrokerEvent{}, errors.New("broker event exceeds the size limit")
	}
	path := filepath.Join(recorder.eventDirectory(), event.EventID+".json")
	if err := writeImmutableDurable(path, payload, 0o600); err != nil {
		return BrokerEvent{}, err
	}
	return event, nil
}

func (recorder EventRecorder) SaveIncident(
	slot LeaseSlot,
	cause error,
	diagnostics map[string]string,
) (IncidentArtifact, error) {
	if recorder.StateDir == "" {
		return IncidentArtifact{}, errors.New("broker incident state directory is required")
	}
	created := recorder.now()
	incidentID, err := newBrokerRecordID(created)
	if err != nil {
		return IncidentArtifact{}, err
	}
	artifact := IncidentArtifact{
		SchemaVersion:      BrokerIncidentSchemaVersion,
		IncidentID:         incidentID,
		CreatedAt:          created,
		Source:             recorder.Source,
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		FailureReason:      redactBrokerText(errorString(cause), 16<<10),
		Context:            leaseContextFromSlot(slot),
		Diagnostics:        make(map[string]string),
	}
	var commandError *CommandError
	if errors.As(cause, &commandError) {
		artifact.Command = &CommandEvidence{
			Command:    redactBrokerText(commandError.Command(), 4096),
			ExitCode:   commandError.ExitCode,
			DurationMS: commandError.Duration.Milliseconds(),
			Error:      redactBrokerText(commandError.Message, 8192),
		}
	}
	for name, value := range diagnostics {
		if !safeIncidentSection(name) {
			continue
		}
		if name == "vm_1_qmp_log" || name == "vm_2_qmp_log" {
			artifact.Diagnostics[name] = redactBrokerLogTail(value)
		} else {
			artifact.Diagnostics[name] = redactBrokerText(value, 256<<10)
		}
	}
	if err := validateIncidentArtifact(artifact); err != nil {
		return IncidentArtifact{}, err
	}
	payload, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return IncidentArtifact{}, err
	}
	payload = append(payload, '\n')
	if len(payload) > maxIncidentBytes {
		return IncidentArtifact{}, errors.New("broker incident exceeds the size limit")
	}
	path := filepath.Join(recorder.incidentDirectory(), artifact.IncidentID+".json")
	if err := writeImmutableDurable(path, payload, 0o600); err != nil {
		return IncidentArtifact{}, err
	}
	return artifact, nil
}

func (recorder EventRecorder) Export() (SpoolBatch, error) {
	batch := SpoolBatch{SchemaVersion: BrokerSpoolSchemaVersion}
	total := 0
exportRecords:
	for _, item := range []struct {
		directory string
		decode    func([]byte) error
	}{
		{
			directory: recorder.eventDirectory(),
			decode: func(payload []byte) error {
				var event BrokerEvent
				if err := json.Unmarshal(payload, &event); err != nil {
					return err
				}
				if err := validateBrokerEvent(event); err != nil {
					return err
				}
				batch.Events = append(batch.Events, event)
				return nil
			},
		},
		{
			directory: recorder.incidentDirectory(),
			decode: func(payload []byte) error {
				var incident IncidentArtifact
				if err := json.Unmarshal(payload, &incident); err != nil {
					return err
				}
				if err := validateIncidentArtifact(incident); err != nil {
					return err
				}
				batch.Incidents = append(batch.Incidents, incident)
				return nil
			},
		},
	} {
		entries, err := os.ReadDir(item.directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return SpoolBatch{}, err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if len(batch.Events)+len(batch.Incidents) >= maxSpoolBatchRecords {
				break exportRecords
			}
			payload, err := os.ReadFile(filepath.Join(item.directory, entry.Name()))
			if err != nil {
				return SpoolBatch{}, err
			}
			if total != 0 && total+len(payload) > maxSpoolBatchPayloadBytes {
				break exportRecords
			}
			if len(payload) > maxSpoolBatchPayloadBytes {
				return SpoolBatch{}, errors.New("broker spool export exceeds the size limit")
			}
			total += len(payload)
			if err := item.decode(payload); err != nil {
				return SpoolBatch{}, fmt.Errorf("decode broker spool %s: %w", entry.Name(), err)
			}
		}
	}
	sortBrokerEvents(batch.Events)
	sort.Slice(batch.Incidents, func(i, j int) bool {
		return batch.Incidents[i].IncidentID < batch.Incidents[j].IncidentID
	})
	return batch, nil
}

func (recorder EventRecorder) Ack(eventIDs, incidentIDs []string) error {
	for _, group := range []struct {
		directory string
		ids       []string
	}{
		{directory: recorder.eventDirectory(), ids: eventIDs},
		{directory: recorder.incidentDirectory(), ids: incidentIDs},
	} {
		for _, id := range group.ids {
			if !brokerRecordID.MatchString(id) {
				return fmt.Errorf("invalid broker spool acknowledgement %q", id)
			}
			if err := os.Remove(filepath.Join(group.directory, id+".json")); err != nil &&
				!os.IsNotExist(err) {
				return err
			}
		}
		if len(group.ids) != 0 {
			if err := syncDirectory(group.directory); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func validateBrokerEvent(event BrokerEvent) error {
	if !schemaInMigrationWindow(event.SchemaVersion, BrokerEventSchemaVersion) {
		return errors.New("unsupported broker event schema")
	}
	if event.Timestamp.IsZero() || !brokerRecordID.MatchString(event.EventID) {
		return errors.New("invalid broker event identity")
	}
	if event.Sequence == 0 {
		return errors.New("invalid broker event sequence")
	}
	if event.Component != "test-vms-broker" || event.Pool != "agent-e2e/test-vms" {
		return errors.New("invalid broker event source")
	}
	if !safeEventKind(event.Kind) || !safeLeaseText(event.Source, 96) ||
		(event.SlotID != "" && !brokerSlotID.MatchString(event.SlotID)) {
		return errors.New("invalid broker event fields")
	}
	if event.IncidentID != "" && !brokerRecordID.MatchString(event.IncidentID) {
		return errors.New("invalid broker incident reference")
	}
	if event.Context != nil && validateLeaseContext(*event.Context) != nil {
		return errors.New("invalid broker event attribution")
	}
	if event.Error != redactBrokerText(event.Error, 8192) {
		return errors.New("unredacted broker event error")
	}
	return nil
}

func validateIncidentArtifact(artifact IncidentArtifact) error {
	if !schemaInMigrationWindow(artifact.SchemaVersion, BrokerIncidentSchemaVersion) ||
		!brokerRecordID.MatchString(artifact.IncidentID) ||
		artifact.CreatedAt.IsZero() ||
		!brokerSlotID.MatchString(artifact.SlotID) {
		return errors.New("invalid broker incident identity")
	}
	if !safeLeaseText(artifact.Source, 96) ||
		artifact.FailureReason != redactBrokerText(artifact.FailureReason, 16<<10) {
		return errors.New("invalid broker incident fields")
	}
	if artifact.Context != nil && validateLeaseContext(*artifact.Context) != nil {
		return errors.New("invalid broker incident attribution")
	}
	if artifact.Command != nil {
		if artifact.Command.Command != redactBrokerText(artifact.Command.Command, 4096) ||
			artifact.Command.Error != redactBrokerText(artifact.Command.Error, 8192) {
			return errors.New("unredacted broker command evidence")
		}
	}
	for name, value := range artifact.Diagnostics {
		if !safeIncidentSection(name) || value != redactBrokerText(value, 256<<10) {
			return errors.New("invalid broker incident diagnostics")
		}
	}
	return nil
}

func validateSpoolBatch(batch SpoolBatch) error {
	if !schemaInMigrationWindow(batch.SchemaVersion, BrokerSpoolSchemaVersion) {
		return errors.New("unsupported broker spool schema")
	}
	if len(batch.Events)+len(batch.Incidents) > maxSpoolBatchRecords {
		return errors.New("broker spool batch has too many records")
	}
	for _, event := range batch.Events {
		if err := validateBrokerEvent(event); err != nil {
			return err
		}
	}
	for _, incident := range batch.Incidents {
		if err := validateIncidentArtifact(incident); err != nil {
			return err
		}
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	if len(payload) > maxSpoolBatchBytes {
		return errors.New("broker spool batch exceeds the size limit")
	}
	return nil
}

func safeEventKind(value string) bool {
	if value == "" || len(value) > 80 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') &&
			character != '.' && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func safeIncidentSection(value string) bool {
	if value == "" || len(value) > 80 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') &&
			character != '.' && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func newBrokerRecordID(now time.Time) (string, error) {
	token, err := randomToken(8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%020d-%s", now.UTC().UnixNano(), token), nil
}

func (recorder EventRecorder) nextSequence() (uint64, error) {
	path := filepath.Join(recorder.spoolRoot(), "sequence")
	var result uint64
	err := withFileLock(filepath.Join(recorder.spoolRoot(), ".record.lock"), func() error {
		payload, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		current := uint64(0)
		if len(strings.TrimSpace(string(payload))) != 0 {
			current, err = strconv.ParseUint(strings.TrimSpace(string(payload)), 10, 64)
			if err != nil {
				return errors.New("invalid broker event sequence state")
			}
		}
		if current == ^uint64(0) {
			return errors.New("broker event sequence exhausted")
		}
		result = current + 1
		return writeAtomicDurable(
			path,
			[]byte(strconv.FormatUint(result, 10)+"\n"),
			0o600,
		)
	})
	return result, err
}

func sortBrokerEvents(events []BrokerEvent) {
	sort.Slice(events, func(i, j int) bool {
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.Before(events[j].Timestamp)
		}
		if events[i].Source != events[j].Source {
			return events[i].Source < events[j].Source
		}
		if events[i].Sequence != events[j].Sequence {
			return events[i].Sequence < events[j].Sequence
		}
		return events[i].EventID < events[j].EventID
	})
}

func leaseContextFromSlot(slot LeaseSlot) *LeaseContext {
	schema := LeaseAttributionSchemaV1
	yard := slot.Yard
	if yard != "" || slot.Checkout == "" {
		schema = LeaseAttributionSchemaVersion
		if yard == "" {
			yard = "unknown"
		}
	}
	context := LeaseContext{
		SchemaVersion: schema,
		Yard:          yard,
		Project:       slot.Project,
		Checkout:      slot.Checkout,
		Run:           slot.Run,
		Purpose:       slot.Purpose,
	}
	if validateLeaseContext(context) != nil {
		return nil
	}
	return &context
}

func errorString(err error) string {
	if err == nil {
		return "unknown broker failure"
	}
	return err.Error()
}

func redactBrokerText(value string, maximum int) string {
	value = sanitizeBrokerText(value)
	if len(value) > maximum {
		value = value[:maximum]
	}
	return value
}

func redactBrokerLogTail(value string) string {
	// Redact complete input before slicing: a private key can cross the tail boundary.
	value = sanitizeBrokerText(value)
	const maximum = 64 << 10
	const omitted = "[earlier log omitted]\n"
	if len(value) <= maximum {
		return value
	}
	start := len(value) - (maximum - len(omitted))
	for !utf8.RuneStart(value[start]) {
		start++
	}
	return omitted + value[start:]
}

func sanitizeBrokerText(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' {
			return character
		}
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, value)
	value = sshKeyValue.ReplaceAllString(value, "ssh-key [redacted]")
	value = secretField.ReplaceAllString(value, "${1}${2}[redacted]")
	value = secretJSON.ReplaceAllString(value, `${1}"[redacted]"`)
	value = secretYAML.ReplaceAllString(value, "${1}[redacted]")
	value = privateKeyPEM.ReplaceAllString(value, "private-key [redacted]")
	return value
}

func writeImmutableDurable(path string, payload []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if current, err := os.ReadFile(path); err == nil {
		if string(current) == string(payload) {
			return nil
		}
		return errors.New("immutable broker record already exists with different contents")
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	defer func() {
		_ = file.Close()
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temp, path); err != nil {
		if os.IsExist(err) {
			current, readErr := os.ReadFile(path)
			if readErr == nil && string(current) == string(payload) {
				return nil
			}
			return errors.New("immutable broker record already exists with different contents")
		}
		return err
	}
	if err := os.Remove(temp); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func withFileLock(path string, operation func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return operation()
}
