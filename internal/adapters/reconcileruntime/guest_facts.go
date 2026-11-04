package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
)

const guestFactsSource = "config/skills/core/subyard-environment/facts.py"

// The running release supplies facts.py; historical scripts never emit this code.
const guestFactsProfilesPending = 11

// The CLI supplies YARD_VERSION from the executing engine's compiled version.
// This snapshot deliberately makes no claim about optional provisioning.
func (runtime Runtime) guestRuntimeFacts() []byte {
	profiles := strings.Fields(runtime.environmentValue("ENVIRONMENT_PROFILES"))
	if profiles == nil {
		profiles = []string{}
	}
	payload, _ := json.Marshal(map[string]any{
		"version":           runtime.environmentValue("YARD_VERSION"),
		"yard":              map[string]string{"name": runtime.Yard.YardName, "project": runtime.Yard.IncusProject, "instance": runtime.Yard.YardInstanceName},
		"selected_profiles": profiles,
	})
	return payload
}

func (runtime Runtime) guestFactsAvailable() bool {
	_, err := os.Lstat(filepath.Join(runtime.RepositoryRoot, guestFactsSource))
	return !os.IsNotExist(err)
}

func (runtime Runtime) guestFactsConverged(ctx context.Context) (bool, error) {
	exit, err := runtime.guestFactsCheck(ctx)
	if err != nil {
		return false, err
	}
	return exit == 0 || exit == guestFactsProfilesPending, nil
}

// Preserve the exact check result so ordinary configuration application does
// not mistake release readiness for an already applied selection snapshot.
func (runtime Runtime) guestFactsCheck(ctx context.Context) (int, error) {
	program, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, guestFactsSource))
	if err != nil {
		return 0, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", string(program), "check"}, Stdin: runtime.guestRuntimeFacts(),
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, err
	}
	// Native Incus returns the completed guest exit code together with an error.
	// Transport failures without a completed guest result must remain failures.
	if err != nil {
		var completed *ports.GuestExitError
		if !errors.As(err, &completed) || completed.Code != result.ExitCode {
			return 0, err
		}
	}
	switch result.ExitCode {
	case 0, 1, 10, guestFactsProfilesPending:
		return result.ExitCode, nil
	default:
		if err != nil {
			return 0, err
		}
		return 0, errors.New("guest runtime facts check failed")
	}
}
