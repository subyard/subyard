package reconcileruntime

import (
	"context"
	"errors"
)

// RunMigrationPhysicalLifecycle changes physical power without changing intent.
// The normal stop guard still fences listeners and checks active sessions.
func (runtime Runtime) RunMigrationPhysicalLifecycle(ctx context.Context, action string) error {
	if action == "start" {
		return runtime.withNetworkStart(ctx, func() error {
			return runtime.runScript(ctx, runtime.Stderr, "lifecycle-guard.sh", "start", "--reconcile")
		})
	}
	if action == "stop" {
		return runtime.runScript(ctx, runtime.Stderr, "lifecycle-guard.sh", "stop")
	}
	return errors.New("unsupported physical migration lifecycle action")
}

// RunApprovedMigrationStop stops a yard for a restart its caller's plan disclosed and
// the operator approved. The guard still fences listeners and pauses profile services,
// then reports live sessions instead of refusing them.
func (runtime Runtime) RunApprovedMigrationStop(ctx context.Context) error {
	return runtime.runScript(ctx, runtime.Stderr, "lifecycle-guard.sh", "stop", "--interrupt-sessions")
}
