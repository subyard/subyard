package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAgentLogsOwnFailedAndReleasedLeasesSurviveSinkAcknowledgement(t *testing.T) {
	root := testkit.TempDir(t)
	now := time.Now().UTC()
	recorder := EventRecorder{StateDir: root, Source: "fixture", Now: func() time.Time { return now }}
	store := LeaseStore{Path: filepath.Join(root, "leases.json"), SlotCount: 2, Now: recorder.Now}
	var output bytes.Buffer
	facade := Facade{Store: store, Output: &output, Events: &recorder}
	owner := strings.Repeat("a", 32)
	other := strings.Repeat("b", 32)
	key := strings.Fields(fixturePublicKey(t))
	request := func(command string) facadeResponse {
		t.Helper()
		output.Reset()
		if err := facade.Run(command); err != nil {
			t.Fatal(err)
		}
		var response facadeResponse
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), owner) || strings.Contains(output.String(), other) {
			t.Fatal("facade disclosed private controller identity")
		}
		return response
	}
	acquire := func(client, slot string) *LeaseGrant {
		t.Helper()
		response := request(fmt.Sprintf("acquire-v3 subyard-pair %s SHA256:fixture default Fixture shared-run tests %s %s %s", client, key[0], key[1], slot))
		if response.Status != "ok" || response.Grant == nil {
			t.Fatalf("acquire: %#v", response)
		}
		return response.Grant
	}
	first := acquire(owner, "slot-001")
	if err := store.AbortProvisioning(*first); err != nil {
		t.Fatal(err)
	}
	second := acquire(other, "slot-001") // Same slot and display metadata, different lease owner.
	_ = acquire(other, "slot-002")
	facade.OnAcquire = func(grant LeaseGrant, _ string) (LeaseGrant, error) {
		return grant, errors.New("fixture provisioning failed")
	}
	if err := store.AbortProvisioning(*second); err != nil {
		t.Fatal(err)
	}
	failed := request(fmt.Sprintf("acquire-v3 subyard-pair %s SHA256:fixture default Fixture shared-run tests %s %s slot-001", owner, key[0], key[1]))
	if failed.Code != "quarantined" || failed.Grant != nil {
		t.Fatalf("failed acquire: %#v", failed)
	}
	slot, err := storeSlot(store, "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	incident, err := recorder.SaveIncident(slot, errors.New("fixture "+owner), map[string]string{
		"vm_1_allocation_verified": "true",
		"vm_1_console_log":         "kernel panic token=fixture " + owner,
		"service_journal":          "neighbor private log", "legacy_failure_log": "previous lease private log",
		"unknown_section": "unscoped private log",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = recorder.Record(BrokerEvent{agentClientID: owner, Kind: "slot.quarantined", SlotID: slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration, LeaseEpoch: slot.LeaseEpoch,
		IncidentID: incident.IncidentID, Error: "fixture " + owner})
	if err != nil {
		t.Fatal(err)
	}
	// Recovery's terminal event uses the next generation; it still belongs to the failed lease.
	_, err = recorder.Record(BrokerEvent{agentClientID: owner, Kind: "recovery.available", SlotID: slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration + 1, LeaseEpoch: slot.LeaseEpoch,
		FromState: SlotRecovering, ToState: SlotAvailable})
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		response := request("logs-v1 " + owner + " slot-001 50")
		if response.Status != "ok" || response.Events == nil || len(*response.Events) != 5 {
			t.Fatalf("own history: %#v", response)
		}
		for _, event := range *response.Events {
			if event.LeaseEpoch != first.LeaseEpoch && event.LeaseEpoch != slot.LeaseEpoch {
				t.Fatal("another owner's reused slot leaked")
			}
		}
		if denied := request("logs-v1 " + owner + " slot-002 50"); denied.Events == nil || len(*denied.Events) != 0 {
			t.Fatal("another slot leaked")
		}
		if denied := request("logs-v1 " + strings.Repeat("c", 32) + " - 50"); denied.Events == nil || len(*denied.Events) != 0 {
			t.Fatal("unknown credential could read history")
		}
		response = request("incident-v1 " + owner + " " + incident.IncidentID)
		if response.Incident == nil || len(response.Incident.Diagnostics) != 1 ||
			!strings.Contains(response.Incident.Diagnostics["vm_1_console_log"], "kernel panic") {
			t.Fatalf("incident: %#v", response)
		}
		if denied := request("incident-v1 " + other + " " + incident.IncidentID); denied.Status != "error" || denied.Incident != nil {
			t.Fatal("other controller read incident")
		}
	}
	check()
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, event := range batch.Events {
		ids = append(ids, event.EventID)
	}
	if err := recorder.Ack(ids, []string{incident.IncidentID}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Ack(ids, []string{incident.IncidentID}); err != nil {
		t.Fatal("ack replay:", err)
	}
	batch, err = recorder.Export()
	if err != nil || len(batch.Events) != 0 || len(batch.Incidents) != 0 {
		t.Fatalf("archive was exported again: %#v %v", batch, err)
	}
	check()
	if tail := request("logs-v1 " + owner + " - 1"); tail.Events == nil || len(*tail.Events) != 1 || (*tail.Events)[0].Kind != "recovery.available" {
		t.Fatal("tail did not preserve the newest event")
	}
	for _, command := range []string{"logs-v1 ../owner - 1", "logs-v1 " + owner + " ../slot 1", "logs-v1 " + owner + " - 201", "incident-v1 " + owner + " ../incident"} {
		if response := request(command); response.Code != "invalid_request" {
			t.Fatalf("invalid selection accepted: %s", command)
		}
	}
	after, err := os.ReadFile(store.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("diagnostic reads mutated lease state")
	}
	now = now.Add(agentLogRetention + time.Second)
	if expired := request("logs-v1 " + owner + " - 50"); expired.Events == nil || len(*expired.Events) != 0 {
		t.Fatal("expired records were still readable")
	}
	if err := recorder.Ack(nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(recorder.agentLogDirectory(owner, "incidents"), incident.IncidentID+".json")); !os.IsNotExist(err) {
		t.Fatal("expired archive was not pruned")
	}
	if _, err := os.Stat(filepath.Join(recorder.archiveRoot(), capabilityDigest(owner))); !os.IsNotExist(err) {
		t.Fatal("expired controller directory was not pruned")
	}
}

