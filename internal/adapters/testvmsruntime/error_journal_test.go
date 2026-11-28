package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func (f *brokerPoolFixture) acquireAs(index int, client string) LeaseGrant {
	f.t.Helper()
	key := strings.Fields(fixturePublicKey(f.t))
	var result facadeResponse
	f.request(fmt.Sprintf("acquire-v3 subyard-pair %s SHA256:controller test-yard Journal-Owner run%05d private-purpose %s %s slot-%03d",
		client, index, key[0], key[1], index), &result)
	if result.Status != "ok" || result.Grant == nil {
		f.t.Fatalf("acquire slot %d: status=%s code=%s message=%s", index, result.Status, result.Code, result.Message)
	}
	return *result.Grant
}

func (f *brokerPoolFixture) journal(command string) ([]JournalRecord, string) {
	f.t.Helper()
	var response facadeResponse
	raw := f.request(command, &response)
	if response.Status != "ok" || response.Errors == nil {
		f.t.Fatalf("%s: status=%s code=%s message=%s", command, response.Status, response.Code, response.Message)
	}
	return *response.Errors, string(raw)
}

func journalKinds(records []JournalRecord) []string {
	kinds := make([]string, 0, len(records))
	for _, record := range records {
		kinds = append(kinds, record.Kind)
	}
	return kinds
}

// Every controller sees which slot failed, at which step and with which
// incident; only the owner learns the records are its own. The journal and the
// owner's incident outlive the slot's recovery, its retirement and sink pruning.
func TestErrorJournalReportsPoolFailuresWithoutAttributionAndOutlivesTheSlot(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	owner, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f.acquireAs(1, other)
	victim := f.acquireAs(2, owner)
	project := f.rt.slotRuntime(2, "").Config.Project
	f.incus.fault = []string{"delete", "e2e-vm-1", "--project", project}
	if result := f.leaseAction("release", victim); result.Code != "quarantined" {
		t.Fatalf("release = %#v, want quarantine", result)
	}
	quarantined := f.slot(2)

	records, raw := f.journal("errors-v1 " + owner + " - 50")
	if got := journalKinds(records); !slices.Equal(got, []string{"lease.stop_failed", "slot.quarantined"}) {
		t.Fatalf("journal kinds = %v", got)
	}
	for _, record := range records {
		if record.SlotID != "slot-002" || record.LeaseEpoch != victim.LeaseEpoch || !record.Own ||
			record.Step != "delete-allocation" || record.Outcome != "error" {
			t.Errorf("record = %+v", record)
		}
	}
	if records[1].IncidentID != quarantined.IncidentID || records[1].IncidentID == "" {
		t.Errorf("quarantine record incident = %q, slot incident %q", records[1].IncidentID, quarantined.IncidentID)
	}
	for _, private := range []string{owner, other, victim.LeaseID, victim.Capability, "Journal-Owner",
		"test-yard", "run00002", "private-purpose", fakeIncusUnavailable, "context", "\"error\":"} {
		if strings.Contains(raw, private) {
			t.Errorf("journal discloses %q: %s", private, raw)
		}
	}
	for _, reader := range []string{"-", other} {
		records, _ := f.journal("errors-v1 " + reader + " slot-002 50")
		if len(records) != 2 || records[0].Own || records[1].Own {
			t.Errorf("reader %s: %+v", reader, records)
		}
	}
	if records, _ := f.journal("errors-v1 - slot-001 50"); len(records) != 0 {
		t.Errorf("healthy slot has records: %+v", records)
	}

	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))
	if state := f.slot(2).State; state != SlotAvailable {
		t.Fatalf("recovery left slot %s", state)
	}
	// Retiring a recovered slot that still names its last incident.
	brokerMust(t, f.resize(1))
	f.assertRetired(2)
	brokerMust(t, f.rt.Events.Ack(nil, nil))

	records, _ = f.journal("errors-v1 " + owner + " slot-002 50")
	if len(records) != 2 || !records[1].Own || records[1].IncidentID != quarantined.IncidentID {
		t.Fatalf("journal after the slot disappeared: %+v", records)
	}
	var incident facadeResponse
	f.request("incident-v1 "+owner+" "+quarantined.IncidentID, &incident)
	if incident.Incident == nil || incident.Incident.SlotID != "slot-002" {
		t.Fatalf("own incident after the slot disappeared: %#v", incident)
	}
	if got := incident.Incident.Diagnostics["vm_1_guest_journal"]; got != "unavailable: guest is not running" {
		t.Errorf("stopped guest snapshot = %q", got)
	}
}

