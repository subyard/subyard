package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

type Facade struct {
	Context         context.Context
	Input           io.Reader
	OnStatus        func(LeasePool) ResourceStatus
	OnObserve       func([]LeaseSlot) []ResourceObservation
	EnvironmentSpec func(string) (EnvironmentSpec, error)
	Store           LeaseStore
	Output          io.Writer
	Events          *EventRecorder
	OnAcquire       func(context.Context, LeaseGrant, string) (LeaseGrant, error)
	OnRelease       func(context.Context, LeaseGrant) error
	OnQuarantine    func(context.Context, LeaseGrant, error) error
}

type facadeResponse struct {
	Observation   *ObservationPage    `json:"observation,omitempty"`
	Resources     *ResourceStatus     `json:"resources,omitempty"`
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
	Events        *[]BrokerEvent      `json:"events,omitempty"`
	Incident      *IncidentArtifact   `json:"incident,omitempty"`
}

func (facade Facade) Run(originalCommand string) (runErr error) {
	if facade.Context == nil {
		facade.Context = context.Background()
	}
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
		var resources *ResourceStatus
		if facade.OnStatus != nil {
			value := facade.OnStatus(pool)
			resources = &value
		}
		return facade.write(facadeResponse{
			SchemaVersion: LeaseProtocolVersion, Status: "ok",
			Capabilities: []string{"attribution-v2", "environment-acquire-v3", "process-acquire-v4", DisposableLifecycle, "vm-count-v1", "lease-logs-v1", "resource-observe-v1"}, Pool: &pool, Resources: resources,
		})
	case "observe-v1":
		if len(fields) != 4 || facade.Events == nil {
			return facade.writeError("invalid_request", "observe-v1 requires client_id cursor|- count")
		}
		count, err := strconv.Atoi(fields[3])
		if err != nil {
			return facade.writeError("invalid_request", "invalid observer count")
		}
		page, err := facade.Events.readObservationPage(fields[1], fields[2], count)
		if err != nil {
			return facade.writeError("unavailable", "observer records unavailable")
		}
		pool, err := facade.Store.Inspect()
		if err != nil {
			return facade.writeError("unavailable", "observer state unavailable")
		}
		var owned []LeaseSlot
		for _, slot := range pool.Slots {
			if slot.ClientID == fields[1] && slot.State != SlotAvailable {
				owned = append(owned, slot)
			}
		}
		if facade.OnObserve != nil && len(owned) > 0 {
			page.Observations = facade.OnObserve(owned)
		}
		// A delayed measurement cannot be attached to a successor allocation.
		current, err := facade.Store.Inspect()
		if err != nil {
			return facade.writeError("unavailable", "observer state unavailable")
		}
		kept := page.Observations[:0]
		for _, sample := range page.Observations {
			for _, slot := range current.Slots {
				if slot.ClientID == fields[1] && slot.SlotID == sample.SlotID && slot.ResourceGeneration == sample.ResourceGeneration && slot.LeaseEpoch == sample.LeaseEpoch && slot.State != SlotAvailable {
					kept = append(kept, sample)
					break
				}
			}
		}
		page.Observations = kept
		if page.Observations == nil {
			page.Observations = []ResourceObservation{}
		}
		page.ObservedAt = facade.Events.now()
		for _, slot := range current.Slots {
			if slot.ClientID == fields[1] && slot.State != SlotAvailable {
				if allocation := publicResourceAllocation(slot); allocation != nil {
					page.Allocations = append(page.Allocations, allocation)
				}
			}
		}
		return facade.write(facadeResponse{SchemaVersion: LeaseProtocolVersion, Status: "ok", Observation: &page})

	case "logs-v1", "incident-v1":
		if facade.Events == nil {
			return facade.writeError("unavailable", "agent logs are unavailable")
		}
		if fields[0] == "incident-v1" {
			if len(fields) != 3 || !agentLogClientID.MatchString(fields[1]) || !brokerRecordID.MatchString(fields[2]) {
				return facade.writeError("invalid_request", "incident-v1 requires client_id incident_id")
			}
			incident, err := facade.Events.readAgentIncident(fields[1], fields[2])
			if err != nil {
				return facade.writeError("unavailable", "incident is unavailable for this controller")
			}
			return facade.write(facadeResponse{SchemaVersion: LeaseProtocolVersion, Status: "ok", Incident: incident})
		}
		if len(fields) != 4 || !agentLogClientID.MatchString(fields[1]) {
			return facade.writeError("invalid_request", "logs-v1 requires client_id slot_id|- count")
		}
		count, err := strconv.Atoi(fields[3])
		if err != nil || count < 1 || count > maxAgentLogEvents || (fields[2] != "-" && !brokerSlotID.MatchString(fields[2])) {
			return facade.writeError("invalid_request", "invalid agent log selection")
		}
		events, err := facade.Events.readAgentEvents(fields[1], fields[2], count)
		if err != nil {
			return facade.writeError("unavailable", "agent logs are unavailable")
		}
		return facade.write(facadeResponse{SchemaVersion: LeaseProtocolVersion, Status: "ok", Events: &events})
	case "acquire", "acquire-v2":
		return facade.writeAcquireError("invalid_request", "unsupported_acquire", "use acquire-v3 with an explicit environment type")
	case "acquire-v3", "acquire-v4":
		if len(fields) == 10 {
			return facade.writeAcquireError("invalid_request", "missing_slot_id", "acquire-v3 requires TYPE client_id fingerprint yard project run purpose key_type key_blob slot_id")
		}
		if len(fields) != 11 && len(fields) != 12 {
			return facade.writeError("invalid_request", "acquire-v3 requires TYPE client_id fingerprint yard project run purpose key_type key_blob slot_id [vm_count]")
		}
		resolve := facade.EnvironmentSpec
		if resolve == nil {
			cfg, err := ConfigFromValues(nil)
			if err != nil {
				return facade.writeError("unavailable", "invalid broker configuration")
			}
			resolve = cfg.EnvironmentSpec
		}
		spec, err := resolve(fields[1])
		if err != nil {
			return facade.writeAcquireError("invalid_request", "unsupported_environment", "unsupported environment type")
		}
		if len(fields) == 12 {
			if fields[11] != "1" && fields[11] != "2" {
				return facade.writeAcquireError("invalid_request", "invalid_vm_count", "vm_count must be 1 or 2")
			}
			spec.Count, _ = strconv.Atoi(fields[11])
			if err := spec.Validate(); err != nil {
				return facade.writeAcquireError("invalid_request", "invalid_vm_count", "unsupported VM count for environment type")
			}
		}
		publicKey := fields[8] + " " + fields[9]
		if _, keyErr := normalizedPublicKey(publicKey); keyErr != nil || fields[8] != "ssh-ed25519" {
			return facade.writeError("invalid_request", "lease key must be Ed25519")
		}
		if fields[0] == "acquire-v4" {
			owner, err := currentAcquireOwner()
			if err != nil {
				return facade.writeError("unavailable", "requester identity unavailable")
			}
			facade.Store.AcquireOwner = &owner
		}
		grant, err := facade.Store.AcquireV3Slot(spec, fields[2], fields[3], fields[4], fields[5], fields[6], fields[7], fields[10])
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
		var session *acquireSession
		if fields[0] == "acquire-v4" {
			session = facade.newAcquireSession(grant)
			defer func() { runErr = errors.Join(runErr, session.close()) }()
			facade.Context = session.context
			if err := facade.write(facadeResponse{SchemaVersion: LeaseProtocolVersion, Status: "reserved", Grant: &grant}); err != nil {
				return err
			}
		}
		if facade.OnAcquire != nil {
			grant, err = facade.OnAcquire(facade.Context, grant, publicKey)
			if err != nil {
				var capacity *CapacityError
				if errors.As(err, &capacity) || errors.Is(err, ErrLegacyRetained) {
					_ = facade.recordSlot("lease.admission_rejected", grant.SlotID, SlotProvisioning, SlotAvailable, err)
					if abortErr := facade.Store.AbortProvisioning(grant); abortErr != nil {
						return facade.writeError("unavailable", "admission cancellation failed")
					}
					if errors.Is(err, ErrLegacyRetained) {
						return facade.writeAcquireError("unavailable", "legacy_retirement_required", "retained guest data requires explicit operator retirement")
					}
					// Keep the released five-key refusal envelope. Numeric evidence fits
					// its existing bounded message; older strict readers need no new keys.
					message := capacity.Error()
					if len(message) > 1024 {
						message = message[:1024]
					}
					return facade.write(facadeResponse{SchemaVersion: LeaseProtocolVersion,
						Status: "error", Code: "capacity", Reason: capacity.Resource, Message: message})
				}
				if session != nil {
					slot, inspectErr := storeSlot(facade.Store, grant.SlotID)
					if session.context.Err() != nil || (inspectErr == nil && slot.State == SlotDraining) {
						return facade.writeError("cancelled", "acquisition cancelled; cleanup required")
					}
				}
				_ = facade.quarantine(grant, err)
				return facade.writeError("quarantined", "slot provisioning failed")
			}
		}
		if eventErr := facade.recordSlot("lease.held", grant.SlotID, SlotProvisioning, SlotHeld, nil); eventErr != nil {
			_ = facade.quarantine(grant, eventErr)
			return facade.writeError("quarantined", "durable broker event failed")
		}
		if session != nil {
			close(session.ready)
		}
		if err := facade.write(facadeResponse{SchemaVersion: LeaseProtocolVersion, Status: "ok", Grant: &grant}); err != nil {
			return err
		}
		if session != nil {
			return session.handoff()
		}
		return nil
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
			SchemaVersion: LeaseProtocolVersion, Status: "ok", ExpiresAt: &expires,
		})
	case "release":
		grant, err := parseGrant(fields)
		if err != nil {
			return facade.writeError("invalid_request", err.Error())
		}
		if facade.OnRelease == nil {
			return facade.writeError("unavailable", "physical lease lifecycle is unavailable")
		}
		if err := facade.OnRelease(facade.Context, grant); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return facade.writeError("lease_lost", "lease is no longer current")
			}
			return facade.writeError("quarantined", "slot fencing or stop failed")
		}
		return facade.write(facadeResponse{
			SchemaVersion: LeaseProtocolVersion, Status: "ok", Message: "released",
		})
	default:
		return facade.writeError("invalid_request", "unknown facade operation")
	}
}

func (facade Facade) quarantine(grant LeaseGrant, cause error) error {
	if facade.OnQuarantine == nil {
		return facade.Store.Quarantine(grant, cause)
	}
	return facade.OnQuarantine(facade.Context, grant, cause)
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
		Allocation:         publicResourceAllocation(slot),
		agentClientID:      slot.ClientID,
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
		slot.AcquireOwner = nil
	}
}

func (facade Facade) writeError(code, message string) error {
	return facade.write(facadeResponse{
		SchemaVersion: LeaseProtocolVersion, Status: "error", Code: code,
		Message: boundedReason(message),
	})
}

func (facade Facade) writeAcquireError(code, reason, message string) error {
	return facade.write(facadeResponse{
		SchemaVersion: LeaseProtocolVersion, Status: "error", Code: code,
		Reason: reason, Message: boundedReason(message),
	})
}

func (facade Facade) writeUnavailableError(unavailable *SlotUnavailableError) error {
	return facade.write(facadeResponse{
		SchemaVersion: LeaseProtocolVersion, Status: "error", Code: "busy",
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
