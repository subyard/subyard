package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestEventRecorderPersistsRedactedImmutableSpoolAndAcknowledges(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	recorder := EventRecorder{
		StateDir: t.TempDir(),
		Source:   "test-yard",
		Now:      func() time.Time { return now },
	}
	slot := LeaseSlot{
		SlotID:             "slot-002",
		ResourceGeneration: 3,
		LeaseEpoch:         9,
		State:              SlotQuarantined,
		Project:            "Subyard/Subyard",
		Checkout:           "checkout-a",
		Run:                "run-a",
		Purpose:            "acceptance",
	}
	event, err := recorder.Record(BrokerEvent{
		Kind:               "slot.quarantined",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		Error:              `capability=secret {"token":"json-secret"} ssh-ed25519 AAAAB3NzaFixture`,
		Context:            leaseContextFromSlot(slot),
	})
	if err != nil {
		t.Fatal(err)
	}
	incident, err := recorder.SaveIncident(
		slot,
		&CommandError{
			Name:     "incus",
			Args:     []string{"stop", "e2e-vm-1", "--timeout", "60"},
			ExitCode: 1,
			Duration: 60 * time.Second,
			Message: "capability=secret context deadline exceeded\n" +
				"private_key: yaml-secret\n" +
				"-----BEGIN OPENSSH PRIVATE KEY-----\npem-secret\n" +
				"-----END OPENSSH PRIVATE KEY-----",
		},
		map[string]string{"vm_1_info_log": "ssh-ed25519 AAAAFixture"},
	)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"capability=secret",
		"json-secret",
		"yaml-secret",
		"pem-secret",
		"AAAAB3NzaFixture",
		"AAAAFixture",
	} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("spool disclosed %q: %s", secret, payload)
		}
	}
	if len(batch.Events) != 1 || len(batch.Incidents) != 1 ||
		batch.Incidents[0].Command == nil ||
		batch.Incidents[0].Command.DurationMS != 60000 ||
		batch.Incidents[0].Context == nil ||
		batch.Incidents[0].Context.Project != "Subyard/Subyard" {
		t.Fatalf("spool batch = %#v", batch)
	}
	if err := recorder.Ack(
		[]string{event.EventID},
		[]string{incident.IncidentID},
	); err != nil {
		t.Fatal(err)
	}
	empty, err := recorder.Export()
	if err != nil || len(empty.Events) != 0 || len(empty.Incidents) != 0 {
		t.Fatalf("acknowledged spool = %#v, %v", empty, err)
	}
}

func TestLeaseContextTreatsRollbackDroppedYardAsUnknownV2(t *testing.T) {
	context := leaseContextFromSlot(LeaseSlot{
		Project: "Subyard-2", Run: "run-a", Purpose: "acceptance",
	})
	if context == nil || context.SchemaVersion != 2 || context.Yard != "unknown" ||
		context.Project != "Subyard-2" || context.Checkout != "" {
		t.Fatalf("rollback context = %#v", context)
	}
}

func TestHostSinkReplayIsIdempotentAndOpenIncidentIsNotRotated(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	recorder := EventRecorder{
		StateDir: t.TempDir(),
		Source:   "test-yard",
		Now:      func() time.Time { return now },
	}
	slot := LeaseSlot{
		SlotID:             "slot-001",
		ResourceGeneration: 1,
		State:              SlotQuarantined,
	}
	event, err := recorder.Record(BrokerEvent{
		Kind:               "slot.quarantined",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		ToState:            SlotQuarantined,
	})
	if err != nil {
		t.Fatal(err)
	}
	incident, err := recorder.SaveIncident(
		slot,
		errors.New("stop timeout"),
		map[string]string{"project": "fixture"},
	)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	ackFails := true
	runner := &fakeRunner{handler: func(
		_ string,
		arguments []string,
		_ []string,
		_ io.Reader,
	) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		switch {
		case joined == "list --all-projects --format=json":
			return []byte(`[{
				"name":"yard-test","project":"subyard-test","status":"Running",
				"expanded_config":{"user.subyard.test_vms_spool_schema":"1"}
			}]`), nil, nil
		case strings.Contains(joined, "spool-export"):
			payload, _ := json.Marshal(batch)
			return payload, nil, nil
		case strings.Contains(joined, "spool-ack"):
			if ackFails {
				return nil, nil, errors.New("sink acknowledgement offline")
			}
			return nil, nil, nil
		default:
			return nil, nil, errors.New("unexpected host sink call")
		}
	}}
	sink := &HostSink{
		DataHome:    dataHome,
		Runner:      runner,
		Now:         func() time.Time { return now.Add(365 * 24 * time.Hour) },
		OperatorGID: -1,
	}
	if err := sink.Sync(context.Background()); err == nil {
		t.Fatal("offline acknowledgement was accepted")
	}
	ackFails = false
	if err := sink.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := ReadHostBrokerEvents(
		filepath.Join(dataHome, "logs", HostEventLogName),
		10,
		"",
	)
	if err != nil || len(events) != 1 || events[0].EventID != event.EventID {
		t.Fatalf("idempotent event log = %#v, %v", events, err)
	}
	incidentPath := filepath.Join(
		dataHome,
		"logs",
		"test-vms-broker-incidents",
		incident.IncidentID+".json",
	)
	if _, err := os.Stat(incidentPath); err != nil {
		t.Fatalf("unresolved incident was rotated: %v", err)
	}
}

