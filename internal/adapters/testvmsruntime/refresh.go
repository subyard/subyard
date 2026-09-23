package testvmsruntime

import (
	"context"
	"errors"
	"time"
)

// RefreshBase builds a new immutable generation without acquiring or replacing a
// lease. Publication is serialized with allocation and GC; existing base pins
// and the prior current base survive failure.
func (rt *Runtime) RefreshBase(ctx context.Context, store LeaseStore, name string) error {
	rt.prepareDefaults()
	spec, err := rt.Config.EnvironmentSpec(name)
	if err != nil {
		return err
	}
	if name == EnvironmentAndroid && imageArchitecture() != "x86_64" {
		return errors.New("android-test requires x86_64")
	}
	rt, err = rt.pinnedRecipeRuntime()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, ProvisioningTTL-provisioningSafetyMargin)
	defer cancel()
	return imageBuildLock(ctx, rt.imageRegistryPath()+".lock", func() error {
		registry, err := rt.imageRegistry()
		if err != nil {
			return err
		}
		if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
			return err
		}
		if registry.Build != nil {
			if err := rt.cleanupImageBuild(ctx, store, &registry); err != nil {
				return err
			}
		}
		recipe, err := rt.recipeDigest(name)
		if err != nil {
			return err
		}
		previous := append([]BaseImage(nil), registry.Bases...)
		built, err := rt.buildBase(ctx, store, LeaseGrant{Environment: &spec}, &registry, recipe)
		if err != nil {
			registry.Bases = previous
			registry.LastError = "base_build_failed"
			var capacity *CapacityError
			if errors.As(err, &capacity) && (capacity.Resource == "memory" || capacity.Resource == "disk") {
				registry.LastError = "capacity_" + capacity.Resource
			}
			if capacity == nil {
				registry.RetryAttempt++
				registry.RetryAfter = rt.Now().Add(recoveryDelay(registry.RetryAttempt))
			}
			return errors.Join(err, writeJSONAtomic(rt.imageRegistryPath(), registry))
		}
		registry.Bases = append(registry.Bases, built)
		registry.LastError = ""
		registry.RetryAttempt = 0
		registry.RetryAfter = time.Time{}
		return writeJSONAtomic(rt.imageRegistryPath(), registry)
	})
}
