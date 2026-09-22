package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const (
	orcaRuntimeOutputLimit  = 64 << 10
	orcaRuntimeObserveLimit = 30 * time.Second
	orcaRuntimeApplyLimit   = 2 * time.Minute
)

var orcaRuntimeDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ObserveOrcaRuntime inspects only an existing, running yard. The profile handler owns
// the installed-file contract; this adapter only validates and transports its result.
func (runtime Runtime) ObserveOrcaRuntime(ctx context.Context) (ports.OrcaRuntimeObservation, error) {
	if runtime.Incus == nil {
		return ports.OrcaRuntimeObservation{}, errors.New("Orca runtime observation requires Incus")
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		if errors.Is(err, ports.ErrInstanceNotFound) || errors.Is(err, os.ErrNotExist) {
			return ports.OrcaRuntimeObservation{State: "absent"}, nil
		}
		return ports.OrcaRuntimeObservation{}, fmt.Errorf("inspect yard for Orca runtime: %w", err)
	}
	switch {
	case strings.EqualFold(instance.Status, "stopped"):
		return ports.OrcaRuntimeObservation{State: "deferred"}, nil
	case !strings.EqualFold(instance.Status, "running"):
		return ports.OrcaRuntimeObservation{}, fmt.Errorf(
			"cannot inspect Orca runtime while yard state is %q", instance.Status,
		)
	}
	stdout, _, err := runtime.runOrcaRuntimeHandler(ctx, "observe")
	if err != nil {
		return ports.OrcaRuntimeObservation{}, err
	}
	return decodeOrcaRuntimeObservation(stdout)
}

func (runtime Runtime) applyOrcaRuntime(ctx context.Context) error {
	observation, err := runtime.ObserveOrcaRuntime(ctx)
	if err != nil {
		return err
	}
	switch observation.State {
	case "absent", "current":
		return nil
	case "deferred":
		runtime.reportOrcaDeferred()
		return nil
	case "stale":
	default:
		return fmt.Errorf("invalid Orca runtime state %q", observation.State)
	}
	operationID := runtime.environmentValue("SUBYARD_OPERATION_ID")
	if !domain.SafeID(operationID) {
		return errors.New("Orca runtime refresh requires a valid operation ID")
	}
	stdout, stderr, err := runtime.runOrcaRuntimeHandler(
		ctx, "apply", operationID, observation.Actual, observation.Desired,
	)
	if err != nil {
		if runtime.Stderr != nil && len(stderr) != 0 {
			_, _ = runtime.Stderr.Write(stderr)
		}
		return err
	}
	applied, err := decodeOrcaRuntimeObservation(stdout)
	if err != nil {
		return fmt.Errorf("validate Orca runtime apply result: %w", err)
	}
	if applied.State != "current" || applied.Actual != observation.Desired ||
		applied.Desired != observation.Desired {
		return errors.New("Orca runtime refresh did not produce the expected contract")
	}
	verified, err := runtime.ObserveOrcaRuntime(ctx)
	if err != nil {
		return fmt.Errorf("verify Orca runtime refresh: %w", err)
	}
	if verified.State != "current" || verified.Actual != observation.Desired ||
		verified.Desired != observation.Desired {
		return errors.New("Orca runtime refresh did not retain the expected contract")
	}
	return nil
}

func (runtime Runtime) reportOrcaDeferred() {
	if runtime.Stderr != nil {
		fmt.Fprintln(runtime.Stderr, "  [ .. ] Orca runtime refresh deferred while the yard is stopped; installed Orca will be inspected after the yard starts")
	}
}

func (runtime Runtime) runOrcaRuntimeHandler(
	ctx context.Context,
	action string,
	arguments ...string,
) ([]byte, []byte, error) {
	path := filepath.Join(runtime.RepositoryRoot, "config", "profiles", "orca", "resources", "orca", "handler.sh")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect Orca runtime handler: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return nil, nil, errors.New("Orca runtime handler must be a non-symlink executable file")
	}
	commandArguments := append([]string{"_runtime-contract", action}, arguments...)
	timeout := orcaRuntimeObserveLimit
	if action == "apply" {
		timeout = orcaRuntimeApplyLimit
	}
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(callContext, path, commandArguments...)
	command.Dir = runtime.RepositoryRoot
	command.Env = runtime.Environment
	command.Stdin = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	stdout := &orcaBoundedBuffer{limit: orcaRuntimeOutputLimit}
	stderr := &orcaBoundedBuffer{limit: orcaRuntimeOutputLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	err = command.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, stderr.Bytes(), errors.New("Orca runtime handler output exceeded limit")
	}
	if callContext.Err() != nil {
		return nil, stderr.Bytes(), fmt.Errorf("Orca runtime handler cancelled: %w", callContext.Err())
	}
	if err != nil {
		return nil, stderr.Bytes(), fmt.Errorf("Orca runtime handler %s failed: %w", action, err)
	}
	return bytes.Clone(stdout.Bytes()), bytes.Clone(stderr.Bytes()), nil
}

func decodeOrcaRuntimeObservation(payload []byte) (ports.OrcaRuntimeObservation, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var observation ports.OrcaRuntimeObservation
	if err := decoder.Decode(&observation); err != nil {
		return ports.OrcaRuntimeObservation{}, fmt.Errorf("decode Orca runtime observation: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ports.OrcaRuntimeObservation{}, errors.New("Orca runtime observation contains trailing data")
	}
	switch observation.State {
	case "absent", "deferred":
		if observation.Actual != "" || observation.Desired != "" {
			return ports.OrcaRuntimeObservation{}, fmt.Errorf("Orca runtime state %q contains digests", observation.State)
		}
	case "current":
		if !orcaRuntimeDigest.MatchString(observation.Actual) || observation.Actual != observation.Desired {
			return ports.OrcaRuntimeObservation{}, errors.New("current Orca runtime observation has invalid digests")
		}
	case "stale":
		if (observation.Actual != "" && !orcaRuntimeDigest.MatchString(observation.Actual)) ||
			!orcaRuntimeDigest.MatchString(observation.Desired) {
			return ports.OrcaRuntimeObservation{}, errors.New("stale Orca runtime observation has invalid digests")
		}
	default:
		return ports.OrcaRuntimeObservation{}, fmt.Errorf("unknown Orca runtime state %q", observation.State)
	}
	return observation, nil
}

type orcaBoundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *orcaBoundedBuffer) Write(value []byte) (int, error) {
	if buffer.exceeded {
		return len(value), nil
	}
	remaining := buffer.limit - buffer.Len()
	if len(value) > remaining {
		buffer.exceeded = true
		if remaining > 0 {
			_, _ = buffer.Buffer.Write(value[:remaining])
		}
		return len(value), nil
	}
	return buffer.Buffer.Write(value)
}
