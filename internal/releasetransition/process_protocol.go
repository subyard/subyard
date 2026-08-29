package releasetransition

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

const ProcessProtocolSchemaV1 = 1

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
	Execution           *Execution            `json:"execution,omitempty"`
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