func TestErrorJournalFacadeValidatesSelectionAndAdvertisesCapability(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	var status facadeResponse
	f.request("status", &status)
	if !slices.Contains(status.Capabilities, ErrorJournalCapability) {
		t.Fatalf("capabilities = %v", status.Capabilities)
	}
	for _, command := range []string{
		"errors-v1", "errors-v1 - - 50 extra", "errors-v1 nothex - 50", "errors-v1 - slot-1 50",
		"errors-v1 - - 0", "errors-v1 - - 201", "errors-v1 - - many",
	} {
		var response facadeResponse
		f.request(command, &response)
		if response.Status != "error" || response.Code != "invalid_request" || response.Errors != nil {
			t.Errorf("%q = %#v", command, response)
		}
	}
	if records, _ := f.journal("errors-v1 - - 200"); len(records) != 0 {
		t.Errorf("empty pool journal = %+v", records)
	}
}

// The journal keeps typed facts only, rejects unknown vocabulary and stays
// bounded without sink acknowledgement.
func TestErrorJournalProjectionAndBounds(t *testing.T) {
	root := testkit.TempDir(t)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	recorder := EventRecorder{StateDir: root, Source: "fixture", Now: func() time.Time { return now }}
	record := func(event BrokerEvent) {
		t.Helper()
		if _, err := recorder.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	record(BrokerEvent{Kind: "lease.stop_succeeded", SlotID: "slot-001", DurationMS: 29999})
	record(BrokerEvent{Kind: "lease.stop_succeeded", SlotID: "slot-001", DurationMS: 64800})
	record(BrokerEvent{Kind: "lease.available", SlotID: "slot-001"})
	record(BrokerEvent{Kind: "reaper.fatal", Error: "secret=private"})
	if _, err := recorder.Record(BrokerEvent{Kind: "incus.slow", Operation: "exec"}); err == nil {
		t.Fatal("unknown operation was recorded")
	}
	records, err := recorder.readJournal("-", "-", 50)
	brokerMust(t, err)
	if len(records) != 2 || records[0].Kind != "lease.stop_succeeded" || records[0].Outcome != "slow" ||
		records[0].DurationMS != 64800 || records[1].Kind != "reaper.fatal" || records[1].Outcome != "error" {
		t.Fatalf("records = %+v", records)
	}
	now = now.Add(agentLogRetention)
	if records, err := recorder.readJournal("-", "-", 50); err != nil || len(records) != 0 {
		t.Fatalf("expired records = %+v, %v", records, err)
	}

	directory := recorder.journalDirectory()
	for index := range maxJournalRecords + 2 {
		id := fmt.Sprintf("%020d-%016x", index+1, index)
		payload, err := json.Marshal(JournalRecord{SchemaVersion: JournalSchemaVersion, EventID: id,
			Timestamp: now, Sequence: uint64(index + 1), Kind: "reaper.fatal", Outcome: "error"})
		brokerMust(t, err)
		testkit.WriteFile(t, filepath.Join(directory, id+".json"), payload, 0o600)
	}
	record(BrokerEvent{Kind: "reaper.fatal"})
	entries, err := os.ReadDir(directory)
	brokerMust(t, err)
	if len(entries) != maxJournalRecords {
		t.Fatalf("journal holds %d records, want %d", len(entries), maxJournalRecords)
	}
	if _, err := os.Lstat(filepath.Join(directory, fmt.Sprintf("%020d-%016x", 1, 0)+".json")); !os.IsNotExist(err) {
		t.Errorf("oldest record survived trimming: %v", err)
	}
	if records, err := recorder.readJournal("-", "-", maxJournalReadEvents); err != nil || len(records) != maxJournalReadEvents {
		t.Fatalf("bounded read = %d records, %v", len(records), err)
	}
}

func TestSlowIncusCallsAreRecordedWhileRunningAndOnReturn(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	child := f.rt.slotRuntime(1, "")
	var timers []func()
	var mutex sync.Mutex
	child.slowIncusAfter = func(_ time.Duration, callback func()) func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		timers = append(timers, callback)
		return func() bool { return false }
	}
	handler := f.incus.handler
	f.incus.handler = func(name string, args, env []string, input io.Reader) ([]byte, []byte, error) {
		// The call is still running when its threshold expires.
		mutex.Lock()
		pending := timers
		timers = nil
		mutex.Unlock()
		for _, callback := range pending {
			callback()
		}
		return handler(name, args, env, input)
	}
	brokerMust(t, child.ensureProject(f.ctx))
	records, err := f.rt.Events.readJournal("-", "-", 50)
	brokerMust(t, err)
	var running, returned int
	for _, record := range records {
		if record.Kind != "incus.slow" || record.SlotID != "slot-001" || record.Operation == "" {
			t.Fatalf("record = %+v", record)
		}
		if record.DurationMS >= slowIncusRead.Milliseconds() {
			running++
		} else {
			returned++
		}
	}
	// One running record per operation per minute, each followed by its return.
	if running == 0 || running != returned {
		t.Fatalf("running=%d returned=%d records=%+v", running, returned, records)
	}
	operations := map[string]int{}
	for _, record := range records {
		if record.DurationMS >= slowIncusRead.Milliseconds() {
			operations[record.Operation]++
		}
	}
	for operation, count := range operations {
		if count != 1 {
			t.Errorf("operation %s recorded %d running records within one minute", operation, count)
		}
	}
}

