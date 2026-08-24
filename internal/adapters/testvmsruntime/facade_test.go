package testvmsruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFacadeContractAndRedaction(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	var output bytes.Buffer
	facade := Facade{Store: store, Output: &output}
	key := strings.Fields(fixturePublicKey(t))
	command := "acquire-v2 client SHA256:key default Subyard-2 run-a tests " +
		key[0] + " " + key[1] + " slot-001"
	if err := facade.Run(command); err != nil {
		t.Fatal(err)
	}
	var acquired facadeResponse
	if err := json.Unmarshal(output.Bytes(), &acquired); err != nil {
		t.Fatal(err)
	}
	if acquired.Grant == nil || acquired.Grant.Capability == "" ||
		acquired.Grant.Context == nil ||
		acquired.Grant.Context.Yard != "default" ||
		acquired.Grant.Context.Project != "Subyard-2" ||
		acquired.Grant.Context.Checkout != "" ||
		acquired.Grant.Context.Run != "run-a" ||
		acquired.Grant.Context.Purpose != "tests" {
		t.Fatalf("missing attributed grant: %#v", acquired)
	}
	if err := store.Quarantine(
		*acquired.Grant, errors.New("fixture failed at /home/dev/private"),
	); err != nil {
		t.Fatal(err)
	}
	failureID := "00000000000000000001-0123456789abcdef"
	incidentID := "00000000000000000002-fedcba9876543210"
	if err := store.SetQuarantineIncident(
		acquired.Grant.SlotID,
		failureID,
		incidentID,
	); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := facade.Run("status"); err != nil {
		t.Fatal(err)
	}
	status := output.String()
	for _, secret := range []string{
		acquired.Grant.Capability, acquired.Grant.LeaseID, "client",
		"SHA256:key", "/home/dev/private",
	} {
		if strings.Contains(status, secret) {
			t.Fatalf("status disclosed %q: %s", secret, status)
		}
	}
	for _, attribution := range []string{
		`"yard":"default"`, `"project":"Subyard-2"`,
		`"run":"run-a"`, `"purpose":"tests"`,
		`"last_failure_event_id":"` + failureID + `"`,
		`"incident_id":"` + incidentID + `"`,
		`"recovery_attempt":0`,
		`"next_recovery_at":`,
	} {
		if !strings.Contains(status, attribution) {
			t.Fatalf("status omitted %s: %s", attribution, status)
		}
	}
	output.Reset()
	if err := facade.Run(command); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"busy"`) {
		t.Fatalf("busy response=%s", output.String())
	}
}

func TestFacadeAcquireRequiresExactSlotWithoutMutatingAnyConfiguredPool(t *testing.T) {
	key := strings.Fields(fixturePublicKey(t))
	for _, slotCount := range []int{1, 2, 3} {
		t.Run("slots="+strconv.Itoa(slotCount), func(t *testing.T) {
			for _, command := range []string{
				"acquire-v2 client SHA256:key default Subyard-2 run-a tests " + key[0] + " " + key[1],
			} {
				name := strings.Fields(command)[0]
				t.Run(name+"/absent", func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "missing", "leases.json")
					var output bytes.Buffer
					facade := Facade{
						Store: LeaseStore{Path: path, SlotCount: slotCount}, Output: &output,
					}
					if err := facade.Run(command); err != nil {
						t.Fatal(err)
					}
					assertMissingSlotResponse(t, output.Bytes())
					for _, artifact := range []string{path, path + ".lock"} {
						if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("slotless acquire created %s: %v", artifact, err)
						}
					}
				})

				t.Run(name+"/existing", func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "leases.json")
					store := LeaseStore{Path: path, SlotCount: slotCount}
					slots := make([]LeaseSlot, slotCount)
					for index := range slots {
						slots[index] = LeaseSlot{
							SlotID:             fmt.Sprintf("slot-%03d", index+1),
							ResourceGeneration: 1,
							State:              SlotAvailable,
						}
					}
					fixture, err := json.Marshal(LeasePool{
						SchemaVersion: LeaseSchemaVersion,
						ResourceType:  "agent-e2e",
						ResourceID:    "test-vms",
						Slots:         slots,
					})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, fixture, 0o600); err != nil {
						t.Fatal(err)
					}
					before, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var output bytes.Buffer
					facade := Facade{Store: store, Output: &output}
					if err := facade.Run(command); err != nil {
						t.Fatal(err)
					}
					assertMissingSlotResponse(t, output.Bytes())
					after, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(after, before) {
						t.Fatalf("slotless acquire rewrote lease state: before=%q after=%q", before, after)
					}
					if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("slotless acquire created lock: %v", err)
					}
				})
			}
		})
	}
}

func TestFacadeRejectsLegacyAcquireWithoutTouchingLeaseState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "leases.json")
	key := strings.Fields(fixturePublicKey(t))
	var output bytes.Buffer
	facade := Facade{
		Store: LeaseStore{Path: path, SlotCount: 2}, Output: &output,
	}
	command := "acquire client SHA256:key Subyard-2+run-a tests " +
		key[0] + " " + key[1] + " slot-002"
	if err := facade.Run(command); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"invalid_request"`) ||
		!strings.Contains(output.String(), `"reason":"unsupported_acquire"`) {
		t.Fatalf("legacy acquire response=%s", output.String())
	}
	for _, artifact := range []string{path, path + ".lock"} {
		if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy acquire created %s: %v", artifact, err)
		}
	}
}

