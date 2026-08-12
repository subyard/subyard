package testvmsruntime

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	LeaseSchemaVersion            = 1
	LeaseRecoverySchemaVersion    = 1
	LeaseAttributionSchemaVersion = 2
	LeaseAttributionSchemaV1      = 1
	LeaseTargetStaleExitCode      = 75
	LeaseTTL                      = 10 * time.Minute
	ProvisioningTTL               = 30 * time.Minute
)

const (
	heartbeatExpiredReason    = "heartbeat expired; cleanup required"
	provisioningExpiredReason = "provisioning deadline expired; cleanup required"
)

var (
	ErrCorruptLeaseState     = errors.New("corrupt lease state")
	ErrUnsupportedLeaseState = errors.New("unsupported lease state")
	ErrLeaseBusy             = errors.New("busy")
	ErrLeaseLost             = errors.New("lease lost")
	ErrLeaseTargetStale      = errors.New("lease target is stale")
)

type SlotState string

const (
	SlotAvailable    SlotState = "available"
	SlotProvisioning SlotState = "provisioning"
	SlotHeld         SlotState = "held"
	SlotDraining     SlotState = "draining"
	SlotQuarantined  SlotState = "quarantined"
	SlotRecovering   SlotState = "recovering"
	SlotUnavailable  SlotState = "unavailable"
)

type LeaseSlot struct {
	SlotID                string    `json:"slot_id"`
	ResourceGeneration    uint64    `json:"resource_generation"`
	LeaseEpoch            uint64    `json:"lease_epoch"`
	State                 SlotState `json:"state"`
	ClientID              string    `json:"client_id,omitempty"`
	ControllerFingerprint string    `json:"controller_fingerprint,omitempty"`
	DisplayLabel          string    `json:"display_label,omitempty"`
	Yard                  string    `json:"yard,omitempty"`
	Project               string    `json:"project,omitempty"`
	Checkout              string    `json:"checkout,omitempty"`
	Run                   string    `json:"run,omitempty"`
	Purpose               string    `json:"purpose,omitempty"`
	LeaseID               string    `json:"lease_id,omitempty"`
	CapabilityHash        string    `json:"capability_hash,omitempty"`
	AcquiredAt            time.Time `json:"acquired_at,omitempty"`
	ProvisioningStartedAt time.Time `json:"provisioning_started_at,omitempty"`
	ReadyAt               time.Time `json:"ready_at,omitempty"`
	LastHeartbeatAt       time.Time `json:"last_heartbeat_at,omitempty"`
	ExpiresAt             time.Time `json:"expires_at,omitempty"`
	FailureReason         string    `json:"failure_reason,omitempty"`
	LastFailureEventID    string    `json:"last_failure_event_id,omitempty"`
	IncidentID            string    `json:"incident_id,omitempty"`
	RecoveryAttempt       uint64    `json:"recovery_attempt"`
	NextRecoveryAt        time.Time `json:"next_recovery_at,omitempty"`
	RecoveryStartedAt     time.Time `json:"recovery_started_at,omitempty"`
}

type LeasePool struct {
	SchemaVersion int         `json:"schema_version"`
	ResourceType  string      `json:"resource_type"`
	ResourceID    string      `json:"resource_id"`
	Slots         []LeaseSlot `json:"slots"`
}

type leaseRecoveryJournal struct {
	SchemaVersion int                 `json:"schema_version"`
	Slots         []leaseRecoverySlot `json:"slots"`
}

type leaseRecoverySlot struct {
	SlotID             string    `json:"slot_id"`
	ResourceGeneration uint64    `json:"resource_generation"`
	LeaseEpoch         uint64    `json:"lease_epoch"`
	State              SlotState `json:"state"`
	LastFailureEventID string    `json:"last_failure_event_id,omitempty"`
	IncidentID         string    `json:"incident_id,omitempty"`
	RecoveryAttempt    uint64    `json:"recovery_attempt"`
	NextRecoveryAt     time.Time `json:"next_recovery_at,omitempty"`
	RecoveryStartedAt  time.Time `json:"recovery_started_at,omitempty"`
}

type LeaseGrant struct {
	SlotID             string        `json:"slot_id"`
	ResourceGeneration uint64        `json:"resource_generation,omitempty"`
	LeaseID            string        `json:"lease_id"`
	Capability         string        `json:"capability"`
	LeaseEpoch         uint64        `json:"lease_epoch"`
	ExpiresAt          time.Time     `json:"expires_at"`
	Context            *LeaseContext `json:"context,omitempty"`
	DataUser           string        `json:"data_user,omitempty"`
	Targets            []LeaseTarget `json:"targets,omitempty"`
}

// LeaseIdentity is the non-secret identity of one concrete lease-backed VM pair.
// Both counters are required so an operator action prepared for an earlier lease
// cannot affect a replacement lease or a rebuilt pair that reused the slot name.
type LeaseIdentity struct {
	SlotID             string
	ResourceGeneration uint64
	LeaseEpoch         uint64
}

