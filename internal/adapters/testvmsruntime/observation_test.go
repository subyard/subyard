package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestObservationCursorIsIndependentPrivateAndReadOnly(t *testing.T) {
	root := testkit.TempDir(t)
	now := time.Now().UTC()
	recorder := EventRecorder{StateDir: root, Source: "fixture", Now: func() time.Time { return now }}
	owner, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	var events []BrokerEvent
	for _, client := range []string{owner, other, owner, owner} {
		event, err := recorder.Record(BrokerEvent{Kind: "lease.held", SlotID: "slot-001", ResourceGeneration: 1, LeaseEpoch: 1, agentClientID: client})
		if err != nil {
			t.Fatal(err)
		}
		if client == owner {
			events = append(events, event)
		}
	}
	before, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	first, err := recorder.readObservationPage(owner, "-", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 || first.Events[0].EventID != events[0].EventID || !first.More || first.Gap {
		t.Fatalf("first page: %+v", first)
	}
	second, err := recorder.readObservationPage(owner, first.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].EventID != events[1].EventID {
		t.Fatalf("second page: %+v", second)
	}
	replay, err := recorder.readObservationPage(owner, first.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replay.NextCursor != second.NextCursor {
		t.Fatal("cursor replay changed")
	}
	after, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	if !bytes.Equal(b, a) {
		t.Fatal("read consumed producer spool")
	}
	if err := recorder.Ack([]string{events[0].EventID}, nil); err != nil {
		t.Fatal(err)
	}
	page, err := recorder.readObservationPage(owner, "-", 200)
	if err != nil || len(page.Events) != 3 {
		t.Fatalf("host ack consumed independent archive: %+v %v", page, err)
	}
	if err := os.Remove(filepath.Join(recorder.agentLogDirectory(owner, "events"), events[0].EventID+".json")); err != nil {
		t.Fatal(err)
	}
	page, err = recorder.readObservationPage(owner, events[0].EventID, 200)
	if err != nil || !page.Gap || len(page.Events) != 2 {
		t.Fatalf("missing cursor: %+v %v", page, err)
	}
	empty, err := recorder.readObservationPage(strings.Repeat("c", 32), "-", 200)
	if err != nil || len(empty.Events) != 0 {
		t.Fatal("foreign observer read events")
	}
}

func TestObservationRejectsStaleGenerationAfterNativeRead(t *testing.T) {
	root := testkit.TempDir(t)
	owner := strings.Repeat("a", 32)
	store := LeaseStore{Path: filepath.Join(root, "leases.json"), SlotCount: 1}
	cfg := fixtureConfig(t)
	spec, _ := cfg.EnvironmentSpec(EnvironmentPair)
	grant, err := store.AcquireV3Slot(spec, owner, "SHA256:fixture", "default", "Fixture", "run", "tests", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	recorder := EventRecorder{StateDir: root, Source: "fixture"}
	var output bytes.Buffer
	facade := Facade{Store: store, Events: &recorder, Output: &output, OnObserve: func(slots []LeaseSlot) []ResourceObservation {
		if len(slots) != 1 {
			t.Fatal("wrong owner selection")
		}
		return []ResourceObservation{{SlotID: grant.SlotID, ResourceGeneration: grant.ResourceGeneration + 1, LeaseEpoch: grant.LeaseEpoch}}
	}}
	if err := facade.Run("observe-v1 " + owner + " - 10"); err != nil {
		t.Fatal(err)
	}
	var response facadeResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.Observation == nil || len(response.Observation.Observations) != 0 {
		t.Fatal("stale sample accepted")
	}
	current, _ := os.ReadFile(store.Path)
	if !bytes.Equal(original, current) {
		t.Fatal("observer mutated lease")
	}
	output.Reset()
	if err := facade.Run("observe-v1 " + strings.Repeat("b", 32) + " - 10"); err != nil {
		t.Fatal(err)
	}
}

func TestObservationNativeCountersRequireManagedGeneration(t *testing.T) {
	for _, generation := range []string{"2", "3"} {
		t.Run(generation, func(t *testing.T) {
			cfg := fixtureConfig(t)
			spec, _ := cfg.EnvironmentSpec(EnvironmentPair)
			spec.Count = 1
			slot := LeaseSlot{SlotID: "slot-001", ResourceGeneration: 2, LeaseEpoch: 3, Environment: &spec, State: SlotHeld}
			queries := 0
			rt := Runtime{Config: cfg, Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				queries++
				if len(args) != 2 || args[0] != "query" {
					t.Fatalf("mutation: %v", args)
				}
				if strings.Contains(args[1], "/state?") {
					return []byte(`{"pid":0,"cpu":{"usage":1234},"memory":{"usage":5678}}`), nil, nil
				}
				return []byte(fmt.Sprintf(`{"type":"virtual-machine","config":{"user.subyard.managed":%q,"user.subyard.generation":%q,"user.subyard.lease-epoch":"3"}}`, managedMarker, generation)), nil, nil
			}}}
			samples := rt.Observe(context.Background(), []LeaseSlot{slot})
			if len(samples) != 1 {
				t.Fatal("missing allocation")
			}
			if generation == "2" {
				if samples[0].CPU != nil || samples[0].Memory != nil || queries != 2 {
					t.Fatal("guest-agent metrics were treated as host usage")
				}
			} else if samples[0].CPU != nil || queries != 1 {
				t.Fatal("read foreign counters")
			}
			if samples[0].IORead != nil || samples[0].FinalComplete {
				t.Fatal("fabricated I/O or complete final tail")
			}
		})
	}
}

func TestObservationHostProcessCountersKeepRSSAndBlockIOSemantics(t *testing.T) {
	root := testkit.TempDir(t)
	// Values after comm: state, ppid..cmajflt (10), utime, stime..starttime.
	stat := "42 (qemu fixture) S 1 2 3 4 5 6 7 8 9 10 20 30 0 0 0 0 1 0 999 0\n"
	for name, value := range map[string]string{"42/stat": stat, "42/status": "VmRSS: 10 kB\nVmHWM: 12 kB\nVmSwap: 1 kB\n", "42/smaps_rollup": "Pss: 8 kB\n", "42/io": "read_bytes: 1024\nwrite_bytes: 2048\nsyscr: 99\nsyscw: 98\n", "sys/kernel/random/boot_id": "fixture-boot"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var sample ResourceObservation
	if err := sample.readProcess(root, "42", 100); err != nil {
		t.Fatal(err)
	}
	if sample.CPU == nil || *sample.CPU != 500000000 || sample.RSS == nil || *sample.RSS != 10240 || sample.PSS == nil || *sample.PSS != 8192 || sample.IORead == nil || *sample.IORead != 1024 {
		t.Fatalf("host counters %+v", sample)
	}
	if sample.Memory != nil || sample.IOReadOps != nil || sample.CounterGeneration == "" {
		t.Fatal("mixed RSS/guest memory or logical/block operation counts")
	}
	originalGeneration := sample.CounterGeneration
	if err := os.WriteFile(filepath.Join(root, "42/stat"), []byte(strings.Replace(stat, "999", "1000", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	var successor ResourceObservation
	if err := successor.readProcess(root, "42", 100); err != nil {
		t.Fatal(err)
	}
	if successor.CounterGeneration == originalGeneration {
		t.Fatal("PID reuse retained old counter identity")
	}
}

func TestObservationProcessOwnershipRefusesForeignUUIDAndPIDReuse(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "42"), 0700); err != nil {
		t.Fatal(err)
	}
	uuid := "12345678-1234-1234-1234-123456789abc"
	path := filepath.Join(root, "42/cmdline")
	if err := os.WriteFile(path, []byte("/usr/bin/qemu-system-x86_64\x00-uuid\x00"+uuid+"\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	if !ownedQEMUProcess(root, "42", uuid) {
		t.Fatal("owned QEMU was not recognized")
	}
	if ownedQEMUProcess(root, "42", "00000000-0000-0000-0000-000000000000") {
		t.Fatal("foreign UUID was accepted")
	}
	if err := os.WriteFile(path, []byte("/usr/bin/sleep\x0010\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	if ownedQEMUProcess(root, "42", uuid) {
		t.Fatal("reused PID was accepted")
	}
}
