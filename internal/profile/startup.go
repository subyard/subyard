package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

// StartupRemainingEnvironment carries this start's remaining readiness budget.
// Core owns the deadline; guest clocks need not share the owner's epoch.
const StartupRemainingEnvironment = "SUBYARD_STARTUP_REMAINING_MS"

// StartupReadiness separates service startup from installation convergence.
// Stable drift is repaired by migrate/init and must not prevent a physical start.
type StartupReadiness struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

var startupDiagnostic = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ,;:._()'/-]{0,239}$`)

func decodeStartupReadiness(body []byte) (StartupReadiness, error) {
	var state StartupReadiness
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if len(body) > 1024 || decoder.Decode(&state) != nil || decoder.Decode(new(any)) != io.EOF {
		return state, errors.New("invalid startup readiness response")
	}
	if state.State != "ready" && state.State != "drift" && state.State != "starting" ||
		state.Reason != "" && !startupDiagnostic.MatchString(state.Reason) ||
		state.Hint != "" && !startupDiagnostic.MatchString(state.Hint) ||
		state.State == "starting" && (state.Reason == "" || state.Hint == "") {
		return state, errors.New("invalid startup readiness response")
	}
	return state, nil
}

// StartupRefusal carries only validated public diagnostics. The operation
// boundary renders it; observers never emit warnings or raw native output.
type StartupRefusal struct {
	Profile, Reason, Hint string
	cause                 error
}

func (failure *StartupRefusal) Error() string {
	return fmt.Sprintf("profile %s startup: %s; hint: %s", failure.Profile, failure.Reason, failure.Hint)
}
func (failure *StartupRefusal) Unwrap() error { return failure.cause }

type startupOutput []byte

func (output *startupOutput) Write(data []byte) (int, error) {
	*output = append(*output, data[:min(len(data), 1025-len(*output))]...)
	return len(data), nil
}

func checkStartupReadiness(ctx context.Context, root string, definition Definition, environment map[string]string, deadline time.Time) (StartupReadiness, error) {
	command := exec.CommandContext(ctx, "bash", filepath.Join(definition.Root, definition.StartupReadiness), "--startup-check")
	command.Dir = root
	for name, value := range environment {
		if name != "SUBYARD_PROFILE_SELECTED" && name != StartupRemainingEnvironment {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	if _, anchored := environment[StartupRemainingEnvironment]; anchored {
		command.Env = append(command.Env, StartupRemainingEnvironment+"="+strconv.FormatInt(max(0, time.Until(deadline).Milliseconds()), 10))
	}
	command.Env = append(command.Env, "SUBYARD_PROFILE_SELECTED=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	var output startupOutput
	command.Stdout, command.Stderr = &output, io.Discard
	if err := command.Run(); err != nil {
		return StartupReadiness{}, err
	}
	return decodeStartupReadiness(output)
}

func waitStartupReadiness(ctx context.Context, root string, definitions []Definition, environment map[string]string, interval time.Duration) error {
	limit := ReadinessTimeout
	if value, anchored := environment[StartupRemainingEnvironment]; anchored {
		remaining, err := strconv.ParseInt(value, 10, 64)
		if err != nil || remaining < 0 || remaining > ReadinessTimeout.Milliseconds() {
			return errors.New("invalid startup remaining budget")
		}
		limit = time.Duration(remaining) * time.Millisecond
	}
	bounded, cancel := context.WithTimeout(ctx, limit)
	deadline, _ := bounded.Deadline()
	defer cancel()
	yard := environment["SUBYARD_YARD"]
	if yard == "" {
		yard = "default"
	}
	failures := make([]error, len(definitions))
	var workers sync.WaitGroup
	for index, definition := range definitions {
		selected := definition.Selected(yard, environment)
		if installed, explicit := environment["ENVIRONMENT_PROFILES"]; explicit {
			// An applied selection remains observable after a declaration is disabled.
			selected = slices.Contains(strings.Fields(installed), definition.Name)
		}
		if definition.StartupReadiness == "" || !selected {
			continue
		}
		workers.Go(func() {
			last := StartupReadiness{Reason: "readiness check unavailable", Hint: "run yard migrate to repair; inspect the profile service"}
			for {
				// A polling deadline must not kill a nearly completed observation
				// and misclassify ready service state as a failed probe.
				observation, cancelObservation := context.WithTimeout(ctx, ReadinessTimeout)
				state, err := checkStartupReadiness(observation, root, definition, environment, deadline)
				cancelObservation()
				if err == nil {
					if state.State != "starting" {
						return
					}
					last = state
				}
				if err != nil || bounded.Err() != nil {
					failures[index] = &StartupRefusal{definition.Name, last.Reason, last.Hint, errors.Join(err, bounded.Err())}
					return
				}
				timer := time.NewTimer(interval)
				select {
				case <-bounded.Done():
					timer.Stop()
					failures[index] = &StartupRefusal{definition.Name, last.Reason, last.Hint, bounded.Err()}
					return
				case <-timer.C:
				}
			}
		})
	}
	workers.Wait()
	return domain.CollectRefusals(failures...)
}