func assertMissingSlotResponse(t *testing.T, payload []byte) {
	t.Helper()
	var response struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "invalid_request" || response.Reason != "missing_slot_id" {
		t.Fatalf("slotless response=%s", payload)
	}
}

func TestFacadeExactSlotAcquireDoesNotFallback(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
	var output bytes.Buffer
	var provisioned []string
	facade := Facade{
		Store: store, Output: &output,
		OnAcquire: func(grant LeaseGrant, _ string) (LeaseGrant, error) {
			provisioned = append(provisioned, grant.SlotID)
			expires, err := store.MarkHeld(grant)
			grant.ExpiresAt = expires
			return grant, err
		},
	}
	key := strings.Fields(fixturePublicKey(t))
	base := "acquire-v2 client SHA256:key default Subyard-2 run-a tests " +
		key[0] + " " + key[1]
	if err := facade.Run(base + " slot-002"); err != nil {
		t.Fatal(err)
	}
	var exact facadeResponse
	if err := json.Unmarshal(output.Bytes(), &exact); err != nil {
		t.Fatal(err)
	}
	if exact.Grant == nil || exact.Grant.SlotID != "slot-002" ||
		len(provisioned) != 1 || provisioned[0] != "slot-002" {
		t.Fatalf("exact acquire response=%s provisioned=%v", output.String(), provisioned)
	}
	output.Reset()
	if err := facade.Run(base + " slot-002"); err != nil {
		t.Fatal(err)
	}
	var occupied struct {
		Code   string              `json:"code"`
		State  string              `json:"state"`
		Reason string              `json:"reason"`
		Owner  *LeaseOwnerSnapshot `json:"owner"`
	}
	if err := json.Unmarshal(output.Bytes(), &occupied); err != nil {
		t.Fatal(err)
	}
	if occupied.Code != "busy" || occupied.State != string(SlotHeld) ||
		occupied.Reason != "busy" || occupied.Owner == nil ||
		occupied.Owner.Yard != "default" || occupied.Owner.Project != "Subyard-2" ||
		occupied.Owner.Run != "run-a" || occupied.Owner.Purpose != "tests" ||
		occupied.Owner.DisplayLabel != "Subyard-2#run-a" ||
		occupied.Owner.AcquiredAt.IsZero() || occupied.Owner.ExpiresAt.IsZero() ||
		len(provisioned) != 1 {
		t.Fatalf("occupied exact response=%s provisioned=%v", output.String(), provisioned)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	held := pool.Slots[1]
	if held.Yard != occupied.Owner.Yard || held.Project != occupied.Owner.Project ||
		held.Run != occupied.Owner.Run || held.Purpose != occupied.Owner.Purpose ||
		held.DisplayLabel != occupied.Owner.DisplayLabel ||
		!held.AcquiredAt.Equal(occupied.Owner.AcquiredAt) ||
		!held.ExpiresAt.Equal(occupied.Owner.ExpiresAt) {
		t.Fatalf("held status and busy owner diverged: slot=%#v owner=%#v", held, occupied.Owner)
	}
	output.Reset()
	if err := facade.Run(base); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"invalid_request"`) ||
		!strings.Contains(output.String(), `"reason":"missing_slot_id"`) || len(provisioned) != 1 {
		t.Fatalf("slotless acquire response=%s provisioned=%v", output.String(), provisioned)
	}
	beforeInvalid, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := facade.Run(base + " slot-2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"invalid_request"`) ||
		!strings.Contains(output.String(), `"reason":"invalid_slot"`) {
		t.Fatalf("non-canonical slot response=%s", output.String())
	}
	afterInvalid, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterInvalid, beforeInvalid) {
		t.Fatalf("invalid exact acquire mutated pool: before=%#v after=%#v", beforeInvalid, afterInvalid)
	}
}

