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
	program, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, guestFactsSource))
	if err != nil {
		return false, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", string(program), "check"}, Stdin: runtime.guestRuntimeFacts(),
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	// Native Incus returns the completed guest exit code together with an error.
	// Transport failures without a completed guest result must remain failures.
	if err != nil && result.ExitCode == 0 {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 1, 10:
		return false, nil // Missing, invalid, incomplete or expected snapshot drift.
	default:
		if err != nil {
			return false, err
		}
		return false, errors.New("guest runtime facts check failed")
	}
}
