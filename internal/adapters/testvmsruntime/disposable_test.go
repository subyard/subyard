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
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFacadeDisposableAdmissionAndLegacyRequestRejection(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	var output bytes.Buffer
	facade := Facade{Store: store, Output: &output}
	for _, command := range []string{"acquire ignored", "acquire-v2 ignored", "acquire-v3 unknown ignored"} {
		if err := facade.Run(command); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
			t.Fatalf("unsupported request mutated state: %v", err)
		}
		output.Reset()
	}
	facade.OnAcquire = func(grant LeaseGrant, _ string) (LeaseGrant, error) {
		if grant.Environment == nil || grant.Environment.Name != EnvironmentAndroid || grant.Environment.Count != 1 {
			t.Fatalf("environment = %#v", grant.Environment)
		}
		if err := store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error { slot.Reserved = true; return nil }); err != nil {
			t.Fatal(err)
		}
		return grant, &CapacityError{Resource: "memory", Reason: "insufficient capacity"}
	}
	command := "acquire-v3 android-test client SHA256:key yard Project run tests " + fixturePublicKey(t) + " slot-001"
	if err := facade.Run(command); err != nil {
		t.Fatal(err)
	}
	var response facadeResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "capacity" || response.Reason != "memory" || response.Grant != nil {
		t.Fatalf("capacity result: %s", output.String())
	}
	slot, err := storeSlot(store, "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != SlotAvailable || slot.Environment != nil || slot.Reserved {
		t.Fatalf("admission leaked reservation: %#v", slot)
	}
}

func TestDisposableGenerationAndReservationRemainUntilVerifiedDrain(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	cfg := fixtureConfig(t)
	spec, err := cfg.environmentSpecForArch(EnvironmentAndroid, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "run", "tests", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.mutateOwned(first, func(slot *LeaseSlot, _ time.Time) error {
		slot.Reserved = true
		slot.BaseFingerprint = "base"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDrain(first); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(first.SlotID, errors.New("delete failed")); err != nil {
		t.Fatal(err)
	}
	slot, _ := storeSlot(store, first.SlotID)
	if slot.State != SlotQuarantined || !slot.Reserved || slot.BaseFingerprint != "base" || slot.Environment == nil {
		t.Fatalf("failed drain lost reservation/pin: %#v", slot)
	}
	if _, _, err := store.BeginScheduledRecovery(first.SlotID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishRecovery(first.SlotID, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	second, err := store.AcquireV3Slot(spec, "next", "SHA256:next", "yard", "Project", "next", "tests", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if second.ResourceGeneration <= first.ResourceGeneration || second.LeaseEpoch <= first.LeaseEpoch {
		t.Fatalf("reused allocation identity: first=%#v second=%#v", first, second)
	}
	if err := store.AbortProvisioning(first); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale abort = %v", err)
	}
	slot, _ = storeSlot(store, second.SlotID)
	if slot.State != SlotProvisioning || slot.LeaseID != second.LeaseID {
		t.Fatal("stale abort changed replacement")
	}
}

func TestDisposableDeletionRequiresEveryExactMarkerAndVerifiedStop(t *testing.T) {
	for _, mismatch := range []string{"", "generation", "epoch", "running", "inventory", "foreign", "delete"} {
		t.Run(mismatch, func(t *testing.T) {
			cfg := fixtureConfig(t)
			spec, _ := cfg.environmentSpecForArch(EnvironmentAndroid, "amd64")
			cfg = cfg.withEnvironment(spec)
			deleted := false
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				joined := strings.Join(args, " ")
				switch joined {
				case "project list --format csv -c n":
					return []byte(cfg.Project + "\n"), nil, nil
				case "project get " + cfg.Project + " user.subyard.managed":
					return []byte(managedMarker), nil, nil
				case "list --project " + cfg.Project + " -f csv -c n":
					if mismatch == "inventory" {
						return nil, nil, errors.New("inventory unavailable")
					}
					if mismatch == "foreign" {
						return []byte("foreign\n"), nil, nil
					}
					if deleted {
						return nil, nil, nil
					}
					return []byte(cfg.vm(1) + "\n"), nil, nil
				case "config get " + cfg.vm(1) + " user.subyard.managed --project " + cfg.Project:
					return []byte(managedMarker), nil, nil
				case "config get " + cfg.vm(1) + " user.subyard.generation --project " + cfg.Project:
					if mismatch == "generation" {
						return []byte("8"), nil, nil
					}
					return []byte("9"), nil, nil
				case "config get " + cfg.vm(1) + " user.subyard.lease-epoch --project " + cfg.Project:
					if mismatch == "epoch" {
						return []byte("2"), nil, nil
					}
					return []byte("3"), nil, nil
				case "list " + cfg.vm(1) + " --project " + cfg.Project + " -f csv -c s":
					if mismatch == "running" {
						return []byte("RUNNING"), nil, nil
					}
					return []byte("STOPPED"), nil, nil
				case "delete " + cfg.vm(1) + " --project " + cfg.Project:
					if mismatch == "delete" {
						return nil, nil, errors.New("delete failed")
					}
					deleted = true
					return nil, nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected call %s", joined)
			}}
			runtime := Runtime{Config: cfg, Runner: runner, allocation: &LeaseIdentity{SlotID: "slot-001", ResourceGeneration: 9, LeaseEpoch: 3}}
			err := runtime.deleteAllocation(context.Background())
			if (err == nil) != (mismatch == "") || deleted != (mismatch == "") {
				t.Fatalf("delete mismatch=%q deleted=%v err=%v", mismatch, deleted, err)
			}
		})
	}
}

func TestFutureLeaseSchemaCannotBeOverwrittenByRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	original := []byte(`{"schema_version":` + strconv.Itoa(LeaseSchemaVersion+1) + `,"resource_type":"agent-e2e","resource_id":"test-vms"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store := LeaseStore{Path: path, SlotCount: 1}
	if err := store.BeginRecovery("slot-001"); !errors.Is(err, ErrUnsupportedLeaseState) {
		t.Fatalf("future schema recovery = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("future schema overwritten")
	}
}

func TestRecoveryPreservesLegacyDataUntilExplicitRetirement(t *testing.T) {
	cfg := fixtureConfig(t)
	runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		return nil, nil, fmt.Errorf("unexpected physical action %s", strings.Join(args, " "))
	}}
	runtime := Runtime{Config: cfg, Runner: runner}
	slot := LeaseSlot{SlotID: "slot-001", ResourceGeneration: 7, State: SlotQuarantined, LegacyRetained: true}
	if err := runtime.rebuildSlotPair(context.Background(), slot); !errors.Is(err, ErrLegacyRetained) {
		t.Fatalf("legacy recovery error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("automatic recovery touched retained data: %#v", runner.calls)
	}
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	if err := os.WriteFile(store.Path, []byte(`{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":0,"state":"available"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	stale := LeaseIdentity{SlotID: "slot-001", ResourceGeneration: 6, LeaseEpoch: 0}
	if err := runtime.RetireLegacySlot(context.Background(), store, stale); !errors.Is(err, ErrLeaseTargetStale) {
		t.Fatalf("stale retirement error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("stale retirement touched retained data")
	}
}