func TestAgentLogsRejectUnsafeArchiveRecordsAndBoundOutput(t *testing.T) {
	recorder := EventRecorder{StateDir: testkit.TempDir(t)}
	owner := strings.Repeat("a", 32)
	grant := LeaseGrant{SlotID: "slot-001", ResourceGeneration: 1, LeaseEpoch: 1}
	var ids []string
	for index := 0; index < maxAgentLogEvents; index++ {
		event, err := recorder.Record(BrokerEvent{agentClientID: owner, Kind: "fixture.event", SlotID: grant.SlotID,
			ResourceGeneration: grant.ResourceGeneration, LeaseEpoch: grant.LeaseEpoch, Error: strings.Repeat("x", 8192)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, event.EventID)
	}
	// Record publication uses temporary files while another request may be reading.
	testkit.WriteFile(t, filepath.Join(recorder.agentLogDirectory(owner, "events"), ".record.tmp"), nil, 0o600)
	events, err := recorder.readAgentEvents(owner, "-", maxAgentLogEvents)
	payload, _ := json.Marshal(events)
	if err != nil || len(events) == 0 || len(payload) > maxAgentLogBytes {
		t.Fatalf("unbounded response: events=%d bytes=%d err=%v", len(events), len(payload), err)
	}
	if err := recorder.Ack(ids, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(recorder.agentLogDirectory(owner, "events"), ids[len(ids)-1]+".json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.readAgentEvents(owner, "-", 1); err == nil {
		t.Fatal("public archive record was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(recorder.agentLogDirectory(owner, "events"), ids[0]+".json"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.readAgentEvents(owner, "-", 1); err == nil {
		t.Fatal("symlink archive record was accepted")
	}
}

func TestAgentLogArchiveEnforcesByteCap(t *testing.T) {
	recorder := EventRecorder{StateDir: testkit.TempDir(t)}
	path := filepath.Join(recorder.archiveRoot(), "events", "00000000000000000001-0123456789abcdef.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, nil, 0o600)
	// A sparse fixture exercises the cap without allocating a large payload.
	if err := os.Truncate(path, agentLogMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Ack(nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("archive byte cap did not remove the oldest record")
	}
}

func TestAgentLogsDoNotAuthorizeReusedCountersAfterPoolReset(t *testing.T) {
	for _, reset := range []string{"resize", "new-state"} {
		t.Run(reset, func(t *testing.T) {
			recorder := EventRecorder{StateDir: testkit.TempDir(t)}
			store := LeaseStore{Path: filepath.Join(recorder.StateDir, "leases.json"), SlotCount: 2}
			owner, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
			var output bytes.Buffer
			facade := Facade{Store: store, Output: &output, Events: &recorder}
			key := strings.Fields(fixturePublicKey(t))
			acquire := func(client string) LeaseGrant {
				t.Helper()
				output.Reset()
				if err := facade.Run(fmt.Sprintf("acquire-v3 subyard-pair %s SHA256:fixture default Fixture shared-run tests %s %s slot-002", client, key[0], key[1])); err != nil {
					t.Fatal(err)
				}
				var response facadeResponse
				if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Grant == nil {
					t.Fatalf("acquire: %s %v", output.String(), err)
				}
				return *response.Grant
			}
			first := acquire(owner)
			if err := store.AbortProvisioning(first); err != nil {
				t.Fatal(err)
			}
			if reset == "resize" {
				smaller := store
				smaller.SlotCount = 1
				if _, err := smaller.PrepareResize(); err != nil {
					t.Fatal(err)
				}
				if err := smaller.CommitResize(); err != nil {
					t.Fatal(err)
				}
			} else {
				facade.Store.Path = filepath.Join(recorder.StateDir, "replacement-leases.json")
			}
			second := acquire(other)
			if first.ResourceGeneration != second.ResourceGeneration || first.LeaseEpoch != second.LeaseEpoch {
				t.Fatal("fixture did not reproduce reused counters")
			}
			slot, err := storeSlot(facade.Store, second.SlotID)
			if err != nil {
				t.Fatal(err)
			}
			incident, err := recorder.SaveIncident(slot, errors.New("new holder failure"), map[string]string{
				"vm_1_allocation_verified": "true", "vm_1_console_log": "new holder private data",
			})
			if err != nil {
				t.Fatal(err)
			}
			events, err := recorder.readAgentEvents(owner, "slot-002", 50)
			if err != nil || len(events) != 2 {
				t.Fatalf("old holder read another lease: count=%d err=%v", len(events), err)
			}
			if _, err := recorder.readAgentIncident(owner, incident.IncidentID); err == nil {
				t.Fatal("old holder read new holder incident after counter reset")
			}
			if _, err := recorder.readAgentIncident(other, incident.IncidentID); err != nil {
				t.Fatal("new holder lost its own incident:", err)
			}
		})
	}
}

func TestAgentIncidentRequiresPhysicalVMOwnershipAtCollection(t *testing.T) {
	for _, scenario := range []string{"owned", "foreign-project", "foreign-vm", "generation", "epoch", "other-controller", "missing-owner"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Environment = &EnvironmentSpec{Name: EnvironmentPair, Count: 1, CPU: 4, Memory: "4GiB", Disk: "20GiB", Lifecycle: DisposableLifecycle}
			owner := strings.Repeat("a", 32)
			slot := LeaseSlot{SlotID: "slot-001", ResourceGeneration: 7, LeaseEpoch: 3, ClientID: owner, Environment: cfg.Environment}
			runner := &fakeRunner{}
			var initArgs []string
			runner.handler = func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				joined := strings.Join(args, " ")
				switch {
				case args[0] == "init":
					initArgs = args
					return nil, nil, nil
				case strings.HasPrefix(joined, "project get "):
					if scenario == "foreign-project" {
						return []byte("foreign"), nil, nil
					}
					return []byte(managedMarker), nil, nil
				case strings.HasPrefix(joined, "config get "):
					switch args[3] {
					case "user.subyard.managed":
						if scenario == "foreign-vm" {
							return []byte("foreign"), nil, nil
						}
						return []byte(managedMarker), nil, nil
					case "user.subyard.generation":
						if scenario == "generation" {
							return []byte("6"), nil, nil
						}
						return []byte("7"), nil, nil
					case "user.subyard.lease-epoch":
						if scenario == "epoch" {
							return []byte("2"), nil, nil
						}
						return []byte("3"), nil, nil
					case agentLogOwnerMarker:
						if scenario == "other-controller" {
							return []byte(capabilityDigest(strings.Repeat("b", 32))), nil, nil
						}
						if scenario == "missing-owner" {
							return nil, nil, nil
						}
						return []byte(capabilityDigest(owner)), nil, nil
					}
				}
				return []byte("VM log data"), nil, nil
			}
			runtime := &Runtime{Config: cfg, Runner: runner}
			runtime.useLeaseSlot(slot)
			if err := runtime.initVM(context.Background(), cfg.vm(1)); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(initArgs, " "), agentLogOwnerMarker+"="+capabilityDigest(owner)) {
				t.Fatal("native VM creation did not stamp diagnostic ownership")
			}
			diagnostics := runtime.recoveryDiagnostics(context.Background(), "")
			recorder := EventRecorder{StateDir: testkit.TempDir(t)}
			incident, err := recorder.SaveIncident(slot, errors.New("fixture failure"), diagnostics)
			if err != nil {
				t.Fatal(err)
			}
			own, err := recorder.readAgentIncident(owner, incident.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			if got := own.Diagnostics["vm_1_console_log"]; (got != "") != (scenario == "owned") {
				t.Fatalf("unverified VM log visibility: scenario=%s data=%q", scenario, got)
			}
			if incident.Diagnostics["vm_1_console_log"] == "" {
				t.Fatal("operator artifact lost native diagnostic evidence")
			}
		})
	}
}
