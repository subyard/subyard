package releasetransition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	ownerRegistrationResource   = "test-yard-owner"
	legacyOwnerRegistrationYard = "e2e-yard"
)

// OwnerRegistrationState is a closed, redacted description of the persisted
// test-yard owner topology. It deliberately carries neither paths nor config
// values across the release-transition boundary.
type OwnerRegistrationState string
type OwnerRegistrationProgress string

// OwnerRegistrationObservation binds the closed topology class to the exact
// registration bytes and project image namespace needed by Commit. Registration
// is an opaque digest; raw configuration never crosses the port.
type OwnerRegistrationObservation struct {
	State        OwnerRegistrationState `json:"state"`
	Registration Fingerprint            `json:"registration"`
	Overrides    Fingerprint            `json:"overrides"`
	Controller   Fingerprint            `json:"controller"`
	SharedImages bool                   `json:"sharedImages"`

	// These fields are the closed runtime binding from the outer recovery
	// record. They never enter a plan fingerprint or serialized observation.
	RecoveryToken   string `json:"-"`
	TerminalCleanup bool   `json:"-"`
}

const (
	OwnerRegistrationAbsent                      OwnerRegistrationState = "absent"
	OwnerRegistrationLegacyDirectory             OwnerRegistrationState = "legacy-directory"
	OwnerRegistrationLegacyDirectoryProjects     OwnerRegistrationState = "legacy-directory+projects"
	OwnerRegistrationLegacyDirectoryOverrides    OwnerRegistrationState = "legacy-directory+overrides"
	OwnerRegistrationLegacyDirectoryState        OwnerRegistrationState = "legacy-directory+projects+overrides"
	OwnerRegistrationLegacyFlat                  OwnerRegistrationState = "legacy-flat"
	OwnerRegistrationLegacyDirectoryAdoptCurrent OwnerRegistrationState = "legacy-directory-adopt-current"
	OwnerRegistrationLegacyFlatAdoptCurrent      OwnerRegistrationState = "legacy-flat-adopt-current"
	OwnerRegistrationCurrent                     OwnerRegistrationState = "current"
)

const (
	OwnerRegistrationExpected   OwnerRegistrationProgress = "expected"
	OwnerRegistrationInProgress OwnerRegistrationProgress = "in-progress"
	OwnerRegistrationDesired    OwnerRegistrationProgress = "desired"
)

// V2OwnerRegistration is the trusted mutation port for the compiled
// owner-registration capability. Prepare is the fresh read-only assessment;
// prospective settings let source ingress expose the exact registration it
// will import before that import is authorized.
// Observe accepts intermediate progress only when bound to its journaled
// before-state. Commit is called only after that exact state was authorized.
type V2OwnerRegistration interface {
	Prepare(context.Context, V2SettingsSnapshotView) (OwnerRegistrationObservation, error)
	Observe(context.Context, OwnerRegistrationObservation) (OwnerRegistrationProgress, error)
	Commit(context.Context, OwnerRegistrationObservation) error
}

type ownerRegistrationV2Plan struct {
	Before              OwnerRegistrationObservation
	ExpectedFingerprint Fingerprint
	DesiredFingerprint  Fingerprint
}

type ownerRegistrationRecoveryV1 struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Transaction   TransactionID                `json:"transaction"`
	Step          string                       `json:"step"`
	Migration     string                       `json:"migration"`
	Resource      string                       `json:"resource"`
	Expected      Fingerprint                  `json:"expectedFingerprint"`
	Token         string                       `json:"token"`
	Before        OwnerRegistrationObservation `json:"before"`
}

func inspectOwnerRegistrationV2(
	ctx context.Context,
	port V2OwnerRegistration,
	prospective V2SettingsSnapshotView,
) (*ownerRegistrationV2Plan, error) {
	if port == nil {
		return nil, fmt.Errorf("owner-registration capability is unavailable")
	}
	observation, err := port.Prepare(ctx, prospective)
	if err != nil {
		return nil, err
	}
	if err := validateOwnerRegistrationObservation(observation); err != nil {
		return nil, err
	}
	if observation.State == OwnerRegistrationAbsent || observation.State == OwnerRegistrationCurrent {
		return nil, nil
	}
	desired := observation
	desired.State = OwnerRegistrationCurrent
	return &ownerRegistrationV2Plan{
		Before:              observation,
		ExpectedFingerprint: ownerRegistrationFingerprint(observation),
		DesiredFingerprint:  ownerRegistrationFingerprint(desired),
	}, nil
}

