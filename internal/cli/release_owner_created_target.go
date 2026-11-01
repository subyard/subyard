package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner"
)

// BindInstalledRuntimeRecipeForTarget retains the approved payload and target.
// Fresh observations validate its immutable scope; they cannot replace it.
func (c *releaseOwnerComponents) BindInstalledRuntimeRecipeForTarget(ctx context.Context, hash string, raw []byte, original releaseowner.Target) error {
	digest := sha256.Sum256(raw)
	if original.PreparedRecipe != hash || hex.EncodeToString(digest[:]) != hash {
		return domain.ErrPlanStale
	}
	if original.AfterOwner != "" {
		if err := c.bindAfterOwnerRecipe(ctx, hash, raw, original); err != nil {
			return err
		}
		if c.installedBound == nil {
			c.installedBound = map[string]bool{}
		}
		c.installedBound[hash] = true
		return nil
	}
	resolved := original
	if original.InstanceUUID == "absent" {
		var err error
		resolved, err = c.ResolveOwnerTarget(ctx, original)
		if err != nil {
			return err
		}
	}
	key := original.Owner + ":" + original.Yard + ":" + original.Class
	prepared, ok := c.activation[key]
	if !ok {
		return domain.ErrPlanStale
	}
	runtime, ok := prepared.platform.(reconcileruntime.Runtime)
	if !ok {
		return domain.ErrPlanStale
	}
	payload, err := runtime.BindInstalledRuntimeContext(ctx, raw)
	if err != nil {
		return err
	}
	if payload.Binding() != hash {
		return errors.Join(domain.ErrPlanStale, payload.Close())
	}
	if !sameCreatedActivationSource(prepared.value, resolved) && original.Class == "materialized-integrations" && original.InstanceUUID != "absent" {
		prepared, err = c.compareOriginalIntegration(ctx, prepared, original, payload)
		if err != nil {
			return errors.Join(err, payload.Close())
		}
	}
	if !sameCreatedActivationSource(prepared.value, resolved) {
		return errors.Join(domain.ErrPlanStale, payload.Close())
	}
	if original.Class == "materialized-files" && original.InstanceUUID != "absent" {
		mixed := prepared.value.Actual != original.Actual || prepared.value.ReportedActual != original.ReportedActual || prepared.value.State != original.State
		if mixed && !payload.HasCurrentMaterializedCustody() || payload.HasCurrentMaterializedCustody() && (payload.PreparedConfigs() == nil || payload.PreparedConfigs().InstanceUUID() != original.InstanceUUID || (original.State == "stale" || original.Before != "") && payload.PreparedConfigs().Binding() != original.Before) {
			return errors.Join(domain.ErrPlanStale, payload.Close())
		}
		if payload.HasCurrentMaterializedCustody() {
			prepared.nativeConfigs = payload.PreparedConfigs()
		}
	}
	if original.Class == "materialized-integrations" && original.State == "stale" && original.InstanceUUID != "absent" {
		native := payload.PreparedIntegrations()
		if native == nil || native.Binding() != original.Before || reconcileruntime.CheckIntegrationPlan(native.Plan(), prepared.integration) != nil {
			return errors.Join(domain.ErrPlanStale, payload.Close())
		}
		prepared.nativeIntegrations = native
	}
	if err = payload.ShareSources(c.retainPreparedSource); err != nil {
		return errors.Join(err, payload.Close())
	}
	prepared.nativeInstalled = payload
	if broker := payload.PreparedBroker(); broker != nil {
		prepared.broker = broker
	}
	prepared.value = resolved
	if original.InstanceUUID != "absent" && (original.Class == "materialized-integrations" || original.Owner == "core-guest-runtime") {
		value := original
		prepared.originalTarget = &value
		if c.installedRecipes == nil {
			c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
		}
		c.installedRecipes[hash] = payload
	}
	if original.InstanceUUID == "absent" && resolved.InstanceUUID != "absent" {
		value := original
		prepared.originalTarget = &value
	}
	c.activation[key] = prepared
	c.closers = append(c.closers, payload)
	if c.installedBound == nil {
		c.installedBound = map[string]bool{}
	}
	c.installedBound[hash] = true
	return nil
}

