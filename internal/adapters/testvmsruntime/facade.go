package testvmsruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

type Facade struct {
	Store        LeaseStore
	Output       io.Writer
	Events       *EventRecorder
	OnAcquire    func(LeaseGrant, string) (LeaseGrant, error)
	OnRelease    func(LeaseGrant) error
	OnQuarantine func(LeaseGrant, error) error
}

type facadeResponse struct {
	SchemaVersion int                 `json:"schema_version"`
	Status        string              `json:"status"`
	Capabilities  []string            `json:"capabilities,omitempty"`
	Code          string              `json:"code,omitempty"`
	State         SlotState           `json:"state,omitempty"`
	Reason        string              `json:"reason,omitempty"`
	Owner         *LeaseOwnerSnapshot `json:"owner,omitempty"`
	Message       string              `json:"message,omitempty"`
	Pool          *LeasePool          `json:"pool,omitempty"`
	Grant         *LeaseGrant         `json:"grant,omitempty"`
	ExpiresAt     *time.Time          `json:"expires_at,omitempty"`
}

func (facade Facade) Run(originalCommand string) error {
	if facade.Output == nil {
		facade.Output = io.Discard
	}
	fields := strings.Fields(originalCommand)
	if len(fields) == 0 {
		fields = []string{"status"}
	}
	switch fields[0] {
	case "status":
		if len(fields) != 1 {
			return facade.writeError("invalid_request", "status accepts no arguments")
		}
		pool, err := facade.Store.Inspect()
		if err != nil {
			return facade.writeError("unavailable", err.Error())
		}
		redactPool(&pool)
		return facade.write(facadeResponse{
			SchemaVersion: LeaseSchemaVersion, Status: "ok",
			Capabilities: []string{"attribution-v2"}, Pool: &pool,
		})
	case "acquire":
		return facade.writeAcquireError("invalid_request", "unsupported_acquire",
			"legacy acquire is unsupported; use acquire-v2")
	case "acquire-v2":
		var grant LeaseGrant
		var err error
		if len(fields) == 9 {
			return facade.writeAcquireError("invalid_request", "missing_slot_id",
				"acquire-v2 requires client_id fingerprint yard project run purpose key_type key_blob slot_id")
		}
		if len(fields) != 10 {
			return facade.writeError("invalid_request",
				"acquire-v2 requires client_id fingerprint yard project run purpose key_type key_blob slot_id")
		}
		publicKey := fields[7] + " " + fields[8]
		if _, keyErr := normalizedPublicKey(publicKey); keyErr != nil || fields[7] != "ssh-ed25519" {
			return facade.writeError("invalid_request", "lease key must be Ed25519")
		}
		grant, err = facade.Store.AcquireV2Slot(
			fields[1], fields[2], fields[3], fields[4], fields[5], fields[6], fields[9],
		)
		if err != nil {
			var unavailable *SlotUnavailableError
			if errors.As(err, &unavailable) {
				return facade.writeUnavailableError(unavailable)
			}
			if errors.Is(err, ErrInvalidSlot) {
				return facade.writeAcquireError("invalid_request", "invalid_slot", "invalid slot_id")
			}
			if errors.Is(err, ErrLeaseBusy) {
				return facade.writeError("busy", "requested slot is busy or unavailable")
			}
			return facade.writeError("invalid_request", err.Error())
		}
		if eventErr := facade.recordSlot("lease.acquire", grant.SlotID, "", SlotProvisioning, nil); eventErr != nil {
			_ = facade.quarantine(grant, eventErr)
			return facade.writeError("quarantined", "durable broker event failed")
		}
		if facade.OnAcquire != nil {
			grant, err = facade.OnAcquire(grant, publicKey)
			if err != nil {
				_ = facade.quarantine(grant, err)
				return facade.writeError("quarantined", "slot provisioning failed")
			}
		}
		if eventErr := facade.recordSlot("lease.held", grant.SlotID, SlotProvisioning, SlotHeld, nil); eventErr != nil {
			_ = facade.quarantine(grant, eventErr)
			return facade.writeError("quarantined", "durable broker event failed")
		}
		return facade.write(facadeResponse{
			SchemaVersion: LeaseSchemaVersion, Status: "ok", Grant: &grant,
		})
	case "renew":
		grant, err := parseGrant(fields)
		if err != nil {
			return facade.writeError("invalid_request", err.Error())
		}
		expires, err := facade.Store.Renew(grant)
		if err != nil {
			_ = facade.recordGrantFailure("lease.renew_failed", grant, err)
			return facade.writeError("lease_lost", "lease is no longer current")
		}
		return facade.write(facadeResponse{
			SchemaVersion: LeaseSchemaVersion, Status: "ok", ExpiresAt: &expires,
		})
	case "release":
		grant, err := parseGrant(fields)
		if err != nil {
			return facade.writeError("invalid_request", err.Error())
		}
		if facade.OnRelease == nil {
			return facade.writeError("unavailable", "physical lease lifecycle is unavailable")
		}
		if err := facade.OnRelease(grant); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return facade.writeError("lease_lost", "lease is no longer current")
			}
			return facade.writeError("quarantined", "slot fencing or stop failed")
		}
		return facade.write(facadeResponse{
			SchemaVersion: LeaseSchemaVersion, Status: "ok", Message: "released",
		})
	default:
		return facade.writeError("invalid_request", "unknown facade operation")
	}
}

