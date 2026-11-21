package reconcileruntime

import (
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type materializedIntegrationPlan struct {
	Plan           IntegrationPlan
	Scope, Sources string
	Facts          []integrationFact
	PackageSources map[string]string
	Instance       ports.InstanceInfo
	InstanceScope  string
	Fingerprints   []string
	ProfileRoutes  []integrationProfileRoute
}

type materializedHookPlan struct {
	Approved, Current, Provisioned               hookObservation
	Conditional, Unavailable, Completed, Stopped bool
	AllowedHooks                                 []string
	Source, ResolvedProjects, ExecutionWiring    string
	OwnerProjects                                []domain.ProjectRecord
	HookBindings                                 map[string]string
	IntegrationScope                             string
	Metadata                                     *projectMetadataPlan
	MetadataCompleted                            bool
}

type materializedExecutionSource struct {
	Layout, Payload, Identity string
	Source                    []byte
	Projected                 bool
}

type materializedEnvironmentSource struct {
	Name, Payload, Identity string
	Source                  []byte
	Projected               bool
}

type materializedIntegrations struct {
	Binding, Runtime, Skipped string
	Plan                      materializedIntegrationPlan
	Entries                   []materializedArtifact
	Hooks                     *materializedHookPlan
	Sources                   []materializedExecutionSource
	Environment               []materializedEnvironmentSource
}

func validMaterializedLayout(path string) bool {
	return path != "." && path != ".." && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, "../") && len(path) <= 512
}