func TestFacadeExactUnavailableResponseIsTypedAndRedacted(t *testing.T) {
	key := strings.Fields(fixturePublicKey(t))
	for _, test := range []struct {
		state  SlotState
		reason string
		holder bool
	}{
		{state: SlotProvisioning, reason: "provisioning"},
		{state: SlotHeld, reason: "busy", holder: true},
		{state: SlotDraining, reason: "draining"},
		{state: SlotQuarantined, reason: "quarantined"},
		{state: SlotRecovering, reason: "recovering"},
		{state: SlotUnavailable, reason: "unavailable"},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			heldAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
			store := LeaseStore{
				Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2,
				Now: func() time.Time { return heldAt.Add(time.Minute) },
			}
			target := LeaseSlot{
				SlotID: "slot-002", ResourceGeneration: 13, LeaseEpoch: 17, State: test.state,
				FailureReason: "fixture failure at /private/secret",
			}
			if test.holder {
				target.ClientID = "private-client-id"
				target.ControllerFingerprint = "SHA256:private-controller"
				target.DisplayLabel = "Subyard-2#run-a"
				target.Yard = "test-yard"
				target.Project = "Subyard-2"
				target.Run = "run-a"
				target.Purpose = "unit-tests"
				target.LeaseID = "private-lease-id"
				target.CapabilityHash = "private-capability-hash"
				target.AcquiredAt = heldAt
				target.ExpiresAt = heldAt.Add(10 * time.Minute)
			}
			pool := LeasePool{
				SchemaVersion: LeaseSchemaVersion, ResourceType: "agent-e2e", ResourceID: "test-vms",
				Slots: []LeaseSlot{
					{SlotID: "slot-001", ResourceGeneration: 7, LeaseEpoch: 11, State: SlotAvailable},
					target,
				},
			}
			payload, err := json.Marshal(pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.Path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := store.Status()
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			facade := Facade{Store: store, Output: &output}
			command := "acquire-v2 client SHA256:key test-yard Subyard-2 run-b unit-tests " +
				key[0] + " " + key[1] + " slot-002"
			if err := facade.Run(command); err != nil {
				t.Fatal(err)
			}
			var response struct {
				Code   string `json:"code"`
				State  string `json:"state"`
				Reason string `json:"reason"`
				Owner  *struct {
					DisplayLabel string    `json:"display_label"`
					Yard         string    `json:"yard"`
					Project      string    `json:"project"`
					Run          string    `json:"run"`
					Purpose      string    `json:"purpose"`
					AcquiredAt   time.Time `json:"acquired_at"`
					ExpiresAt    time.Time `json:"expires_at"`
				} `json:"owner"`
			}
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != "busy" || response.State != string(test.state) ||
				response.Reason != test.reason {
				t.Fatalf("exact unavailable response=%s", output.String())
			}
			if test.holder {
				if response.Owner == nil || response.Owner.DisplayLabel != "Subyard-2#run-a" ||
					response.Owner.Yard != "test-yard" || response.Owner.Project != "Subyard-2" ||
					response.Owner.Run != "run-a" || response.Owner.Purpose != "unit-tests" ||
					!response.Owner.AcquiredAt.Equal(heldAt) ||
					!response.Owner.ExpiresAt.Equal(heldAt.Add(10*time.Minute)) {
					t.Fatalf("held exact response lost holder attribution: %s", output.String())
				}
				var raw struct {
					Owner map[string]json.RawMessage `json:"owner"`
				}
				if err := json.Unmarshal(output.Bytes(), &raw); err != nil {
					t.Fatal(err)
				}
				wantKeys := map[string]bool{
					"display_label": true, "yard": true, "project": true, "run": true,
					"purpose": true, "acquired_at": true, "expires_at": true,
				}
				if len(raw.Owner) != len(wantKeys) {
					t.Fatalf("held exact owner keys=%v, want=%v", raw.Owner, wantKeys)
				}
				for key := range raw.Owner {
					if !wantKeys[key] {
						t.Fatalf("held exact owner disclosed unexpected field %q: %s", key, output.String())
					}
				}
			} else if response.Owner != nil {
				t.Fatalf("non-held exact response disclosed holder attribution: %s", output.String())
			}
			for _, secret := range []string{
				"/private/secret", "private-client-id", "private-controller",
				"private-lease-id", "private-capability-hash",
			} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("exact unavailable response disclosed %q: %s", secret, output.String())
				}
			}
			after, err := store.Status()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("failed exact acquire mutated pool: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestFacadeAdvertisesAndAcceptsAttributionV2(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
	var output bytes.Buffer
	facade := Facade{Store: store, Output: &output}
	if err := facade.Run("status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"capabilities":["attribution-v2"]`) {
		t.Fatalf("status omitted attribution capability: %s", output.String())
	}
	output.Reset()
	key := strings.Fields(fixturePublicKey(t))
	command := strings.Join([]string{
		"acquire-v2", "client", "SHA256:key", "default", "Subyard-2",
		"run-a", "tests", key[0], key[1], "slot-002",
	}, " ")
	if err := facade.Run(command); err != nil {
		t.Fatal(err)
	}
	var response facadeResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Grant == nil || response.Grant.SlotID != "slot-002" ||
		response.Grant.Context == nil || response.Grant.Context.SchemaVersion != 2 ||
		response.Grant.Context.Yard != "default" ||
		response.Grant.Context.Project != "Subyard-2" ||
		response.Grant.Context.Checkout != "" {
		t.Fatalf("v2 response = %s", output.String())
	}
}

func TestFacadeReleaseReplayAndWrongCredentialsReturnLeaseLost(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	var output bytes.Buffer
	facade := Facade{
		Store: store, Output: &output,
		OnAcquire: func(grant LeaseGrant, _ string) (LeaseGrant, error) {
			expires, err := store.MarkHeld(grant)
			grant.ExpiresAt = expires
			return grant, err
		},
		OnRelease: func(grant LeaseGrant) error {
			if err := store.BeginDrain(grant); err != nil {
				return err
			}
			return store.FinishDrain(grant.SlotID, nil)
		},
	}
	key := strings.Fields(fixturePublicKey(t))
	if err := facade.Run("acquire-v2 client SHA256:key default Subyard-2 run-a tests " +
		key[0] + " " + key[1] + " slot-001"); err != nil {
		t.Fatal(err)
	}
	var acquired facadeResponse
	if err := json.Unmarshal(output.Bytes(), &acquired); err != nil {
		t.Fatal(err)
	}
	grant := acquired.Grant
	command := func(capability string) string {
		return strings.Join([]string{
			"release", grant.SlotID, grant.LeaseID,
			strconv.FormatUint(grant.LeaseEpoch, 10), capability,
		}, " ")
	}
	output.Reset()
	if err := facade.Run(command("wrong")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"lease_lost"`) {
		t.Fatalf("wrong credential response = %s", output.String())
	}
	output.Reset()
	if err := facade.Run(command(grant.Capability)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"message":"released"`) {
		t.Fatalf("release response = %s", output.String())
	}
	output.Reset()
	if err := facade.Run(command(grant.Capability)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"lease_lost"`) {
		t.Fatalf("replayed release response = %s", output.String())
	}
}

func TestFacadeRejectsUnboundedInput(t *testing.T) {
	var output bytes.Buffer
	path := filepath.Join(t.TempDir(), "leases.json")
	facade := Facade{
		Store:  LeaseStore{Path: path, SlotCount: 1},
		Output: &output,
	}
	if err := facade.Run("status arbitrary"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"invalid_request"`) {
		t.Fatalf("response=%s", output.String())
	}
	output.Reset()
	key := strings.Fields(fixturePublicKey(t))
	if err := facade.Run(
		"acquire-v2 client SHA256:key /home/dev/private Subyard-2 run-a tests " +
			key[0] + " " + key[1] + " slot-001",
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"invalid_request"`) {
		t.Fatalf("private-path response=%s", output.String())
	}
}
