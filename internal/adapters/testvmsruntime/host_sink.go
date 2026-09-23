package testvmsruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	HostEventLogName        = "test-vms-broker.jsonl"
	closedEventRetention    = 90 * 24 * time.Hour
	closedEventMaxBytes     = int64(128 << 20)
	closedIncidentRetention = 30 * 24 * time.Hour
	closedIncidentMaxBytes  = int64(512 << 20)
)

type HostSink struct {
	DataHome          string
	Incus             string
	Runner            CommandRunner
	Output            io.Writer
	Now               func() time.Time
	OperatorGID       int
	EventRetention    time.Duration
	EventMaxBytes     int64
	IncidentRetention time.Duration
	IncidentMaxBytes  int64
}

type outerBrokerInstance struct {
	Name           string            `json:"name"`
	Project        string            `json:"project"`
	Status         string            `json:"status"`
	Config         map[string]string `json:"config"`
	ExpandedConfig map[string]string `json:"expanded_config"`
}

func (sink *HostSink) Sync(ctx context.Context) error {
	if sink.DataHome == "" || !filepath.IsAbs(sink.DataHome) {
		return errors.New("absolute Subyard data home is required")
	}
	if sink.Runner == nil {
		sink.Runner = ProcessRunner{}
	}
	if sink.Incus == "" {
		sink.Incus = "incus"
	}
	if sink.Output == nil {
		sink.Output = io.Discard
	}
	payload, _, err := sink.Runner.Run(
		ctx,
		sink.Incus,
		[]string{"list", "--all-projects", "--format=json"},
		nil,
		nil,
	)
	if err != nil {
		return fmt.Errorf("list broker yards: %w", err)
	}
	if len(payload) > 16<<20 {
		return errors.New("broker yard inventory exceeds the size limit")
	}
	var instances []outerBrokerInstance
	if err := json.Unmarshal(payload, &instances); err != nil {
		return fmt.Errorf("decode broker yard inventory: %w", err)
	}
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Project == instances[j].Project {
			return instances[i].Name < instances[j].Name
		}
		return instances[i].Project < instances[j].Project
	})
	var result error
	for _, instance := range instances {
		if !strings.EqualFold(instance.Status, "running") ||
			!supportedBrokerSpoolSchema(instance.testVMSpoolSchema()) {
			continue
		}
		batch, fetchErr := sink.fetch(ctx, instance)
		if fetchErr != nil {
			result = errors.Join(result, fetchErr)
			continue
		}
		if len(batch.Events) == 0 && len(batch.Incidents) == 0 {
			continue
		}
		if ingestErr := sink.Ingest(batch); ingestErr != nil {
			result = errors.Join(result, fmt.Errorf(
				"ingest broker spool from %s/%s: %w",
				instance.Project,
				instance.Name,
				ingestErr,
			))
			continue
		}
		if evidenceErr := sink.saveHostEvidence(ctx, instance, batch.Incidents); evidenceErr != nil {
			result = errors.Join(result, evidenceErr)
			continue
		}
		if ackErr := sink.ack(ctx, instance, batch); ackErr != nil {
			result = errors.Join(result, ackErr)
			continue
		}
		fmt.Fprintf(
			sink.Output,
			"synced %d broker event(s) and %d incident(s) from %s/%s\n",
			len(batch.Events),
			len(batch.Incidents),
			instance.Project,
			instance.Name,
		)
	}
	return result
}

func supportedBrokerSpoolSchema(value string) bool {
	observed, err := strconv.Atoi(value)
	return err == nil && schemaInMigrationWindow(observed, BrokerSpoolSchemaVersion)
}

func schemaInMigrationWindow(observed, current int) bool {
	if current < 1 || observed < 1 || observed > current {
		return false
	}
	return observed == current || observed == current-1
}

func (instance outerBrokerInstance) testVMSpoolSchema() string {
	if value := instance.ExpandedConfig["user.subyard.test_vms_spool_schema"]; value != "" {
		return value
	}
	return instance.Config["user.subyard.test_vms_spool_schema"]
}