func validateOwnerRegistrationObservation(observation OwnerRegistrationObservation) error {
	if err := validateOwnerRegistrationState(observation.State); err != nil {
		return err
	}
	if observation.State == OwnerRegistrationAbsent {
		if observation.Registration != "" || observation.Overrides != "" ||
			observation.Controller != "" || observation.SharedImages {
			return invalid("absent owner-registration observation carries resource identity")
		}
		return nil
	}
	if err := validateFingerprint(observation.Registration, "owner-registration identity"); err != nil {
		return err
	}
	if err := validateFingerprint(observation.Overrides, "owner overrides identity"); err != nil {
		return err
	}
	if err := validateFingerprint(observation.Controller, "owner controller identity"); err != nil {
		return err
	}
	return nil
}

func validateOwnerRegistrationState(state OwnerRegistrationState) error {
	switch state {
	case OwnerRegistrationAbsent,
		OwnerRegistrationLegacyDirectory,
		OwnerRegistrationLegacyDirectoryProjects,
		OwnerRegistrationLegacyDirectoryOverrides,
		OwnerRegistrationLegacyDirectoryState,
		OwnerRegistrationLegacyFlat,
		OwnerRegistrationLegacyDirectoryAdoptCurrent,
		OwnerRegistrationLegacyFlatAdoptCurrent,
		OwnerRegistrationCurrent:
		return nil
	default:
		return invalid("unknown owner-registration state %q", state)
	}
}

func ownerRegistrationFingerprint(observation OwnerRegistrationObservation) Fingerprint {
	observation.RecoveryToken = ""
	observation.TerminalCleanup = false
	payload, _ := json.Marshal(observation)
	return fingerprintPayload(payload)
}

func desiredOwnerRegistrationObservation(
	before OwnerRegistrationObservation,
) OwnerRegistrationObservation {
	desired := before
	desired.State = OwnerRegistrationCurrent
	return desired
}

func ownerRegistrationProgressFingerprint(
	progress OwnerRegistrationProgress,
	expected Fingerprint,
	desired Fingerprint,
) (Fingerprint, error) {
	switch progress {
	case OwnerRegistrationExpected, OwnerRegistrationInProgress:
		return expected, nil
	case OwnerRegistrationDesired:
		return desired, nil
	default:
		return "", invalid("unknown owner-registration progress %q", progress)
	}
}

func validateOwnerRegistrationRecovery(
	recovery ownerRegistrationRecoveryV1,
	journal JournalRecord,
	step JournalStep,
) error {
	if recovery.SchemaVersion != 1 || recovery.Transaction != journal.Transaction ||
		recovery.Step != step.ID || recovery.Migration != step.Migration ||
		recovery.Resource != ownerRegistrationResource ||
		recovery.Expected != step.Expected || recovery.Before.State == OwnerRegistrationAbsent ||
		recovery.Before.State == OwnerRegistrationCurrent ||
		ownerRegistrationFingerprint(recovery.Before) != step.Expected {
		return invalid("typed owner-registration recovery evidence is invalid")
	}
	if err := validateOwnerRecoveryToken(recovery.Token); err != nil {
		return err
	}
	return validateOwnerRegistrationObservation(recovery.Before)
}

func newOwnerRecoveryToken() (string, error) {
	payload := make([]byte, 16)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return hex.EncodeToString(payload), nil
}

func validateOwnerRecoveryToken(token string) error {
	if len(token) != 32 {
		return invalid("owner-registration recovery token is invalid")
	}
	for _, character := range token {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return invalid("owner-registration recovery token is invalid")
		}
	}
	return nil
}
