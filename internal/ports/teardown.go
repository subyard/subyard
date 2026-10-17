package ports

import (
	"context"
	"fmt"
	"github.com/Subyard/Subyard/internal/domain"
)

// TeardownInventoryReader provides read-only identities at the physical owner.
// It is optional so generic Incus consumers need not implement deletion policy.
type TeardownInventoryReader interface {
	TeardownInventory(context.Context, string) ([]TeardownResource, error)
}

type TeardownResource struct {
	Kind          string                     `json:"kind"`
	Name          string                     `json:"name"`
	Pool          string                     `json:"pool,omitempty"`
	Binding       string                     `json:"binding"`
	SnapshotCount int                        `json:"snapshotCount,omitempty"`
	Path          string                     `json:"path,omitempty"`
	UsedBy        []string                   `json:"usedBy,omitempty"`
	Bindings      []string                   `json:"bindings,omitempty"`
	Owner         *InstallationResourceOwner `json:"owner,omitempty"`
}

type InstallationResourceOwner struct {
	UID        string
	DataHome   string
	ConfigHome string
	Context    *domain.Context
}

type InstallationSharedReader interface {
	InstallationSharedInventory(context.Context, string, string) ([]TeardownResource, error)
}

// TeardownArtifact binds an approved path to metadata only, never file contents.
type TeardownArtifact struct {
	Path       string
	Binding    string
	Entries    []ArtifactEntry
	Opaque     bool              `json:",omitempty"`
	Exclusions []string          `json:",omitempty"`
	Category   string            `json:",omitempty"`
	Preserved  []string          `json:",omitempty"`
	Resource   *TeardownResource `json:",omitempty"`
	Scoped     bool              `json:",omitempty"`
	OwnedPaths []string          `json:",omitempty"`
	Namespaces []ArtifactEntry   `json:",omitempty"`
	Mutable    bool              `json:",omitempty"`
}

// ArtifactEntry is the original metadata observed by the native exact guard.
type ArtifactEntry struct {
	Path     string
	Mode     uint32
	Size     int64
	Modified int64
	Device   uint64
	Inode    uint64
	UID      uint32
	GID      uint32
	Changed  int64
	Link     string
}

// ArtifactBindings keeps the existing bounded environment transport metadata-
// independent. Full cleanup resolves original entries from its protected plan.
type TeardownArtifactBinding struct {
	Path    string
	Binding string
}

func ArtifactBindings(artifacts []TeardownArtifact) []TeardownArtifactBinding {
	bindings := make([]TeardownArtifactBinding, len(artifacts))
	for i, artifact := range artifacts {
		bindings[i] = TeardownArtifactBinding{Path: artifact.Path, Binding: artifact.Binding}
	}
	return bindings
}

// TeardownPlanStaleExitCode carries the physical guard's typed refusal through
// shell and profile-service subprocesses without parsing their diagnostics.
const TeardownPlanStaleExitCode = 75

// CheckTeardownResources permits disappearing approved resources, never added
// identities or altered ownership/content metadata before destructive apply.
func CheckTeardownResources(approved, current []TeardownResource) error {
	original := make(map[string]string, len(approved))
	for _, resource := range approved {
		original[resource.Kind+"/"+resource.Pool+"/"+resource.Name] = resource.Binding
	}
	for _, resource := range current {
		if original[resource.Kind+"/"+resource.Pool+"/"+resource.Name] != resource.Binding {
			return fmt.Errorf("%w: teardown resource identities or binding changed", domain.ErrPlanStale)
		}
	}
	return nil
}

// Shared infrastructure is observed separately from project-owned resources.
type TeardownSharedInventoryReader interface {
	TeardownSharedInventory(context.Context, string, string) ([]TeardownResource, error)
}

// InstallationYardReader discovers registered and partially created projects
// through persisted installation ownership, without deriving deletion targets
// from names or the current configuration defaults.
type InstallationYardReader interface {
	InstallationYards(context.Context) ([]InstallationYard, error)
}

type InstallationYard struct {
	Context     *domain.Context
	Project     string
	Name        string
	Instance    string
	OperatorUID string
	DataHome    string
	ConfigHome  string
	HostBase    string
	Bridge      string
	Pool        string
	Volume      string
	StoragePool string
	StoragePath string
}
