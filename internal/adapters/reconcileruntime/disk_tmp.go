package reconcileruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
)

type diskTmpRebootError struct{ yard string }

func (failure diskTmpRebootError) Error() string {
	message, retry := failure.ActivationDiagnostic()
	return message + "; " + retry
}

func (failure diskTmpRebootError) ActivationDiagnostic() (string, string) {
	return fmt.Sprintf("yard %s: disk-backed /tmp policy is installed; the active /tmp is memory-backed; reboot required", failure.yard),
		fmt.Sprintf("close active yard sessions, then run yard -Y %s stop and yard -Y %s start", failure.yard, failure.yard)
}

func (runtime Runtime) diskTmpReadiness(ctx context.Context, diagnostic bool) (bool, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return false, err
	}
	if !state.InstanceFound {
		return false, nil
	}
	instance := state.Instance
	if strings.EqualFold(instance.Status, "stopped") {
		policy, _ := instance.EffectiveConfig("user.subyard.disk_tmp")
		return instanceIntentionallyStopped(instance) && policy == "v1", nil
	}
	if !strings.EqualFold(instance.Status, "running") {
		return false, nil
	}
	if runtime.Executor == nil {
		return false, errors.New("Incus executor is required")
	}
	script, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "scripts", "guest-disk-tmp.sh"))
	if err != nil {
		return false, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName,
		ports.InstanceExecRequest{Command: []string{"sh", "-s", "--", "check"}, Stdin: script})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	if err != nil && result.ExitCode == 0 {
		return false, err
	}
	if result.ExitCode == 10 && diagnostic {
		return false, diskTmpRebootError{yard: runtime.Yard.YardName}
	}
	return result.ExitCode == 0, nil
}