func (sink *HostSink) fetch(
	ctx context.Context,
	instance outerBrokerInstance,
) (SpoolBatch, error) {
	payload, _, err := sink.Runner.Run(
		ctx,
		sink.Incus,
		[]string{
			"exec",
			instance.Name,
			"--project",
			instance.Project,
			"--mode=non-interactive",
			"--",
			DefaultInstalledPath,
			"_test-vms-worker",
			"spool-export",
		},
		nil,
		nil,
	)
	if err != nil {
		if previousProducerWithoutSpool(err) {
			return SpoolBatch{SchemaVersion: BrokerSpoolSchemaVersion}, nil
		}
		return SpoolBatch{}, fmt.Errorf(
			"fetch broker spool from %s/%s: %w",
			instance.Project,
			instance.Name,
			err,
		)
	}
	if len(payload) > maxSpoolBatchBytes {
		return SpoolBatch{}, errors.New("broker spool response exceeds the size limit")
	}
	var batch SpoolBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		return SpoolBatch{}, fmt.Errorf("decode broker spool: %w", err)
	}
	if err := validateSpoolBatch(batch); err != nil {
		return SpoolBatch{}, err
	}
	return batch, nil
}

func previousProducerWithoutSpool(err error) bool {
	var commandError *CommandError
	return errors.As(err, &commandError) &&
		strings.Contains(commandError.Message, "unknown test-vms worker command") &&
		strings.Contains(commandError.Message, `"spool-export"`)
}

func (sink *HostSink) ack(
	ctx context.Context,
	instance outerBrokerInstance,
	batch SpoolBatch,
) error {
	acknowledgement := struct {
		EventIDs    []string `json:"event_ids"`
		IncidentIDs []string `json:"incident_ids"`
	}{}
	for _, event := range batch.Events {
		acknowledgement.EventIDs = append(acknowledgement.EventIDs, event.EventID)
	}
	for _, incident := range batch.Incidents {
		acknowledgement.IncidentIDs = append(
			acknowledgement.IncidentIDs,
			incident.IncidentID,
		)
	}
	payload, err := json.Marshal(acknowledgement)
	if err != nil {
		return err
	}
	_, _, err = sink.Runner.Run(
		ctx,
		sink.Incus,
		[]string{
			"exec",
			instance.Name,
			"--project",
			instance.Project,
			"--mode=non-interactive",
			"--env",
			"SUBYARD_SPOOL_ACK=" + string(payload),
			"--",
			DefaultInstalledPath,
			"_test-vms-worker",
			"spool-ack",
			"--yes",
		},
		nil,
		nil,
	)
	if err != nil {
		return fmt.Errorf(
			"acknowledge broker spool from %s/%s: %w",
			instance.Project,
			instance.Name,
			err,
		)
	}
	return nil
}

func (sink *HostSink) Ingest(batch SpoolBatch) error {
	if err := validateSpoolBatch(batch); err != nil {
		return err
	}
	root := filepath.Join(sink.DataHome, "logs")
	return withFileLock(
		filepath.Join(sink.DataHome, ".test-vms-broker-sink.lock"),
		func() error {
			if err := sink.ensureLogRoot(root); err != nil {
				return err
			}
			eventDirectory := filepath.Join(root, ".test-vms-broker-events")
			incidentDirectory := filepath.Join(root, "test-vms-broker-incidents")
			for _, directory := range []string{eventDirectory, incidentDirectory} {
				if err := os.MkdirAll(directory, 0o750); err != nil {
					return err
				}
			}
			if err := os.Chmod(eventDirectory, 0o700); err != nil {
				return err
			}
			if err := os.Chmod(incidentDirectory, 0o750); err != nil {
				return err
			}
			if err := sink.setOperatorGroup(incidentDirectory); err != nil {
				return err
			}
			for _, event := range batch.Events {
				if err := validateBrokerEvent(event); err != nil {
					return err
				}
				payload, err := json.Marshal(event)
				if err != nil {
					return err
				}
				payload = append(payload, '\n')
				path := filepath.Join(eventDirectory, event.EventID+".json")
				if err := writeImmutableDurable(path, payload, 0o640); err != nil {
					return err
				}
				if err := sink.setOperatorGroup(path); err != nil {
					return err
				}
			}
			for _, incident := range batch.Incidents {
				if err := validateIncidentArtifact(incident); err != nil {
					return err
				}
				payload, err := json.MarshalIndent(incident, "", "  ")
				if err != nil {
					return err
				}
				payload = append(payload, '\n')
				if len(payload) > maxIncidentBytes {
					return errors.New("broker incident exceeds the size limit")
				}
				path := filepath.Join(incidentDirectory, incident.IncidentID+".json")
				if err := writeImmutableDurable(path, payload, 0o640); err != nil {
					return err
				}
				if err := sink.setOperatorGroup(path); err != nil {
					return err
				}
			}
			events, err := readEventDirectory(eventDirectory)
			if err != nil {
				return err
			}
			if err := sink.rotateIncidents(incidentDirectory, events); err != nil {
				return err
			}
			events, err = sink.rotateEvents(eventDirectory, incidentDirectory, events)
			if err != nil {
				return err
			}
			return sink.materialize(root, events)
		},
	)
}

