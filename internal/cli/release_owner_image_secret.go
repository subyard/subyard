package cli

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
)

// DrainOwnerImageSecret selects only a policy in the already-bound original
// owner capsule. The native session separately requires both original leases.
func (c *releaseOwnerComponents) DrainOwnerImageSecret(ctx context.Context, operation ports.NativeOperation) (ports.NativeOperation, error) {
	original := c.ownerChildren
	if original == nil || !c.installedBound[original.binding()] {
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	index, choice := -1, ""
	switch operation.Kind {
	case "owner-image-secret-current-primary":
		index, choice = 0, "primary"
	case "owner-image-secret-current-fallback":
		index, choice = 0, "fallback"
	case "owner-image-secret-legacy-primary":
		index, choice = 1, "primary"
	case "owner-image-secret-legacy-fallback":
		index, choice = 1, "fallback"
	case "owner-image-secret-recreate-primary":
		index, choice = 2, "primary"
	case "owner-image-secret-recreate-fallback":
		index, choice = 2, "fallback"
	default:
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	var record ownerChildrenRecord
	for _, file := range original.wire.Files {
		if file.Path == "prepared/owner-children.json" {
			if err := json.Unmarshal(file.Content, &record); err != nil {
				return ports.NativeOperation{}, err
			}
		}
	}
	if len(record.Inits) != 3 || !ownerInitChildSlot(record.Inits[index], index) {
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	child := record.Inits[index]
	if child.Instance == nil || child.Instance.Policy == nil {
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	policy := *child.Instance.Policy
	selected := policy.Primary
	if choice == "fallback" {
		selected = policy.Fallback
	}
	if len(selected) == 0 || len(operation.Resource) != 64 || strings.Trim(operation.Resource, "0123456789abcdef") != "" {
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	var metadata ownerInitRecord
	if err := nativejson.DecodeLosslessNativeJSON(child.Metadata, &metadata, 32<<20); err != nil {
		return ports.NativeOperation{}, err
	}
	loaded, err := metadata.Input.restore(c.cli.catalog)
	if err != nil || loaded.Context.YardName != child.Yard || loaded.Context.Paths.DataHome != c.context.DataHome || loaded.Context.Paths.ConfigHome != c.context.ConfigHome {
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	loaded = ownerLoadedLabels(loaded, c.context.RepositoryRoot, c.cli.options.RepositoryRoot)
	runtime, ok := c.platform(loaded, nil).(reconcileruntime.Runtime)
	if !ok {
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	environment := append([]string(nil), runtime.Environment...)
	environment = append(environment, "INCUS_PROJECT="+loaded.Context.IncusProject)
	incus, _ := c.cli.statusPorts()
	drain, ok := incus.(interface {
		DrainOwnerImageSecretOperation(context.Context, ports.PreparedOwnerImagePolicy, string, []string, string, ports.NativeOperation) (ports.NativeOperation, error)
	})
	if !ok {
		return ports.NativeOperation{}, domain.ErrPlanStale
	}
	return drain.DrainOwnerImageSecretOperation(ctx, policy, string(selected), environment, loaded.Context.Paths.OperatorHome, operation)
}
