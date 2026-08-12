package testvmsruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFacadeContractAndRedaction(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	var output bytes.Buffer
	facade := Facade{Store: store, Output: &output}
	key := strings.Fields(fixturePublicKey(t))
	command := "acquire client SHA256:key Subyard/Subyard@checkout-a#run-a tests " +
		key[0] + " " + key[1]
	if err := facade.Run(command); err != nil {
		t.Fatal(err)
	}
	var acquired facadeResponse
	if err := json.Unmarshal(output.Bytes(), &acquired); err != nil {
		t.Fatal(err)
	}
	if acquired.Grant == nil || acquired.Grant.Capability == "" ||
		acquired.Grant.Context == nil ||
		acquired.Grant.Context.Project != "Subyard/Subyard" ||
		acquired.Grant.Context.Checkout != "checkout-a" ||
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
		`"project":"Subyard/Subyard"`, `"checkout":"checkout-a"`,
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

func TestFacadeExactSlotAcquireIsBackwardCompatible(t *testing.T) {
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
	base := "acquire client SHA256:key checkout tests " + key[0] + " " + key[1]
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
	if !strings.Contains(output.String(), `"code":"busy"`) || len(provisioned) != 1 {
		t.Fatalf("occupied exact response=%s provisioned=%v", output.String(), provisioned)
	}
	output.Reset()
	if err := facade.Run(base); err != nil {
		t.Fatal(err)
	}
	var automatic facadeResponse
	if err := json.Unmarshal(output.Bytes(), &automatic); err != nil {
		t.Fatal(err)
	}
	if automatic.Grant == nil || automatic.Grant.SlotID != "slot-001" {
		t.Fatalf("automatic acquire response=%s", output.String())
	}
	output.Reset()
	if err := facade.Run(base + " slot-2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"invalid_request"`) {
		t.Fatalf("non-canonical slot response=%s", output.String())
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

func TestFacadeAcceptsSafeOpaqueNewClientOldBrokerFallback(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	var output bytes.Buffer
	facade := Facade{Store: store, Output: &output}
	key := strings.Fields(fixturePublicKey(t))
	command := strings.Join([]string{
		"acquire", "client", "SHA256:key", "Subyard-2+run-a",
		"tests", key[0], key[1], "slot-001",
	}, " ")
	if err := facade.Run(command); err != nil {
		t.Fatal(err)
	}
	var response facadeResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Grant == nil || response.Grant.SlotID != "slot-001" ||
		response.Grant.Context != nil {
		t.Fatalf("opaque fallback response = %s", output.String())
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
	if err := facade.Run("acquire client SHA256:key checkout tests " + key[0] + " " + key[1]); err != nil {
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
		"acquire client SHA256:key /home/dev/private tests " + key[0] + " " + key[1],
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"invalid_request"`) {
		t.Fatalf("private-path response=%s", output.String())
	}
}