func (sink *HostSink) materialize(root string, events []BrokerEvent) error {
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	path := filepath.Join(root, HostEventLogName)
	if err := writeAtomicDurable(path, payload.Bytes(), 0o640); err != nil {
		return err
	}
	return sink.setOperatorGroup(path)
}

func (sink *HostSink) rotateIncidents(
	directory string,
	events []BrokerEvent,
) error {
	type recoveredSlot struct {
		generation uint64
		at         time.Time
	}
	recovered := map[string]recoveredSlot{}
	for _, event := range events {
		if event.Kind != "recovery.available" || event.SlotID == "" {
			continue
		}
		current := recovered[event.Source+"\x00"+event.SlotID]
		if event.ResourceGeneration > current.generation ||
			(event.ResourceGeneration == current.generation && event.Timestamp.After(current.at)) {
			recovered[event.Source+"\x00"+event.SlotID] = recoveredSlot{
				generation: event.ResourceGeneration,
				at:         event.Timestamp,
			}
		}
	}
	type closedIncident struct {
		path string
		at   time.Time
		size int64
	}
	var closed []closedIncident
	var closedBytes int64
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if sink.Now != nil {
		now = sink.Now().UTC()
	}
	retention := sink.IncidentRetention
	if retention <= 0 {
		retention = closedIncidentRetention
	}
	maxBytes := sink.IncidentMaxBytes
	if maxBytes <= 0 {
		maxBytes = closedIncidentMaxBytes
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var incident IncidentArtifact
		if err := json.Unmarshal(payload, &incident); err != nil {
			return err
		}
		success, ok := recovered[incident.Source+"\x00"+incident.SlotID]
		if !ok || success.generation <= incident.ResourceGeneration {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size := info.Size()
		if hostInfo, err := os.Stat(filepath.Join(directory, "host", entry.Name())); err == nil {
			size += hostInfo.Size()
		} else if !os.IsNotExist(err) {
			return err
		}
		closed = append(closed, closedIncident{
			path: path,
			at:   success.at,
			size: size,
		})
		closedBytes += size
	}
	sort.Slice(closed, func(i, j int) bool {
		if closed[i].at.Equal(closed[j].at) {
			return closed[i].path < closed[j].path
		}
		return closed[i].at.Before(closed[j].at)
	})
	for _, incident := range closed {
		if now.Sub(incident.at) < retention &&
			closedBytes <= maxBytes {
			continue
		}
		if err := os.Remove(incident.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		hostPath := filepath.Join(directory, "host", filepath.Base(incident.path))
		if err := os.Remove(hostPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		closedBytes -= incident.size
	}
	return syncDirectory(directory)
}

func (sink *HostSink) rotateEvents(
	eventDirectory string,
	incidentDirectory string,
	events []BrokerEvent,
) ([]BrokerEvent, error) {
	retainedIDs := make(map[string]bool)
	retainedGenerations := make(map[string]bool)
	incidentEntries, err := os.ReadDir(incidentDirectory)
	if err != nil {
		return nil, err
	}
	for _, entry := range incidentEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		payload, readErr := os.ReadFile(filepath.Join(incidentDirectory, entry.Name()))
		if readErr != nil {
			return nil, readErr
		}
		var incident IncidentArtifact
		if decodeErr := json.Unmarshal(payload, &incident); decodeErr != nil {
			return nil, decodeErr
		}
		if validateErr := validateIncidentArtifact(incident); validateErr != nil {
			return nil, validateErr
		}
		retainedIDs[incident.IncidentID] = true
		retainedGenerations[eventGenerationKey(
			incident.Source,
			incident.SlotID,
			incident.ResourceGeneration,
		)] = true
	}

	now := time.Now().UTC()
	if sink.Now != nil {
		now = sink.Now().UTC()
	}
	retention := sink.EventRetention
	if retention <= 0 {
		retention = closedEventRetention
	}
	maxBytes := sink.EventMaxBytes
	if maxBytes <= 0 {
		maxBytes = closedEventMaxBytes
	}

	type storedEvent struct {
		event     BrokerEvent
		path      string
		size      int64
		protected bool
	}
	stored := make([]storedEvent, 0, len(events))
	var totalBytes int64
	for _, event := range events {
		path := filepath.Join(eventDirectory, event.EventID+".json")
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, statErr
		}
		protected := retainedIDs[event.IncidentID] ||
			retainedGenerations[eventGenerationKey(
				event.Source,
				event.SlotID,
				event.ResourceGeneration,
			)]
		stored = append(stored, storedEvent{
			event: event, path: path, size: info.Size(), protected: protected,
		})
		totalBytes += info.Size()
	}

	kept := make([]BrokerEvent, 0, len(stored))
	for _, item := range stored {
		expired := now.Sub(item.event.Timestamp) >= retention
		overCap := totalBytes > maxBytes
		if !item.protected && (expired || overCap) {
			if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			totalBytes -= item.size
			continue
		}
		kept = append(kept, item.event)
	}
	if len(kept) != len(stored) {
		if err := syncDirectory(eventDirectory); err != nil {
			return nil, err
		}
	}
	return kept, nil
}

func eventGenerationKey(source, slotID string, generation uint64) string {
	return source + "\x00" + slotID + "\x00" + strconv.FormatUint(generation, 10)
}

func (sink *HostSink) setOperatorGroup(path string) error {
	if os.Geteuid() == 0 && sink.OperatorGID >= 0 {
		return os.Chown(path, 0, sink.OperatorGID)
	}
	return nil
}

func (sink *HostSink) ensureLogRoot(root string) error {
	info, statErr := os.Stat(root)
	if statErr == nil {
		stat, isSystemStat := info.Sys().(*syscall.Stat_t)
		if os.Geteuid() == 0 && sink.OperatorGID >= 0 &&
			isSystemStat && stat.Uid == 0 {
			if err := os.Chown(root, 0, sink.OperatorGID); err != nil {
				return err
			}
			return os.Chmod(root, info.Mode().Perm()|0o070)
		}
		return nil
	}
	if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.MkdirAll(root, 0o770); err != nil {
		return err
	}
	if os.Geteuid() == 0 && sink.OperatorGID >= 0 {
		if err := os.Chown(root, 0, sink.OperatorGID); err != nil {
			return err
		}
	}
	return os.Chmod(root, 0o770)
}

func readEventDirectory(directory string) ([]BrokerEvent, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []BrokerEvent
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(directory, entry.Name()))
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
		events = append(events, event)
	}
	sortBrokerEvents(events)
	return events, nil
}

func ReadHostBrokerEvents(
	path string,
	lines int,
	slotID string,
) ([]BrokerEvent, error) {
	if lines < 1 {
		return nil, errors.New("line count must be positive")
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxBrokerEventBytes)
	var events []BrokerEvent
	for scanner.Scan() {
		var event BrokerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode broker event log: %w", err)
		}
		if err := validateBrokerEvent(event); err != nil {
			return nil, err
		}
		if slotID != "" && event.SlotID != slotID {
			continue
		}
		events = append(events, event)
		if len(events) > lines {
			events = events[len(events)-lines:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func ParseOperatorGID(value string) (int, error) {
	if value == "" {
		return -1, nil
	}
	gid, err := strconv.Atoi(value)
	if err != nil || gid < 0 {
		return -1, errors.New("invalid Subyard operator GID")
	}
	return gid, nil
}

func writeAtomicDurable(path string, payload []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