// integrationInput is only the fresh input read-set. The original authenticated
// capsule and target remain authority; a comparison never replaces their bytes.
func (c *releaseOwnerComponents) compareOriginalIntegration(ctx context.Context, current releaseActivationPrepared, original releaseowner.Target, payload *reconcileruntime.PreparedInstalledRuntime) (releaseActivationPrepared, error) {
	runtime, ok := current.platform.(reconcileruntime.Runtime)
	if !ok || payload == nil || payload.Binding() != original.PreparedRecipe || payload.PreparedIntegrations() == nil ||
		current.value.Owner != original.Owner || current.value.Class != original.Class || current.value.Yard != original.Yard ||
		current.value.Project != original.Project || current.value.Instance != original.Instance || current.value.InstanceUUID != original.InstanceUUID || original.InstanceUUID == "absent" {
		return current, domain.ErrPlanStale
	}
	// Rebind even on later captures: source bytes, identities and runtime inputs
	// must still validate the same original capsule before projecting any output.
	checked, err := runtime.RebindPreparedInstalledRuntime(ctx, payload)
	if err != nil {
		return current, err
	}
	defer checked.Close()
	var core *reconcileruntime.PreparedInstalledRuntime
	var desired string
	sibling := c.activation["core-guest-runtime:"+original.Yard+":profile-runtime"]
	if target := sibling.originalTarget; target != nil && target.Owner == "core-guest-runtime" && target.Yard == original.Yard &&
		target.Project == original.Project && target.Instance == original.Instance && target.InstanceUUID == original.InstanceUUID && c.installedBound[target.PreparedRecipe] {
		candidate := c.installedRecipes[target.PreparedRecipe]
		source, sourceErr := runtime.SelectedRuntimeInputs("core-guest-runtime")
		if sourceErr != nil {
			return current, sourceErr
		}
		if candidate != nil && candidate.Binding() == target.PreparedRecipe && nativeValueDigest(struct{ Source, Recipe string }{source, candidate.Binding()}) == target.Source {
			core, desired = candidate, target.Desired
		}
	}
	approved := checked.PreparedIntegrations().Plan()
	compared, err := runtime.ProjectIntegrationPlanAfterCoreRuntime(ctx, core, desired, approved, current.integration)
	if err == nil {
		compared, err = runtime.ProjectIntegrationPlanAfterProfileActivation(ctx, approved, compared)
	}
	if err == nil {
		err = reconcileruntime.CheckIntegrationPlan(approved, compared)
	}
	if err != nil {
		return current, err
	}
	source := nativeValueDigest(struct{ Input, Scope string }{current.integrationInput, compared.Scope})
	source = nativeValueDigest(struct{ Source, Component string }{source, payload.MaterializedSourceBinding()})
	if source != original.Source || nativeValueDigest(struct{ Scope string }{compared.Scope}) != original.Desired {
		return current, domain.ErrPlanStale
	}
	current.value.Source, current.value.Desired, current.value.PreparedRecipe = source, original.Desired, original.PreparedRecipe
	current.value.Before = original.Before
	current.value.ReportedActual, current.value.ReportedDesired = compared.Fingerprint, compared.Scope
	current.value.Actual = original.Desired
	if compared.Changed {
		current.value.Actual = compared.Fingerprint
	}
	current.integration, current.nativeInstalled, current.nativeIntegrations = compared, payload, payload.PreparedIntegrations()
	value := original
	current.originalTarget = &value
	return current, nil
}
func sameCreatedActivationSource(current, original releaseowner.Target) bool {
	current.PreparedRecipe, original.PreparedRecipe = "", ""
	return sameNativeActivationScope(current, original)
}
