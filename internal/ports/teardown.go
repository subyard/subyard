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
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Pool    string `json:"pool,omitempty"`
	Binding string `json:"binding"`
}

// TeardownArtifact binds an approved path to metadata only, never file contents.
type TeardownArtifact struct {
	Path    string
	Binding string
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
