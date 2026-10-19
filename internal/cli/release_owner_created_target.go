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
	if !ok || !sameCreatedActivationSource(prepared.value, resolved) {
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
	if err = payload.ShareSources(c.retainPreparedSource); err != nil {
		return errors.Join(err, payload.Close())
	}
	prepared.nativeInstalled = payload
	if broker := payload.PreparedBroker(); broker != nil {
		prepared.broker = broker
	}
	prepared.value = resolved
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
func sameCreatedActivationSource(current, original releaseowner.Target) bool {
	current.PreparedRecipe, original.PreparedRecipe = "", ""
	return sameNativeActivationScope(current, original)
}