func (identity LeaseIdentity) validate() error {
	if !brokerSlotID.MatchString(identity.SlotID) ||
		identity.ResourceGeneration == 0 || identity.LeaseEpoch == 0 {
		return errors.New("complete lease target identity is required")
	}
	return nil
}

type LeaseContext struct {
	SchemaVersion int    `json:"schema_version"`
	Yard          string `json:"yard,omitempty"`
	Project       string `json:"project"`
	Checkout      string `json:"checkout,omitempty"`
	Run           string `json:"run"`
	Purpose       string `json:"purpose"`
}

type LeaseTarget struct {
	Selector    int    `json:"selector"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	HostKeyType string `json:"host_key_type"`
	HostKeyBlob string `json:"host_key_blob"`
}

type LeaseStore struct {
	Path      string
	SlotCount int
	Now       func() time.Time
}

func (store LeaseStore) now() time.Time {
	if store.Now != nil {
		return store.Now().UTC()
	}
	return time.Now().UTC()
}

func (store LeaseStore) Status() (LeasePool, error) {
	var result LeasePool
	err := store.withLock(true, func(pool *LeasePool) error {
		result = *pool
		result.Slots = append([]LeaseSlot(nil), pool.Slots...)
		return nil
	})
	return result, err
}

// Inspect reads the durable lease state without creating a directory or lock,
// modifying expiration state, or reconciling the pool on disk. Callers that
// need a safe preflight may use the returned, in-memory reconciliation.
func (store LeaseStore) Inspect() (LeasePool, error) {
	if store.SlotCount < 1 {
		return LeasePool{}, errors.New("slot count must be positive")
	}
	pool, err := store.load()
	if err != nil {
		return LeasePool{}, err
	}
	if err := validateLeaseSlotInventory(pool.Slots); err != nil {
		return LeasePool{}, err
	}
	if err := reconcileSlotCount(&pool, store.SlotCount); err != nil {
		return LeasePool{}, err
	}
	expireStale(&pool, store.now())
	pool.Slots = append([]LeaseSlot(nil), pool.Slots...)
	return pool, nil
}

func validateLeaseSlotInventory(slots []LeaseSlot) error {
	seen := make(map[int]struct{}, len(slots))
	for _, slot := range slots {
		number, err := slotNumber(slot.SlotID, len(slots))
		if err != nil || slot.SlotID != fmt.Sprintf("slot-%03d", number) {
			return fmt.Errorf("invalid lease slot id %q", slot.SlotID)
		}
		if _, duplicate := seen[number]; duplicate {
			return fmt.Errorf("duplicate lease slot id %q", slot.SlotID)
		}
		seen[number] = struct{}{}
		switch slot.State {
		case SlotAvailable, SlotProvisioning, SlotHeld, SlotDraining,
			SlotQuarantined, SlotRecovering, SlotUnavailable:
		default:
			return fmt.Errorf("invalid lease slot state %q for %s", slot.State, slot.SlotID)
		}
	}
	return nil
}

func (store LeaseStore) Acquire(clientID, fingerprint, label, purpose string) (LeaseGrant, error) {
	context := legacyLeaseContext(label, purpose)
	return store.acquire(clientID, fingerprint, label, purpose, context, "")
}

func (store LeaseStore) AcquireSlot(
	clientID, fingerprint, label, purpose, slotID string,
) (LeaseGrant, error) {
	number, err := slotNumber(slotID, store.SlotCount)
	if err != nil || slotID != fmt.Sprintf("slot-%03d", number) {
		return LeaseGrant{}, fmt.Errorf("invalid slot id %q", slotID)
	}
	context := legacyLeaseContext(label, purpose)
	return store.acquire(clientID, fingerprint, label, purpose, context, slotID)
}

func (store LeaseStore) AcquireV2(
	clientID, fingerprint, yard, project, run, purpose string,
) (LeaseGrant, error) {
	context := LeaseContext{
		SchemaVersion: LeaseAttributionSchemaVersion,
		Yard:          yard, Project: project, Run: run, Purpose: purpose,
	}
	if err := validateLeaseContext(context); err != nil {
		return LeaseGrant{}, err
	}
	return store.acquire(
		clientID, fingerprint, contextDisplayLabel(context), purpose, &context, "",
	)
}

func (store LeaseStore) AcquireV2Slot(
	clientID, fingerprint, yard, project, run, purpose, slotID string,
) (LeaseGrant, error) {
	number, err := slotNumber(slotID, store.SlotCount)
	if err != nil || slotID != fmt.Sprintf("slot-%03d", number) {
		return LeaseGrant{}, fmt.Errorf("invalid slot id %q", slotID)
	}
	context := LeaseContext{
		SchemaVersion: LeaseAttributionSchemaVersion,
		Yard:          yard, Project: project, Run: run, Purpose: purpose,
	}
	if err := validateLeaseContext(context); err != nil {
		return LeaseGrant{}, err
	}
	return store.acquire(
		clientID, fingerprint, contextDisplayLabel(context), purpose, &context, slotID,
	)
}

func (store LeaseStore) acquire(
	clientID, fingerprint, label, purpose string, context *LeaseContext,
	requestedSlot string,
) (LeaseGrant, error) {
	if !safeLeaseText(clientID, 96) || clientID == "" {
		return LeaseGrant{}, errors.New("invalid client_id")
	}
	if !safeLeaseText(fingerprint, 128) || fingerprint == "" {
		return LeaseGrant{}, errors.New("invalid controller fingerprint")
	}
	if !safeLeaseText(label, 80) || !safeLeaseText(purpose, 160) {
		return LeaseGrant{}, errors.New("invalid display metadata")
	}
	if (label != "" && context == nil && !safeLeaseLabel(label, 80)) ||
		(purpose != "" && !safeLeaseLabel(purpose, 80)) {
		return LeaseGrant{}, errors.New("invalid display metadata")
	}
	var grant LeaseGrant
	err := store.withLock(true, func(pool *LeasePool) error {
		now := store.now()
		expireStale(pool, now)
		for index := range pool.Slots {
			slot := &pool.Slots[index]
			if requestedSlot != "" && slot.SlotID != requestedSlot {
				continue
			}
			if slot.State != SlotAvailable {
				if requestedSlot != "" {
					return fmt.Errorf("%w: %s is %s", ErrLeaseBusy, slot.SlotID, slot.State)
				}
				continue
			}
			leaseID, err := randomToken(16)
			if err != nil {
				return err
			}
			capability, err := randomToken(32)
			if err != nil {
				return err
			}
			slot.LeaseEpoch++
			slot.State = SlotProvisioning
			slot.ClientID = clientID
			slot.ControllerFingerprint = fingerprint
			slot.DisplayLabel = label
			if context != nil {
				slot.Yard = context.Yard
				slot.Project = context.Project
				slot.Checkout = context.Checkout
				slot.Run = context.Run
			}
			slot.Purpose = purpose
			slot.LeaseID = leaseID
			slot.CapabilityHash = capabilityDigest(capability)
			slot.AcquiredAt = now
			slot.ProvisioningStartedAt = now
			slot.LastHeartbeatAt = now
			slot.ExpiresAt = now.Add(ProvisioningTTL)
			slot.FailureReason = ""
			slot.LastFailureEventID = ""
			slot.IncidentID = ""
			slot.RecoveryAttempt = 0
			slot.NextRecoveryAt = time.Time{}
			slot.RecoveryStartedAt = time.Time{}
			grant = LeaseGrant{
				SlotID: slot.SlotID, ResourceGeneration: slot.ResourceGeneration,
				LeaseID: leaseID, Capability: capability,
				LeaseEpoch: slot.LeaseEpoch, ExpiresAt: slot.ExpiresAt,
			}
			if context != nil {
				current := *context
				grant.Context = &current
			}
			return nil
		}
		return ErrLeaseBusy
	})
	return grant, err
}

func (store LeaseStore) MarkHeld(grant LeaseGrant) (time.Time, error) {
	var expires time.Time
	err := store.mutateOwned(grant, func(slot *LeaseSlot, now time.Time) error {
		if slot.State != SlotProvisioning {
			return fmt.Errorf("slot is %s, not provisioning", slot.State)
		}
		slot.State = SlotHeld
		slot.ReadyAt = now
		slot.LastHeartbeatAt = now
		slot.ExpiresAt = now.Add(LeaseTTL)
		expires = slot.ExpiresAt
		return nil
	})
	return expires, err
}

func (store LeaseStore) Renew(grant LeaseGrant) (time.Time, error) {
	var expires time.Time
	err := store.mutateOwned(grant, func(slot *LeaseSlot, now time.Time) error {
		if slot.State != SlotHeld && slot.State != SlotProvisioning {
			return ErrLeaseLost
		}
		slot.LastHeartbeatAt = now
		slot.ExpiresAt = now.Add(LeaseTTL)
		expires = slot.ExpiresAt
		return nil
	})
	return expires, err
}

func (store LeaseStore) BeginDrain(grant LeaseGrant) error {
	return store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error {
		if slot.State == SlotDraining {
			return nil
		}
		if slot.State != SlotHeld && slot.State != SlotProvisioning {
			return ErrLeaseLost
		}
		slot.State = SlotDraining
		return nil
	})
}

func (store LeaseStore) FinishDrain(slotID string, stopErr error) error {
	return store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, slotID)
		if err != nil {
			return err
		}
		if slot.State == SlotAvailable && stopErr == nil {
			// A concurrent release/revoke may have completed the same fencing sequence first.
			return nil
		}
		if slot.State != SlotDraining {
			return fmt.Errorf("slot %s is not draining", slotID)
		}
		if stopErr != nil {
			slot.State = SlotQuarantined
			slot.FailureReason = boundedReason(stopErr.Error())
			slot.NextRecoveryAt = store.now()
			return nil
		}
		clearLease(slot)
		slot.State = SlotAvailable
		return nil
	})
}

func (store LeaseStore) BeginDrainAll(reason string) error {
	return store.withLock(true, func(pool *LeasePool) error {
		for index := range pool.Slots {
			slot := &pool.Slots[index]
			if slot.State != SlotHeld && slot.State != SlotProvisioning {
				continue
			}
			slot.State = SlotDraining
			slot.FailureReason = boundedReason(reason)
		}
		return nil
	})
}

func (store LeaseStore) BeginDrainSlot(slotID, reason string) error {
	return store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, slotID)
		if err != nil {
			return err
		}
		switch slot.State {
		case SlotAvailable:
			return nil
		case SlotHeld, SlotProvisioning:
			slot.State = SlotDraining
			slot.FailureReason = boundedReason(reason)
			return nil
		case SlotDraining:
			return nil
		default:
			return fmt.Errorf("slot %s is %s", slotID, slot.State)
		}
	})
}

// BeginExpectedDrain atomically verifies the target observed during preflight
// and fences only that lease. The returned bool says whether the caller should
// continue the physical drain; an already completed matching lease is a no-op.
func (store LeaseStore) BeginExpectedDrain(
	expected LeaseIdentity,
	reason string,
) (bool, error) {
	if err := expected.validate(); err != nil {
		return false, err
	}
	started := false
	err := store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, expected.SlotID)
		if err != nil {
			return err
		}
		if !slotMatchesLeaseIdentity(*slot, expected) {
			return fmt.Errorf("%w: slot %s identity changed", ErrLeaseTargetStale, expected.SlotID)
		}
		switch slot.State {
		case SlotAvailable:
			return nil
		case SlotHeld, SlotProvisioning:
			slot.State = SlotDraining
			slot.FailureReason = boundedReason(reason)
			started = true
			return nil
		case SlotDraining:
			started = true
			return nil
		default:
			return fmt.Errorf("slot %s is %s", expected.SlotID, slot.State)
		}
	})
	return started, err
}

func (store LeaseStore) BeginRecovery(slotID string) error {
	_, _, err := store.BeginScheduledRecovery(slotID, true)
	if !errors.Is(err, ErrCorruptLeaseState) && !errors.Is(err, ErrUnsupportedLeaseState) {
		return err
	}
	return store.rebuildCorruptPoolForRecovery(slotID)
}

func (store LeaseStore) Quarantine(grant LeaseGrant, cause error) error {
	return store.mutateOwned(grant, func(slot *LeaseSlot, now time.Time) error {
		slot.State = SlotQuarantined
		slot.FailureReason = boundedReason(cause.Error())
		slot.NextRecoveryAt = now
		return nil
	})
}

func (store LeaseStore) SetQuarantineIncident(
	slotID, failureEventID, incidentID string,
) error {
	if !brokerRecordID.MatchString(failureEventID) ||
		!brokerRecordID.MatchString(incidentID) {
		return errors.New("invalid quarantine incident identity")
	}
	return store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, slotID)
		if err != nil {
			return err
		}
		if slot.State != SlotQuarantined && slot.State != SlotRecovering {
			return fmt.Errorf("slot %s is %s, not quarantined", slotID, slot.State)
		}
		slot.LastFailureEventID = failureEventID
		slot.IncidentID = incidentID
		if slot.NextRecoveryAt.IsZero() {
			slot.NextRecoveryAt = store.now()
		}
		return nil
	})
}

func (store LeaseStore) InterruptRecovery(slotID string, cause error) error {
	if cause == nil {
		return errors.New("interrupted recovery cause is required")
	}
	return store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, slotID)
		if err != nil {
			return err
		}
		if slot.State != SlotRecovering {
			return fmt.Errorf("slot %s is %s, not recovering", slotID, slot.State)
		}
		slot.State = SlotQuarantined
		slot.FailureReason = boundedReason(cause.Error())
		slot.NextRecoveryAt = store.now()
		slot.RecoveryStartedAt = time.Time{}
		return nil
	})
}

func (store LeaseStore) BeginScheduledRecovery(
	slotID string,
	force bool,
) (LeaseSlot, bool, error) {
	var snapshot LeaseSlot
	started := false
	err := store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, slotID)
		if err != nil {
			return err
		}
		if slot.State == SlotRecovering {
			return nil
		}
		if slot.State != SlotQuarantined {
			return fmt.Errorf("slot %s is %s, not quarantined", slotID, slot.State)
		}
		now := store.now()
		if !force && !slot.NextRecoveryAt.IsZero() && now.Before(slot.NextRecoveryAt) {
			snapshot = *slot
			return nil
		}
		slot.State = SlotRecovering
		slot.RecoveryAttempt++
		slot.RecoveryStartedAt = now
		slot.NextRecoveryAt = time.Time{}
		snapshot = *slot
		started = true
		return nil
	})
	return snapshot, started, err
}

// BeginExpectedRecovery atomically verifies the target observed during
// preflight and starts recovery only for that exact quarantined VM pair.
// A matching recovery already in progress is left untouched.
func (store LeaseStore) BeginExpectedRecovery(
	expected LeaseIdentity,
) (LeaseSlot, bool, error) {
	if err := expected.validate(); err != nil {
		return LeaseSlot{}, false, err
	}
	var snapshot LeaseSlot
	started := false
	err := store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, expected.SlotID)
		if err != nil {
			return err
		}
		if !slotMatchesLeaseIdentity(*slot, expected) {
			return fmt.Errorf("%w: slot %s identity changed", ErrLeaseTargetStale, expected.SlotID)
		}
		switch slot.State {
		case SlotAvailable, SlotRecovering:
			snapshot = *slot
			return nil
		case SlotQuarantined:
			now := store.now()
			slot.State = SlotRecovering
			slot.RecoveryAttempt++
			slot.RecoveryStartedAt = now
			slot.NextRecoveryAt = time.Time{}
			snapshot = *slot
			started = true
			return nil
		default:
			return fmt.Errorf("slot %s is %s, not quarantined", expected.SlotID, slot.State)
		}
	})
	return snapshot, started, err
}

func slotMatchesLeaseIdentity(slot LeaseSlot, expected LeaseIdentity) bool {
	return slot.SlotID == expected.SlotID &&
		slot.ResourceGeneration == expected.ResourceGeneration &&
		slot.LeaseEpoch == expected.LeaseEpoch
}

func (store LeaseStore) FinishRecovery(
	slotID string,
	cause error,
	failureEventID, incidentID string,
) (LeaseSlot, error) {
	var snapshot LeaseSlot
	err := store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, slotID)
		if err != nil {
			return err
		}
		if slot.State != SlotRecovering {
			return fmt.Errorf("slot %s is %s, not recovering", slotID, slot.State)
		}
		now := store.now()
		if cause != nil {
			slot.State = SlotQuarantined
			slot.FailureReason = boundedReason(cause.Error())
			if failureEventID != "" {
				if !brokerRecordID.MatchString(failureEventID) {
					return errors.New("invalid recovery failure event identity")
				}
				slot.LastFailureEventID = failureEventID
			}
			if incidentID != "" {
				if !brokerRecordID.MatchString(incidentID) {
					return errors.New("invalid recovery incident identity")
				}
				slot.IncidentID = incidentID
			}
			slot.NextRecoveryAt = now.Add(recoveryDelay(slot.RecoveryAttempt))
			slot.RecoveryStartedAt = time.Time{}
			snapshot = *slot
			return nil
		}
		lastEvent := slot.LastFailureEventID
		lastIncident := slot.IncidentID
		attempt := slot.RecoveryAttempt
		clearLease(slot)
		slot.ResourceGeneration++
		slot.State = SlotAvailable
		slot.LastFailureEventID = lastEvent
		slot.IncidentID = lastIncident
		slot.RecoveryAttempt = attempt
		snapshot = *slot
		return nil
	})
	return snapshot, err
}

func recoveryDelay(attempt uint64) time.Duration {
	switch attempt {
	case 0, 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

func (store LeaseStore) mutateOwned(
	grant LeaseGrant, mutate func(*LeaseSlot, time.Time) error,
) error {
	return store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, grant.SlotID)
		if err != nil {
			return err
		}
		if slot.LeaseID != grant.LeaseID || slot.LeaseEpoch != grant.LeaseEpoch ||
			slot.CapabilityHash == "" || slot.CapabilityHash != capabilityDigest(grant.Capability) {
			return ErrLeaseLost
		}
		return mutate(slot, store.now())
	})
}

func (store LeaseStore) withLock(write bool, operation func(*LeasePool) error) error {
	if store.SlotCount < 1 {
		return errors.New("slot count must be positive")
	}
	return store.withPoolLock(func(pool *LeasePool) error {
		if err := reconcileSlotCount(pool, store.SlotCount); err != nil {
			return err
		}
		before, _ := json.Marshal(*pool)
		expireStale(pool, store.now())
		if err := operation(pool); err != nil {
			return err
		}
		after, _ := json.Marshal(*pool)
		if write || string(before) != string(after) {
			return store.writePool(*pool)
		}
		return nil
	})
}

func (store LeaseStore) withPoolLock(operation func(*LeasePool) error) error {
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(store.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	pool, err := store.load()
	if err != nil {
		return err
	}
	return operation(&pool)
}

func (store LeaseStore) load() (LeasePool, error) {
	pool := LeasePool{
		SchemaVersion: LeaseSchemaVersion, ResourceType: "agent-e2e",
		ResourceID: "test-vms",
	}
	payload, err := os.ReadFile(store.Path)
	if os.IsNotExist(err) {
		return pool, nil
	}
	if err != nil {
		return pool, err
	}
	if err := json.Unmarshal(payload, &pool); err != nil {
		return pool, fmt.Errorf("%w: %v", ErrCorruptLeaseState, err)
	}
	if pool.SchemaVersion != LeaseSchemaVersion {
		return pool, ErrUnsupportedLeaseState
	}
	if pool.ResourceType != "agent-e2e" || pool.ResourceID != "test-vms" {
		return pool, ErrUnsupportedLeaseState
	}
	if err := store.mergeRecoveryJournal(&pool); err != nil {
		return pool, err
	}
	return pool, nil
}

func (store LeaseStore) recoveryJournalPath() string {
	return store.Path + ".recovery-v1.json"
}

func (store LeaseStore) writePool(pool LeasePool) error {
	journal := leaseRecoveryJournal{SchemaVersion: LeaseRecoverySchemaVersion}
	for _, slot := range pool.Slots {
		if !slotHasRecoveryMetadata(slot) {
			continue
		}
		journal.Slots = append(journal.Slots, leaseRecoverySlot{
			SlotID:             slot.SlotID,
			ResourceGeneration: slot.ResourceGeneration,
			LeaseEpoch:         slot.LeaseEpoch,
			State:              slot.State,
			LastFailureEventID: slot.LastFailureEventID,
			IncidentID:         slot.IncidentID,
			RecoveryAttempt:    slot.RecoveryAttempt,
			NextRecoveryAt:     slot.NextRecoveryAt,
			RecoveryStartedAt:  slot.RecoveryStartedAt,
		})
	}
	journalPath := store.recoveryJournalPath()
	if len(journal.Slots) == 0 {
		if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := writeJSONAtomic(journalPath, journal); err != nil {
		return err
	}
	return writeJSONAtomic(store.Path, pool)
}

func (store LeaseStore) mergeRecoveryJournal(pool *LeasePool) error {
	payload, err := os.ReadFile(store.recoveryJournalPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal leaseRecoveryJournal
	if err := json.Unmarshal(payload, &journal); err != nil ||
		journal.SchemaVersion != LeaseRecoverySchemaVersion {
		return fmt.Errorf("%w: invalid lease recovery journal", ErrCorruptLeaseState)
	}
	seen := map[string]bool{}
	for _, recovery := range journal.Slots {
		if !brokerSlotID.MatchString(recovery.SlotID) ||
			seen[recovery.SlotID] ||
			recovery.ResourceGeneration == 0 ||
			(recovery.LastFailureEventID != "" &&
				!brokerRecordID.MatchString(recovery.LastFailureEventID)) ||
			(recovery.IncidentID != "" &&
				!brokerRecordID.MatchString(recovery.IncidentID)) {
			return fmt.Errorf("%w: invalid lease recovery metadata", ErrCorruptLeaseState)
		}
		switch recovery.State {
		case SlotAvailable, SlotQuarantined, SlotRecovering:
		default:
			return fmt.Errorf("%w: invalid lease recovery state", ErrCorruptLeaseState)
		}
		seen[recovery.SlotID] = true
		slot, err := findSlot(pool, recovery.SlotID)
		if err != nil ||
			slot.ResourceGeneration != recovery.ResourceGeneration ||
			slot.LeaseEpoch != recovery.LeaseEpoch ||
			slot.State != recovery.State {
			continue
		}
		slot.LastFailureEventID = recovery.LastFailureEventID
		slot.IncidentID = recovery.IncidentID
		slot.RecoveryAttempt = recovery.RecoveryAttempt
		slot.NextRecoveryAt = recovery.NextRecoveryAt
		slot.RecoveryStartedAt = recovery.RecoveryStartedAt
	}
	return nil
}

func slotHasRecoveryMetadata(slot LeaseSlot) bool {
	return slot.State == SlotQuarantined ||
		slot.State == SlotRecovering ||
		slot.LastFailureEventID != "" ||
		slot.IncidentID != "" ||
		slot.RecoveryAttempt != 0 ||
		!slot.NextRecoveryAt.IsZero() ||
		!slot.RecoveryStartedAt.IsZero()
}

func (store LeaseStore) rebuildCorruptPoolForRecovery(slotID string) error {
	number, err := slotNumber(slotID, store.SlotCount)
	if err != nil {
		return err
	}
	return store.withRawLock(func() error {
		if _, loadErr := store.load(); !errors.Is(loadErr, ErrCorruptLeaseState) &&
			!errors.Is(loadErr, ErrUnsupportedLeaseState) {
			if loadErr == nil {
				return errors.New("lease state changed while preparing recovery")
			}
			return loadErr
		}
		backup := fmt.Sprintf("%s.corrupt-%d", store.Path, store.now().UnixNano())
		journalPath := store.recoveryJournalPath()
		if _, err := os.Stat(journalPath); err == nil {
			if err := os.Rename(journalPath, backup+".recovery-v1.json"); err != nil {
				return fmt.Errorf("preserve corrupt lease recovery metadata: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(store.Path, backup); err != nil {
			return fmt.Errorf("preserve corrupt lease state: %w", err)
		}
		pool := LeasePool{
			SchemaVersion: LeaseSchemaVersion, ResourceType: "agent-e2e",
			ResourceID: "test-vms",
		}
		for index := 1; index <= store.SlotCount; index++ {
			state := SlotQuarantined
			reason := "lease state recovery required"
			if index == number {
				state = SlotQuarantined
				reason = "operator recovery"
			}
			pool.Slots = append(pool.Slots, LeaseSlot{
				SlotID: fmt.Sprintf("slot-%03d", index), ResourceGeneration: 1,
				State: state, FailureReason: reason, NextRecoveryAt: store.now(),
			})
		}
		return store.writePool(pool)
	})
}

func (store LeaseStore) withRawLock(operation func() error) error {
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(store.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return operation()
}

func reconcileSlotCount(pool *LeasePool, count int) error {
	sort.Slice(pool.Slots, func(i, j int) bool { return pool.Slots[i].SlotID < pool.Slots[j].SlotID })
	for len(pool.Slots) < count {
		index := len(pool.Slots) + 1
		pool.Slots = append(pool.Slots, LeaseSlot{
			SlotID: fmt.Sprintf("slot-%03d", index), ResourceGeneration: 1,
			State: SlotAvailable,
		})
	}
	if len(pool.Slots) > count {
		for _, slot := range pool.Slots[count:] {
			if slot.State != SlotAvailable {
				return fmt.Errorf("cannot shrink pool: retiring %s is %s", slot.SlotID, slot.State)
			}
		}
		return errors.New("pool shrink requires physical reconciliation")
	}
	return nil
}

func (store LeaseStore) PrepareResize() ([]LeaseSlot, error) {
	var retiring []LeaseSlot
	err := store.withPoolLock(func(pool *LeasePool) error {
		if len(pool.Slots) < store.SlotCount {
			if err := reconcileSlotCount(pool, store.SlotCount); err != nil {
				return err
			}
			return store.writePool(*pool)
		}
		if len(pool.Slots) == store.SlotCount {
			return nil
		}
		for index := store.SlotCount; index < len(pool.Slots); index++ {
			slot := &pool.Slots[index]
			if slot.State == SlotDraining && slot.FailureReason == "pool resize" {
				continue
			}
			if slot.State != SlotAvailable {
				return fmt.Errorf("cannot shrink pool: retiring %s is %s", slot.SlotID, slot.State)
			}
		}
		for index := store.SlotCount; index < len(pool.Slots); index++ {
			slot := &pool.Slots[index]
			slot.State = SlotDraining
			slot.FailureReason = "pool resize"
			retiring = append(retiring, *slot)
		}
		return store.writePool(*pool)
	})
	return retiring, err
}

func (store LeaseStore) ResizePlan() (int, []LeaseSlot, error) {
	current := 0
	var retiring []LeaseSlot
	err := store.withPoolLock(func(pool *LeasePool) error {
		current = len(pool.Slots)
		if current <= store.SlotCount {
			return nil
		}
		for index := store.SlotCount; index < current; index++ {
			slot := pool.Slots[index]
			if slot.State != SlotAvailable &&
				(slot.State != SlotDraining || slot.FailureReason != "pool resize") {
				return fmt.Errorf("cannot shrink pool: retiring %s is %s",
					slot.SlotID, slot.State)
			}
			retiring = append(retiring, slot)
		}
		return nil
	})
	return current, retiring, err
}

func (store LeaseStore) CommitResize() error {
	return store.withPoolLock(func(pool *LeasePool) error {
		if len(pool.Slots) < store.SlotCount {
			return errors.New("pool resize was not prepared")
		}
		for index := store.SlotCount; index < len(pool.Slots); index++ {
			slot := pool.Slots[index]
			if slot.State != SlotDraining || slot.FailureReason != "pool resize" {
				return fmt.Errorf("retiring %s is not fenced for pool resize", slot.SlotID)
			}
		}
		pool.Slots = pool.Slots[:store.SlotCount]
		return store.writePool(*pool)
	})
}

func expireStale(pool *LeasePool, now time.Time) {
	for index := range pool.Slots {
		slot := &pool.Slots[index]
		if slot.ExpiresAt.IsZero() || now.Before(slot.ExpiresAt) {
			continue
		}
		switch slot.State {
		case SlotHeld:
			slot.State = SlotDraining
			slot.FailureReason = heartbeatExpiredReason
		case SlotProvisioning:
			slot.State = SlotDraining
			slot.FailureReason = provisioningExpiredReason
		}
	}
}

func findSlot(pool *LeasePool, id string) (*LeaseSlot, error) {
	for index := range pool.Slots {
		if pool.Slots[index].SlotID == id {
			return &pool.Slots[index], nil
		}
	}
	return nil, fmt.Errorf("unknown slot %q", id)
}

func clearLease(slot *LeaseSlot) {
	slot.ClientID = ""
	slot.ControllerFingerprint = ""
	slot.DisplayLabel = ""
	slot.Yard = ""
	slot.Project = ""
	slot.Checkout = ""
	slot.Run = ""
	slot.Purpose = ""
	slot.LeaseID = ""
	slot.CapabilityHash = ""
	slot.AcquiredAt = time.Time{}
	slot.ProvisioningStartedAt = time.Time{}
	slot.ReadyAt = time.Time{}
	slot.LastHeartbeatAt = time.Time{}
	slot.ExpiresAt = time.Time{}
	slot.FailureReason = ""
	slot.LastFailureEventID = ""
	slot.IncidentID = ""
	slot.RecoveryAttempt = 0
	slot.NextRecoveryAt = time.Time{}
	slot.RecoveryStartedAt = time.Time{}
}

func legacyLeaseContext(label, purpose string) *LeaseContext {
	hash := strings.LastIndex(label, "#")
	at := strings.LastIndex(label, "@")
	if at <= 0 || hash <= at+1 || hash == len(label)-1 {
		return nil
	}
	context := LeaseContext{
		SchemaVersion: LeaseAttributionSchemaV1,
		Project:       label[:at],
		Checkout:      label[at+1 : hash],
		Run:           label[hash+1:],
		Purpose:       purpose,
	}
	if validateLeaseContext(context) != nil {
		return nil
	}
	return &context
}

func validateLeaseContext(context LeaseContext) error {
	if context.SchemaVersion != LeaseAttributionSchemaV1 &&
		context.SchemaVersion != LeaseAttributionSchemaVersion {
		return errors.New("unsupported lease attribution schema")
	}
	if context.SchemaVersion == LeaseAttributionSchemaV1 {
		if context.Yard != "" || context.Checkout == "" {
			return errors.New("invalid schema-1 lease attribution")
		}
	} else if context.Yard == "" || context.Checkout != "" {
		return errors.New("invalid schema-2 lease attribution")
	}
	for _, field := range []struct {
		name    string
		value   string
		maximum int
	}{
		{name: "project", value: context.Project, maximum: 50},
		{name: "run", value: context.Run, maximum: 24},
		{name: "purpose", value: context.Purpose, maximum: 80},
	} {
		if !safeLeaseLabel(field.value, field.maximum) {
			return fmt.Errorf("invalid lease attribution %s", field.name)
		}
	}
	if context.SchemaVersion == LeaseAttributionSchemaV1 &&
		!safeLeaseLabel(context.Checkout, 24) {
		return errors.New("invalid lease attribution checkout")
	}
	if context.SchemaVersion == LeaseAttributionSchemaVersion &&
		!safeCanonicalYard(context.Yard) {
		return errors.New("invalid lease attribution yard")
	}
	if context.SchemaVersion == LeaseAttributionSchemaVersion &&
		!domain.SafeProjectName(context.Project) {
		return errors.New("invalid canonical project attribution")
	}
	if !safeLeaseText(contextDisplayLabel(context), 80) {
		return errors.New("lease attribution is too long")
	}
	return nil
}

func safeCanonicalYard(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	if (value[0] < 'a' || value[0] > 'z') &&
		(value[0] < '0' || value[0] > '9') {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') &&
			character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func contextDisplayLabel(context LeaseContext) string {
	if context.SchemaVersion == LeaseAttributionSchemaVersion {
		return context.Project + "#" + context.Run
	}
	return context.Project + "@" + context.Checkout + "#" + context.Run
}

func safeLeaseLabel(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.Contains(value, "//") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "." || component == ".." {
			return false
		}
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case strings.ContainsRune("._/+-:", character):
		default:
			return false
		}
	}
	return true
}

func randomToken(bytes int) (string, error) {
	payload := make([]byte, bytes)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return hex.EncodeToString(payload), nil
}

func capabilityDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func safeLeaseText(value string, maximum int) bool {
	return len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n\t")
}

func boundedReason(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > 240 {
		value = value[:240]
	}
	return value
}

func writeJSONAtomic(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".lease-state.*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
