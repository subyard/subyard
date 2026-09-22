package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/blang/semver/v4"
)

const ProcessProtocolSchemaV1 = 1

// InspectProcessV1 presents a new activation repair in the frozen caller's
// vocabulary. Inspect remains the canonical Module interface; both bind the
// same fresh plan to the same observation and never resume completed history.
func (transition *V2Transition) InspectProcessV1(ctx context.Context, goal Goal) (Inspection, error) {
	return transition.inspect(ctx, goal, true)
}

const SourceIngressRequestSchemaV1 = 1

type SourceIngressRequestKind string

const SourceIngressPreGoV1 SourceIngressRequestKind = "pre-go-source-v1"

// SourceIngressRequest carries only bounded path roles needed to rediscover a
// source installation. The trusted candidate adapter separately anchors these
// roles to the operating-system account home before use.
type SourceIngressRequest struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Kind          SourceIngressRequestKind `json:"kind"`
	SourceRoot    string                   `json:"sourceRoot"`
	DataHome      string                   `json:"dataHome"`
	BinDir        string                   `json:"binDir"`
	RC            string                   `json:"rc"`
	LoginRC       string                   `json:"loginRC"`
}

type ProcessMode string

const (
	ProcessInspect  ProcessMode = "inspect"
	ProcessConverge ProcessMode = "converge"
)

// ProcessRequest is the bounded candidate-runtime protocol used by the active
// updater. It carries only non-secret identities and an opaque authorization;
// the authorization verifier itself is injected through a separate trusted
// process boundary.
type ProcessRequest struct {
	SchemaVersion       int                   `json:"schemaVersion"`
	Mode                ProcessMode           `json:"mode"`
	RuntimeRoot         string                `json:"runtimeRoot"`
	ConfigHome          string                `json:"configHome"`
	Yard                string                `json:"yard"`
	Target              ReleaseID             `json:"target"`
	Direction           Direction             `json:"direction"`
	ArtifactDigest      Fingerprint           `json:"artifactDigest"`
	RegistryDigest      Fingerprint           `json:"registryDigest,omitempty"`
	InheritedSettingIDs []string              `json:"inheritedSettingIds,omitempty"`
	SourceIngress       *SourceIngressRequest `json:"sourceIngress,omitempty"`
	Replacement         *JournalReplacement   `json:"replacement,omitempty"`
	Execution           *Execution            `json:"execution,omitempty"`
}

// RollbackTarget is internal compatibility proof derived from the sealed
// retained runtime. Version is the engine semantic version without a v prefix;
// an empty registry digest proves that the target predates registry v2.
type RollbackTarget struct {
	Version        string      `json:"version"`
	RegistryDigest Fingerprint `json:"registryDigest,omitempty"`
}

func (target RollbackTarget) Validate() error {
	version, err := semver.Parse(target.Version)
	if err != nil || version.String() != target.Version {
		return invalid("rollback target version is not canonical semantic version")
	}
	if target.RegistryDigest != "" {
		return validateFingerprint(target.RegistryDigest, "rollback target registry digest")
	}
	return nil
}

func validateRollbackTarget(direction Direction, target *RollbackTarget) error {
	if direction == DirectionActivatePrevious {
		if target == nil {
			return invalid("rollback target facts are required")
		}
		return target.Validate()
	}
	if target != nil {
		return invalid("rollback target facts are forbidden for a forward goal")
	}
	return nil
}

func cloneRollbackTarget(target *RollbackTarget) *RollbackTarget {
	if target == nil {
		return nil
	}
	clone := *target
	return &clone
}

type ProcessResponse struct {
	SchemaVersion                 int         `json:"schemaVersion"`
	ActivationReconciliationOwned bool        `json:"activationReconciliationOwned"`
	Inspection                    *Inspection `json:"inspection,omitempty"`
	Outcome                       *Outcome    `json:"outcome,omitempty"`
}

func cloneSourceIngressRequest(request *SourceIngressRequest) *SourceIngressRequest {
	if request == nil {
		return nil
	}
	clone := *request
	return &clone
}

func (request SourceIngressRequest) Validate() error {
	if request.SchemaVersion != SourceIngressRequestSchemaV1 ||
		request.Kind != SourceIngressPreGoV1 {
		return errors.New("unknown source ingress descriptor schema or kind")
	}
	roles := []struct {
		name string
		path string
	}{
		{"source root", request.SourceRoot},
		{"data home", request.DataHome},
		{"launcher directory", request.BinDir},
		{"interactive shell rc", request.RC},
		{"login shell rc", request.LoginRC},
	}
	for _, role := range roles {
		if err := validateSourceIngressRolePath(role.path); err != nil {
			return fmt.Errorf("%s is invalid: %w", role.name, err)
		}
	}
	return nil
}

func validateSourceIngressRolePath(path string) error {
	const maximumSourceIngressPath = 4096
	if path == "" || len(path) > maximumSourceIngressPath || !filepath.IsAbs(path) ||
		path == string(filepath.Separator) || filepath.Clean(path) != path ||
		strings.ContainsFunc(path, unicode.IsControl) {
		return errors.New("path must be clean, absolute, non-root, and bounded")
	}
	return nil
}
