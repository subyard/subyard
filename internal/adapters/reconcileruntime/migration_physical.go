package reconcileruntime

import (
	"context"
	"errors"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
)

func (runtime Runtime) RegisteredDiskTmpReady(ctx context.Context) (bool, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return false, err
	}
	if strings.EqualFold(state.Instance.Status, "stopped") {
		// Policy completion does not certify ordinary initialization or profiles.
		policy, _ := state.Instance.EffectiveConfig("user.subyard.disk_tmp")
		return application.ProjectHookStoppedIntentPreserved(state.Instance) && policy == "v1", nil
	}
	return runtime.diskTmpInstanceReadiness(ctx, state.Instance, false)
}

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
