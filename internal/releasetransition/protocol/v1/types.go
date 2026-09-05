// Package v1 defines the frozen schema version 1 release-transition process
// protocol. Its wire types are deliberately independent from the current
// release-transition engine model.
package v1

const (
	SchemaVersion = 1

	SourceIngressSchemaVersion = 1
	SourceIngressPreGo         = "pre-go-source-v1"

	ModeInspect  = "inspect"
	ModeConverge = "converge"

	DirectionActivateTarget   = "activate-target"
	DirectionActivatePrevious = "activate-previous"

	StatusReady                  = "ready"
	StatusMigrationRequired      = "migration-required"
	StatusRecovering             = "recovering"
	StatusOperatorActionRequired = "operator-action-required"

	CodeReady                 = "ready"
	CodeTransitionRequired    = "transition-required"
	CodeRecoveryPending       = "recovery-pending"
	CodeRegistryInvalid       = "registry-invalid"
	CodeUnsupportedEpoch      = "unsupported-epoch"
	CodeUnsupportedKind       = "unsupported-kind"
	CodeConfirmationRequired  = "confirmation-required"
	CodeOperationDeclined     = "operation-declined"
	CodePlanStale             = "plan-stale"
	CodeMigrationStale        = "migration-stale"
	CodeResourceConflict      = "resource-conflict"
	CodePreconditionBlocked   = "precondition-blocked"
	CodeDependencyUnavailable = "dependency-unavailable"
	CodeVerificationFailed    = "verification-failed"
	CodeActivationAmbiguous   = "activation-ambiguous"
	CodeRecoveryAmbiguous     = "recovery-ambiguous"
	CodeJournalInvalid        = "journal-invalid"
	CodeRollbackIncompatible  = "rollback-incompatible"
	CodeRollbackExpired       = "rollback-expired"

	DecisionPreserve     = "preserve"
	DecisionTransform    = "transform"
	DecisionCanonicalize = "canonicalize"
	DecisionReset        = "reset"
	DecisionQuarantine   = "quarantine"
	DecisionBlock        = "block"

	ActionEffectRead         = "read"
	ActionEffectSession      = "session"
	ActionEffectBoundedWrite = "bounded-write"
	ActionEffectMutation     = "mutation"
	ActionEffectDestruction  = "destruction"

	ActionImpactLocalMetadata  = "local-metadata"
	ActionImpactYardRuntime    = "yard-runtime"
	ActionImpactHostOS         = "host-os"
	ActionImpactHostNetwork    = "host-network"
	ActionImpactHostIncus      = "host-incus"
	ActionImpactSecurity       = "security"
	ActionImpactTrust          = "trust"
	ActionImpactAccess         = "access"
	ActionImpactExternalSystem = "external-system"
	ActionImpactSharedWorkload = "shared-workload"
	ActionImpactPersistentData = "persistent-data"

	RecoveryNotNeeded    = "not-needed"
	RecoveryReversible   = "reversible"
	RecoveryRecreatable  = "recreatable"
	RecoveryIrreversible = "irreversible"

	JournalReplacementPreActivationPlanStale   = "pre-activation-plan-stale"
	JournalReplacementPostActivationScopeV0111 = "post-activation-scope-v0.11.1"
)

type SourceIngressRequest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Kind          string `json:"kind"`
	SourceRoot    string `json:"sourceRoot"`
	DataHome      string `json:"dataHome"`
	BinDir        string `json:"binDir"`
	RC            string `json:"rc"`
	LoginRC       string `json:"loginRC"`
}

type Request struct {
	SchemaVersion       int                   `json:"schemaVersion"`
	Mode                string                `json:"mode"`
	RuntimeRoot         string                `json:"runtimeRoot"`
	ConfigHome          string                `json:"configHome"`
	Yard                string                `json:"yard"`
	Target              string                `json:"target"`
	Direction           string                `json:"direction"`
	ArtifactDigest      string                `json:"artifactDigest"`
	RegistryDigest      string                `json:"registryDigest,omitempty"`
	InheritedSettingIDs []string              `json:"inheritedSettingIds,omitempty"`
	SourceIngress       *SourceIngressRequest `json:"sourceIngress,omitempty"`
	Replacement         *JournalReplacement   `json:"replacement,omitempty"`
	Execution           *Execution            `json:"execution,omitempty"`
}

type Response struct {
	SchemaVersion                 int         `json:"schemaVersion"`
	ActivationReconciliationOwned bool        `json:"activationReconciliationOwned"`
	Inspection                    *Inspection `json:"inspection,omitempty"`
	Outcome                       *Outcome    `json:"outcome,omitempty"`
}

type Goal struct {
	Target    string `json:"target"`
	Direction string `json:"direction"`
}

type ActionAssessment struct {
	Action       string   `json:"action"`
	Effect       string   `json:"effect"`
	Changed      bool     `json:"changed"`
	Impacts      []string `json:"impacts,omitempty"`
	Recovery     string   `json:"recovery"`
	Consequences []string `json:"consequences,omitempty"`
}

type RedactedDecision struct {
	Resource string `json:"resource"`
	Scope    string `json:"scope,omitempty"`
	Decision string `json:"decision"`
	Result   string `json:"result,omitempty"`
}

type Blocker struct {
	Code     string `json:"code"`
	Resource string `json:"resource,omitempty"`
	Message  string `json:"message"`
	Retry    string `json:"retry"`
}

type Inspection struct {
	Plan       string             `json:"plan"`
	Assessment ActionAssessment   `json:"assessment"`
	Decisions  []RedactedDecision `json:"decisions,omitempty"`
	Blockers   []Blocker          `json:"blockers,omitempty"`
	Resume     *string            `json:"resume,omitempty"`
	Outcome    *Outcome           `json:"outcome,omitempty"`
}

type Execution struct {
	Plan          string `json:"plan"`
	Authorization string `json:"authorization"`
}

type Outcome struct {
	Status      string   `json:"status"`
	ReachedGoal bool     `json:"reachedGoal"`
	Active      string   `json:"active"`
	Previous    *string  `json:"previous,omitempty"`
	Target      string   `json:"target"`
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	Retry       string   `json:"retry,omitempty"`
	Transaction *string  `json:"transaction,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

type JournalReplacement struct {
	Transaction   string `json:"transaction"`
	Fingerprint   string `json:"fingerprint"`
	Reason        string `json:"reason"`
	SourceVersion string `json:"sourceVersion,omitempty"`
}