func TestSlowIncusRecordingNeverChangesTheCall(t *testing.T) {
	rt := &Runtime{Config: Config{Incus: "incus", Project: "pool-slot-1"}, Runner: newFakeIncus(t)}
	rt.prepareDefaults() // No state directory: every record fails.
	rt.slowIncusAfter = func(_ time.Duration, callback func()) func() bool {
		callback()
		return func() bool { return false }
	}
	if _, err := rt.incus(t.Context(), "project", "list", "--format", "csv", "-c", "n"); err != nil {
		t.Fatalf("recording failure changed the call: %v", err)
	}
	if _, err := os.Lstat("spool"); !os.IsNotExist(err) {
		t.Fatal("recording without a state directory created a relative spool")
	}
}

// Failed release steps keep their typed step and outcome in the pool journal.
func TestErrorJournalClassifiesReleaseFailures(t *testing.T) {
	for _, scenario := range []struct {
		name, fault, step, outcome string
	}{
		{"graceful stop timeout", "stop", "fence-and-stop", "timeout"},
		{"delete failure", "delete", "delete-allocation", "error"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newBrokerPoolFixture(t, 1)
			grant := f.acquire(1, "classified")
			project := f.rt.slotRuntime(1, "").Config.Project
			handler := f.incus.handler
			f.incus.handler = func(name string, args, env []string, input io.Reader) ([]byte, []byte, error) {
				if name == "incus" && len(args) > 0 && args[0] == "stop" {
					f.now = f.now.Add(61 * time.Second)
				}
				return handler(name, args, env, input)
			}
			if scenario.fault == "stop" {
				f.incus.fault = []string{"stop", "e2e-vm-1", "--project", project, "--timeout", "60"}
			} else {
				f.incus.fault = []string{"delete", "e2e-vm-1", "--project", project}
			}
			if result := f.leaseAction("release", grant); result.Code != "quarantined" {
				t.Fatalf("release = %#v", result)
			}
			records, _ := f.journal("errors-v1 - - 50")
			if got := journalKinds(records); !slices.Equal(got, []string{"lease.stop_failed", "slot.quarantined"}) {
				t.Fatalf("kinds = %v", got)
			}
			for _, record := range records {
				if record.Step != scenario.step || record.Outcome != scenario.outcome {
					t.Errorf("record = %+v, want step %s outcome %s", record, scenario.step, scenario.outcome)
				}
			}
			incident, err := os.ReadFile(filepath.Join(f.rt.Events.incidentDirectory(), records[1].IncidentID+".json"))
			brokerMust(t, err)
			var artifact IncidentArtifact
			brokerMust(t, json.Unmarshal(incident, &artifact))
			// A guest left running by the failed stop answers through its agent.
			wantJobs := "unavailable: guest is not running"
			if scenario.fault == "stop" {
				wantJobs = "JOB UNIT TYPE STATE\n42 fixture-stop.service stop running\n"
			}
			if got := artifact.Diagnostics["vm_1_guest_jobs"]; got != wantJobs {
				t.Errorf("guest jobs = %q, want %q", got, wantJobs)
			}
		})
	}
}