func TestHostSinkRotatesOnlyResolvedIncidents(t *testing.T) {
	created := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	recorder := EventRecorder{
		StateDir: t.TempDir(),
		Source:   "test-yard",
		Now:      func() time.Time { return created },
	}
	slot := LeaseSlot{
		SlotID:             "slot-003",
		ResourceGeneration: 4,
		State:              SlotQuarantined,
	}
	incident, err := recorder.SaveIncident(
		slot,
		errors.New("fixture failure"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Record(BrokerEvent{
		Kind:               "recovery.available",
		SlotID:             slot.SlotID,
		ResourceGeneration: 5,
		FromState:          SlotRecovering,
		ToState:            SlotAvailable,
		IncidentID:         incident.IncidentID,
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	sink := &HostSink{
		DataHome:    dataHome,
		Now:         func() time.Time { return created.Add(31 * 24 * time.Hour) },
		OperatorGID: -1,
	}
	if err := sink.Ingest(batch); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(
		dataHome,
		"logs",
		"test-vms-broker-incidents",
		incident.IncidentID+".json",
	)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("resolved old incident remains: %v", err)
	}
	events, err := ReadHostBrokerEvents(
		filepath.Join(dataHome, "logs", HostEventLogName),
		10,
		"",
	)
	if err != nil || len(events) != 1 || events[0].IncidentID != incident.IncidentID {
		t.Fatalf("incident metadata timeline = %#v, %v", events, err)
	}
}

func TestHostSinkResolvedIncidentCapRemovesOldestFirst(t *testing.T) {
	firstAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Hour)
	record := func(
		slotID string,
		at time.Time,
	) (SpoolBatch, IncidentArtifact) {
		t.Helper()
		recorder := EventRecorder{
			StateDir: t.TempDir(),
			Source:   "test-yard",
			Now:      func() time.Time { return at },
		}
		slot := LeaseSlot{
			SlotID:             slotID,
			ResourceGeneration: 1,
			State:              SlotQuarantined,
		}
		incident, err := recorder.SaveIncident(
			slot,
			errors.New("fixture failure"),
			map[string]string{"project": "fixture"},
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recorder.Record(BrokerEvent{
			Kind:               "recovery.available",
			SlotID:             slotID,
			ResourceGeneration: 2,
			IncidentID:         incident.IncidentID,
		}); err != nil {
			t.Fatal(err)
		}
		batch, err := recorder.Export()
		if err != nil {
			t.Fatal(err)
		}
		return batch, incident
	}
	firstBatch, first := record("slot-001", firstAt)
	secondBatch, second := record("slot-002", secondAt)
	batch := SpoolBatch{
		SchemaVersion: BrokerSpoolSchemaVersion,
		Events:        append(firstBatch.Events, secondBatch.Events...),
		Incidents:     append(firstBatch.Incidents, secondBatch.Incidents...),
	}
	dataHome := t.TempDir()
	sink := &HostSink{
		DataHome:          dataHome,
		Now:               func() time.Time { return secondAt.Add(time.Hour) },
		IncidentRetention: 365 * 24 * time.Hour,
		IncidentMaxBytes:  closedIncidentMaxBytes,
		OperatorGID:       -1,
	}
	if err := sink.Ingest(batch); err != nil {
		t.Fatal(err)
	}
	incidentDirectory := filepath.Join(
		dataHome,
		"logs",
		"test-vms-broker-incidents",
	)
	secondInfo, err := os.Stat(filepath.Join(incidentDirectory, second.IncidentID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	sink.IncidentMaxBytes = secondInfo.Size()
	if err := sink.Ingest(SpoolBatch{SchemaVersion: BrokerSpoolSchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(
		filepath.Join(incidentDirectory, first.IncidentID+".json"),
	); !os.IsNotExist(err) {
		t.Fatalf("oldest resolved incident remains: %v", err)
	}
	if _, err := os.Stat(
		filepath.Join(incidentDirectory, second.IncidentID+".json"),
	); err != nil {
		t.Fatalf("newest resolved incident was removed: %v", err)
	}
}

func TestHostSinkBoundsEventsWithoutDroppingRetainedIncidentTimeline(t *testing.T) {
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recorder := EventRecorder{
		StateDir: t.TempDir(),
		Source:   "test-yard",
		Now:      func() time.Time { return created },
	}
	openSlot := LeaseSlot{
		SlotID:             "slot-001",
		ResourceGeneration: 1,
		State:              SlotQuarantined,
	}
	openIncident, err := recorder.SaveIncident(
		openSlot,
		errors.New("still unresolved"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := recorder.Record(BrokerEvent{
		Kind:               "slot.quarantined",
		SlotID:             openSlot.SlotID,
		ResourceGeneration: openSlot.ResourceGeneration,
		IncidentID:         openIncident.IncidentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := recorder.Record(BrokerEvent{
		Kind:               "lease.available",
		SlotID:             "slot-002",
		ResourceGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	sink := &HostSink{
		DataHome:          dataHome,
		Now:               func() time.Time { return created.Add(91 * 24 * time.Hour) },
		IncidentRetention: 365 * 24 * time.Hour,
		OperatorGID:       -1,
	}
	if err := sink.Ingest(batch); err != nil {
		t.Fatal(err)
	}
	events, err := ReadHostBrokerEvents(
		filepath.Join(dataHome, "logs", HostEventLogName),
		10,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != protected.EventID {
		t.Fatalf("age-bounded event log = %#v, ordinary=%s", events, ordinary.EventID)
	}

	secondRecorder := EventRecorder{
		StateDir: t.TempDir(),
		Source:   "test-yard",
		Now:      func() time.Time { return created.Add(92 * 24 * time.Hour) },
	}
	_, err = secondRecorder.Record(BrokerEvent{
		Kind: "broker.start",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondRecorder.Record(BrokerEvent{
		Kind: "reaper.start",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondBatch, err := secondRecorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	protectedInfo, err := os.Stat(filepath.Join(
		dataHome,
		"logs",
		".test-vms-broker-events",
		protected.EventID+".json",
	))
	if err != nil {
		t.Fatal(err)
	}
	secondPayload, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	sink.Now = func() time.Time { return created.Add(92 * 24 * time.Hour) }
	sink.EventMaxBytes = protectedInfo.Size() + int64(len(secondPayload)+1)
	if err := sink.Ingest(secondBatch); err != nil {
		t.Fatal(err)
	}
	events, err = ReadHostBrokerEvents(
		filepath.Join(dataHome, "logs", HostEventLogName),
		10,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 ||
		events[0].EventID != protected.EventID ||
		events[1].EventID != second.EventID {
		t.Fatalf("size-bounded event log = %#v", events)
	}
}

func TestBrokerEventsKeepProducerOrderAtTheSameTimestamp(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	recorder := EventRecorder{
		StateDir: t.TempDir(),
		Source:   "test-yard",
		Now:      func() time.Time { return now },
	}
	for _, kind := range []string{"recovery.start", "rebuild.delete", "rebuild.create"} {
		if _, err := recorder.Record(BrokerEvent{
			Kind:   kind,
			SlotID: "slot-001",
		}); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	if err := (&HostSink{DataHome: dataHome, OperatorGID: -1}).Ingest(batch); err != nil {
		t.Fatal(err)
	}
	events, err := ReadHostBrokerEvents(
		filepath.Join(dataHome, "logs", HostEventLogName),
		10,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %#v", events)
	}
	for index, kind := range []string{"recovery.start", "rebuild.delete", "rebuild.create"} {
		if events[index].Kind != kind || events[index].Sequence != uint64(index+1) {
			t.Fatalf("event order = %#v", events)
		}
	}
}

func TestHostSinkSkipsProducerWithoutSpoolSchemaMarker(t *testing.T) {
	runner := &fakeRunner{handler: func(
		_ string,
		arguments []string,
		_ []string,
		_ io.Reader,
	) ([]byte, []byte, error) {
		if strings.Join(arguments, " ") != "list --all-projects --format=json" {
			return nil, nil, errors.New("pre-spool producer was queried")
		}
		return []byte(`[{
			"name":"yard-test","project":"subyard-test","status":"Running",
			"expanded_config":{"user.subyard.test_vms_revision":"legacy"}
		}]`), nil, nil
	}}
	sink := &HostSink{
		DataHome:    t.TempDir(),
		Runner:      runner,
		OperatorGID: -1,
	}
	if err := sink.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("pre-spool producer calls = %#v", runner.calls)
	}
}

func TestHostSinkAcceptsImmediatePreviousProducerDuringRollback(t *testing.T) {
	runner := &fakeRunner{handler: func(
		name string,
		arguments []string,
		_ []string,
		_ io.Reader,
	) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		switch joined {
		case "list --all-projects --format=json":
			return []byte(`[{
				"name":"yard-test","project":"subyard-test","status":"Running",
				"expanded_config":{"user.subyard.test_vms_spool_schema":"1"}
			}]`), nil, nil
		default:
			if strings.Contains(joined, "spool-export") {
				return nil, nil, &CommandError{
					Name:     name,
					Args:     arguments,
					ExitCode: 1,
					Message:  `test-vms: unknown test-vms worker command "spool-export"`,
				}
			}
			return nil, nil, errors.New("unexpected rollback sink call")
		}
	}}
	sink := &HostSink{
		DataHome:    t.TempDir(),
		Runner:      runner,
		OperatorGID: -1,
	}
	if err := sink.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("previous producer calls = %#v", runner.calls)
	}
}

func TestBrokerSchemaMigrationWindowAcceptsOnlyCurrentAndPrevious(t *testing.T) {
	for _, test := range []struct {
		observed int
		current  int
		want     bool
	}{
		{observed: 1, current: 1, want: true},
		{observed: 0, current: 1, want: false},
		{observed: 1, current: 2, want: true},
		{observed: 2, current: 2, want: true},
		{observed: 0, current: 2, want: false},
		{observed: 3, current: 2, want: false},
		{observed: 1, current: 3, want: false},
		{observed: 2, current: 3, want: true},
		{observed: 3, current: 3, want: true},
	} {
		if got := schemaInMigrationWindow(test.observed, test.current); got != test.want {
			t.Errorf(
				"schemaInMigrationWindow(%d, %d) = %v, want %v",
				test.observed,
				test.current,
				got,
				test.want,
			)
		}
	}
}

func TestEventRecorderKeepsRecentQMPRecordsAfterFullRedaction(t *testing.T) {
	recorder := EventRecorder{StateDir: testkit.TempDir(t), Source: "test-yard"}
	slot := LeaseSlot{SlotID: "slot-001", ResourceGeneration: 7, LeaseEpoch: 3, State: SlotRecovering}
	ordinary := "ordinary-start\n" + strings.Repeat("x", 280<<10) + "ordinary-end"
	qmp := strings.Repeat("old QMP record € token=x\n", 16000) +
		"-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("pem-boundary-secret\n", 12000) +
		"-----END OPENSSH PRIVATE KEY-----\n" +
		"token=private-qmp-token\nQUERY: system_powerdown latest-control\nEvent: POWERDOWN latest-event\n"
	cut := len(qmp) - (64 << 10)
	if !(strings.Index(qmp, "-----BEGIN") < cut && cut < strings.Index(qmp, "-----END")) {
		t.Fatal("fixture must cross the raw tail boundary inside a private key")
	}
	diagnostics := map[string]string{"vm_1_qmp_log": qmp, "vm_2_qmp_log": qmp}
	for _, section := range []string{"project", "service_journal", "vm_1_info_log", "vm_2_info_log", "vm_1_console_log", "vm_2_console_log"} {
		diagnostics[section] = ordinary
	}
	incident, err := recorder.SaveIncident(slot, errors.New("stop timeout"), diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(recorder.incidentDirectory(), incident.IncidentID+".json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) >= maxIncidentBytes {
		t.Fatalf("bounded incident too large: %d", len(payload))
	}
	for _, secret := range []string{"pem-boundary-secret", "private-qmp-token", "token=x"} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("durable incident contains %q", secret)
		}
	}
	batch, err := recorder.Export()
	if err != nil || len(batch.Incidents) != 1 {
		t.Fatalf("export: incidents=%d error=%v", len(batch.Incidents), err)
	}
	for _, artifact := range []IncidentArtifact{incident, batch.Incidents[0]} {
		for _, section := range []string{"vm_1_qmp_log", "vm_2_qmp_log"} {
			value := artifact.Diagnostics[section]
			if len(value) > 64<<10 || !utf8.ValidString(value) ||
				!strings.HasPrefix(value, "[earlier log omitted]\n") ||
				!strings.Contains(value, "latest-control") || !strings.Contains(value, "latest-event") {
				t.Fatalf("recent bounded QMP evidence missing in %s", section)
			}
		}
		for section := range diagnostics {
			if !strings.HasSuffix(section, "_qmp_log") && artifact.Diagnostics[section] != ordinary[:256<<10] {
				t.Fatalf("ordinary section retention changed: %s", section)
			}
		}
	}
}
