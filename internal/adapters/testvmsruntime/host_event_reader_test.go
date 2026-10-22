package testvmsruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func readerEvent(sequence int, source, slot string) BrokerEvent {
	return BrokerEvent{SchemaVersion: BrokerEventSchemaVersion, Timestamp: time.Unix(100, 0).UTC(), EventID: fmt.Sprintf("%020d-%016x", sequence, sequence), Sequence: uint64(sequence), Component: "test-vms-broker", Pool: "agent-e2e/test-vms", Source: source, Kind: "recovery.available", SlotID: slot}
}

func TestHostBrokerEventReaderSnapshots(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "events")
	write := func(events ...BrokerEvent) {
		t.Helper()
		var payload bytes.Buffer
		for _, event := range events {
			if err := json.NewEncoder(&payload).Encode(event); err != nil {
				t.Fatal(err)
			}
		}
		if err := writeAtomicDurable(path, payload.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	missingReader := HostBrokerEventReader{Path: path, SlotID: "slot-001"}
	if got, err := missingReader.Read(1); err != nil || len(got) != 0 {
		t.Fatalf("missing: %v %v", got, err)
	}
	a, b, c := readerEvent(1, "a", "slot-001"), readerEvent(2, "b", "slot-001"), readerEvent(3, "a", "slot-002")
	write(a, b, c)
	if got, err := missingReader.Read(1); err != nil || len(got) != 2 {
		t.Fatalf("events after initially absent snapshot: %v %v", got, err)
	}
	reader := HostBrokerEventReader{Path: path, SlotID: "slot-001"}
	got, err := reader.Read(1)
	if err != nil || len(got) != 1 || got[0].EventID != b.EventID || len(reader.seen) != 2 {
		t.Fatalf("initial tail/baseline: %v %v ids=%d", got, err, len(reader.seen))
	}
	identity := reader.snapshot
	if got, err := reader.Read(1); err != nil || len(got) != 0 || reader.snapshot != identity {
		t.Fatalf("unchanged snapshot reread: %v %v", got, err)
	}
	// A new inode with identical length and mtime must still be decoded.
	d := readerEvent(4, "c", "slot-001")
	write(a, d, c)
	if err := os.Chtimes(path, identity.ModTime(), identity.ModTime()); err != nil {
		t.Fatal(err)
	}
	got, err = reader.Read(1)
	if err != nil || len(got) != 1 || got[0].EventID != d.EventID || len(reader.seen) != 2 {
		t.Fatalf("replacement: %v %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Read(1); err != nil || len(got) != 0 {
		t.Fatalf("disappearance: %v %v", got, err)
	}
	write(a, d, c)
	if got, err := reader.Read(1); err != nil || len(got) != 0 {
		t.Fatalf("reappearance replay: %v %v", got, err)
	}
	// Rotation drops old IDs. A later restoration is new relative to retained history.
	write(b)
	if got, err := reader.Read(1); err != nil || len(got) != 1 || len(reader.seen) != 1 {
		t.Fatalf("rotation: %v %v ids=%d", got, err, len(reader.seen))
	}
	// In-place truncation is also a new snapshot.
	testkit.WriteFile(t, path, nil, 0o600)
	if got, err := reader.Read(1); err != nil || len(got) != 0 || len(reader.seen) != 0 {
		t.Fatalf("truncation: %v %v", got, err)
	}
	write(a)
	if got, err := reader.Read(1); err != nil || len(got) != 1 {
		t.Fatalf("restored history: %v %v", got, err)
	}
}

func TestHostBrokerEventReaderLateEventBeforeFormerWindow(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "events")
	var payload bytes.Buffer
	for i := 2; i <= 100002; i++ {
		if err := json.NewEncoder(&payload).Encode(readerEvent(i, "current", "slot-001")); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, path, payload.Bytes(), 0o600)
	reader := HostBrokerEventReader{Path: path}
	start := time.Now()
	if got, err := reader.Read(1); err != nil || len(got) != 1 {
		t.Fatalf("initial: count=%d err=%v", len(got), err)
	}
	parsed := time.Since(start)
	start = time.Now()
	if got, err := reader.Read(1); err != nil || len(got) != 0 {
		t.Fatalf("unchanged: count=%d err=%v", len(got), err)
	}
	t.Logf("bounded 100001-record snapshot: initial parse=%s unchanged open/stat=%s", parsed, time.Since(start))
	var replacement bytes.Buffer
	late := readerEvent(1, "late", "slot-001")
	late.Timestamp = time.Unix(1, 0).UTC()
	if err := json.NewEncoder(&replacement).Encode(late); err != nil {
		t.Fatal(err)
	}
	replacement.Write(payload.Bytes())
	if err := writeAtomicDurable(path, replacement.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Read(1); err != nil || len(got) != 1 || got[0].EventID != late.EventID {
		t.Fatalf("late event: count=%d err=%v", len(got), err)
	}
}

func TestHostBrokerEventReaderRejectsInvalidSnapshots(t *testing.T) {
	for _, payload := range [][]byte{[]byte("{\n"), []byte("{}\n"), bytes.Repeat([]byte("x"), maxBrokerEventBytes+1)} {
		path := filepath.Join(testkit.TempDir(t), "events")
		testkit.WriteFile(t, path, payload, 0o600)
		reader := HostBrokerEventReader{Path: path}
		if _, err := reader.Read(1); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
		if reader.snapshot != nil || reader.seen != nil {
			t.Fatal("failed read advanced baseline")
		}
	}
	reader := HostBrokerEventReader{Path: testkit.TempDir(t)}
	if _, err := reader.Read(1); err == nil {
		t.Fatal("directory read accepted")
	}
}

func TestHostSinkInvalidBatchHasNoEffects(t *testing.T) {
	root := testkit.TempDir(t)
	sink := HostSink{DataHome: root, OperatorGID: -1}
	invalid := readerEvent(2, "bad", "slot-001")
	invalid.SchemaVersion = 999
	if err := sink.Ingest(SpoolBatch{SchemaVersion: BrokerSpoolSchemaVersion, Events: []BrokerEvent{readerEvent(1, "good", "slot-001"), invalid}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid batch published effects: %v %v", entries, err)
	}
}