func TestGuestSnapshotIsBoundedRedactedAndOwnerOnly(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	owner := strings.Repeat("c", 32)
	f.acquireAs(1, owner)
	child := f.rt.slotRuntime(1, "")
	child.useLeaseSlot(f.slot(1))
	handler := f.incus.handler
	journalCalls := 0
	f.incus.handler = func(name string, args, env []string, input io.Reader) ([]byte, []byte, error) {
		if name == "incus" && slices.Contains(args, "journalctl") {
			journalCalls++
			if slices.Contains(args, "e2e-vm-2") {
				return nil, nil, fmt.Errorf("Error: VM agent isn't currently running")
			}
			return []byte("boot ok\n-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n"), nil, nil
		}
		return handler(name, args, env, input)
	}
	snapshot := child.guestSnapshot(f.ctx, "e2e-vm-2")
	if journalCalls != 2 || !strings.HasPrefix(snapshot["vm_2_guest_journal"], "unavailable: guest agent not responding") ||
		!strings.Contains(snapshot["vm_1_guest_journal"], "boot ok") {
		t.Fatalf("snapshot = %#v (journal calls %d)", snapshot, journalCalls)
	}
	slot := f.slot(1)
	diagnostics := map[string]string{"vm_1_allocation_verified": "true"}
	for name, value := range snapshot {
		diagnostics[name] = value
	}
	artifact, err := f.rt.Events.SaveIncident(slot, fmt.Errorf("fixture"), diagnostics)
	brokerMust(t, err)
	if strings.Contains(artifact.Diagnostics["vm_1_guest_journal"], "secret") {
		t.Fatal("guest journal kept a private key")
	}
	var own, other facadeResponse
	f.request("incident-v1 "+owner+" "+artifact.IncidentID, &own)
	f.request("incident-v1 "+strings.Repeat("d", 32)+" "+artifact.IncidentID, &other)
	if own.Incident == nil || own.Incident.Diagnostics["vm_1_guest_journal"] == "" ||
		own.Incident.Diagnostics["vm_2_guest_journal"] != "" || other.Incident != nil {
		t.Fatalf("owner-only exposure: own=%#v other=%#v", own.Incident, other)
	}

	// A hung guest agent cannot hold quarantine beyond the snapshot budget.
	f.incus.handler = func(name string, args, env []string, input io.Reader) ([]byte, []byte, error) {
		if name == "incus" && len(args) > 0 && args[0] == "exec" && slices.Contains(args, "systemctl") {
			time.Sleep(guestSnapshotBudget + time.Second)
			return nil, nil, context.DeadlineExceeded
		}
		return handler(name, args, env, input)
	}
	started := time.Now()
	snapshot = child.guestSnapshot(f.ctx, "")
	if elapsed := time.Since(started); elapsed > 2*guestSnapshotBudget+3*time.Second {
		t.Fatalf("snapshot took %s", elapsed)
	}
	if !strings.HasPrefix(snapshot["vm_2_guest_jobs"], "unavailable: guest snapshot budget exhausted") {
		t.Fatalf("snapshot after budget = %#v", snapshot)
	}
}

func TestRecoveryJournalFromOlderBrokerWithRetiringSlotLoads(t *testing.T) {
	root := testkit.TempDir(t)
	store := LeaseStore{Path: filepath.Join(root, "leases.json"), SlotCount: 2, Now: time.Now}
	_, err := store.Status()
	brokerMust(t, err)
	testkit.WriteFile(t, store.recoveryJournalPath(), []byte(`{"schema_version":1,"slots":[{"slot_id":"slot-002","resource_generation":2,"lease_epoch":1,"state":"draining","incident_id":"01767225600000000000-38d2a5c89e81b2a6"}]}`), 0o600)
	if _, err := store.Status(); err != nil {
		t.Fatalf("older recovery journal: %v", err)
	}
}

// A slow call made for a lease is the holder's own record.
func TestSlowIncusRecordsOfALeaseAreTheHoldersOwn(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	owner := strings.Repeat("d", 32)
	f.acquireAs(1, owner)
	child := f.rt.slotRuntime(1, "")
	child.useLeaseSlot(f.slot(1))
	child.recordSlowIncus("stop", slowIncusLifecycle, "", true)
	for reader, own := range map[string]bool{owner: true, "-": false, strings.Repeat("e", 32): false} {
		records, _ := f.journal("errors-v1 " + reader + " slot-001 50")
		if len(records) != 1 || records[0].Kind != "incus.slow" || records[0].Own != own ||
			records[0].LeaseEpoch != f.slot(1).LeaseEpoch {
			t.Errorf("reader %s: %+v", reader, records)
		}
	}
}

func TestReadinessBudgetExpiryIsATimeout(t *testing.T) {
	rt := &Runtime{Config: Config{BootTimeout: 50 * time.Millisecond}, Now: time.Now}
	rt.prepareDefaults()
	err := rt.waitFor(t.Context(), "fixture readiness", func(context.Context) error { return errors.New("not ready") })
	if failureStep(err) != "readiness" || failureOutcome(err) != "timeout" {
		t.Fatalf("step=%s outcome=%s err=%v", failureStep(err), failureOutcome(err), err)
	}
	unobserved := &readinessFailure{Label: "fixture", LastProbe: "error_kind=not_observed"}
	if failureOutcome(unobserved) != "error" {
		t.Fatalf("unobserved readiness outcome = %s", failureOutcome(unobserved))
	}
}

// Legacy and corrupt-state slots have no allocation identity: their guests are
// never asked for jobs or journals.
func TestGuestSnapshotRequiresAnAllocationIdentity(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	f.acquire(1, "legacy")
	child := f.rt.slotRuntime(1, "")
	f.incus.calls = nil
	snapshot := child.guestSnapshot(f.ctx, "")
	for _, section := range []string{"vm_1_guest_jobs", "vm_1_guest_journal"} {
		if snapshot[section] != "unavailable: allocation ownership not verified" {
			t.Errorf("%s = %q", section, snapshot[section])
		}
	}
	for _, call := range f.incus.calls {
		if slices.Contains(call, "exec") {
			t.Fatalf("guest command without allocation identity: %v", call)
		}
	}
}