func (facade Facade) quarantine(grant LeaseGrant, cause error) error {
	if facade.OnQuarantine == nil {
		return facade.Store.Quarantine(grant, cause)
	}
	return facade.OnQuarantine(grant, cause)
}

func (facade Facade) recordGrantFailure(
	kind string,
	grant LeaseGrant,
	cause error,
) error {
	if facade.Events == nil {
		return nil
	}
	_, err := facade.Events.Record(BrokerEvent{
		Kind:       kind,
		SlotID:     grant.SlotID,
		LeaseEpoch: grant.LeaseEpoch,
		Error:      errorString(cause),
	})
	return err
}

func (facade Facade) recordSlot(
	kind, slotID string,
	from, to SlotState,
	cause error,
) error {
	if facade.Events == nil {
		return nil
	}
	slot, err := storeSlot(facade.Store, slotID)
	if err != nil {
		return err
	}
	return facade.recordSlotSnapshot(kind, slot, from, to, cause)
}

func (facade Facade) recordSlotSnapshot(
	kind string,
	slot LeaseSlot,
	from, to SlotState,
	cause error,
) error {
	if facade.Events == nil {
		return nil
	}
	_, err := facade.Events.Record(BrokerEvent{
		Kind:               kind,
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		FromState:          from,
		ToState:            to,
		Error:              errorStringOrEmpty(cause),
		IncidentID:         slot.IncidentID,
		Context:            leaseContextFromSlot(slot),
	})
	return err
}

func errorStringOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func parseGrant(fields []string) (LeaseGrant, error) {
	if len(fields) != 5 {
		return LeaseGrant{}, errors.New("operation requires slot_id lease_id lease_epoch capability")
	}
	epoch, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil || epoch == 0 {
		return LeaseGrant{}, errors.New("invalid lease_epoch")
	}
	for _, value := range []string{fields[1], fields[2], fields[4]} {
		if !safeLeaseText(value, 128) || value == "" {
			return LeaseGrant{}, errors.New("invalid lease credential")
		}
	}
	return LeaseGrant{
		SlotID: fields[1], LeaseID: fields[2], LeaseEpoch: epoch, Capability: fields[4],
	}, nil
}

func redactPool(pool *LeasePool) {
	for index := range pool.Slots {
		slot := &pool.Slots[index]
		slot.CapabilityHash = ""
		slot.LeaseID = ""
		slot.ClientID = ""
		slot.ControllerFingerprint = ""
		slot.FailureReason = ""
	}
}

func (facade Facade) writeError(code, message string) error {
	return facade.write(facadeResponse{
		SchemaVersion: LeaseSchemaVersion, Status: "error", Code: code,
		Message: boundedReason(message),
	})
}

func (facade Facade) writeAcquireError(code, reason, message string) error {
	return facade.write(facadeResponse{
		SchemaVersion: LeaseSchemaVersion, Status: "error", Code: code,
		Reason: reason, Message: boundedReason(message),
	})
}

func (facade Facade) writeUnavailableError(unavailable *SlotUnavailableError) error {
	return facade.write(facadeResponse{
		SchemaVersion: LeaseSchemaVersion, Status: "error", Code: "busy",
		State: unavailable.State, Reason: unavailable.Reason, Owner: unavailable.Owner,
		Message: "requested slot is unavailable",
	})
}

func (facade Facade) write(response facadeResponse) error {
	encoder := json.NewEncoder(facade.Output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return fmt.Errorf("write facade response: %w", err)
	}
	return nil
}
