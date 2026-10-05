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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

const (
	profileRuntimeOutputLimit  = 64 << 10
	profileRuntimeObserveLimit = 30 * time.Second
	profileRuntimeApplyLimit   = 2 * time.Minute
)

var profileRuntimeDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (runtime Runtime) ObserveProfileRuntimes(ctx context.Context) (map[string]ports.RuntimeObservation, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	return runtime.observeProfileRuntimes(ctx, definitions)
}

func (runtime Runtime) observeProfileRuntimes(
	ctx context.Context,
	definitions []profile.Definition,
) (map[string]ports.RuntimeObservation, error) {
	observations := make(map[string]ports.RuntimeObservation, len(definitions))
	if len(definitions) == 0 {
		return observations, nil
	}
	if runtime.Incus == nil {
		return nil, errors.New("profile runtime observation requires Incus")
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		if errors.Is(err, ports.ErrInstanceNotFound) {
			for _, definition := range definitions {
				observations[definition.Runtime.ActivationID] = ports.RuntimeObservation{State: ports.RuntimeStateAbsent}
			}
			return observations, nil
		}
		return nil, fmt.Errorf("inspect yard for profile runtimes: %w", err)
	}
	if strings.EqualFold(instance.Status, "stopped") {
		for _, definition := range definitions {
			observations[definition.Runtime.ActivationID] = ports.RuntimeObservation{State: ports.RuntimeStateDeferred}
		}
		return observations, nil
	}
	if !strings.EqualFold(instance.Status, "running") {
		return nil, fmt.Errorf("cannot inspect profile runtimes while yard state is %q", instance.Status)
	}
	for _, definition := range definitions {
		stdout, _, err := runtime.runProfileRuntimeHandler(ctx, definition, "observe")
		if err != nil {
			return nil, fmt.Errorf("observe profile runtime %s: %w", definition.Runtime.ActivationID, err)
		}
		observation, err := decodeProfileRuntimeObservation(stdout)
		if err != nil {
			return nil, fmt.Errorf("decode profile runtime %s observation: %w", definition.Runtime.ActivationID, err)
		}
		observations[definition.Runtime.ActivationID] = observation
	}
	return observations, nil
}

func (runtime Runtime) ApplyProfileRuntime(
	ctx context.Context,
	activationID, operationID, actual, desired string,
) error {
	if !domain.SafeID(operationID) {
		return errors.New("profile runtime refresh requires a valid operation ID")
	}
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return err
	}
	var selected *profile.Definition
	for index := range definitions {
		if definitions[index].Runtime.ActivationID == activationID {
			selected = &definitions[index]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown profile runtime activation ID %q", activationID)
	}
	return runtime.applyProfileRuntime(ctx, *selected, operationID, actual, desired)
}

func (runtime Runtime) applyProfileRuntimes(ctx context.Context) error {
	if err := runtime.CheckProfileRuntimePlan(ctx); err != nil {
		return err
	}
	runtime.resolveProfileRuntimePlan()
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return err
	}
	observations, err := runtime.observeProfileRuntimes(ctx, definitions)
	if err != nil {
		return err
	}
	operationID := runtime.environmentValue("SUBYARD_OPERATION_ID")
	for _, definition := range definitions {
		observation := observations[definition.Runtime.ActivationID]
		switch observation.State {
		case ports.RuntimeStateAbsent, ports.RuntimeStateCurrent:
			continue
		case ports.RuntimeStateDeferred:
			runtime.reportProfileRuntimeDeferred(definition.Runtime.ActivationID)
			continue
		case ports.RuntimeStateStale:
		default:
			return fmt.Errorf("invalid profile runtime state %q for %s", observation.State, definition.Runtime.ActivationID)
		}
		if err := runtime.applyProfileRuntime(ctx, definition, operationID, observation.Actual, observation.Desired); err != nil {
			return fmt.Errorf("apply profile runtime %s: %w", definition.Runtime.ActivationID, err)
		}
	}
	return nil
}

func (runtime Runtime) applyProfileRuntime(
	ctx context.Context,
	definition profile.Definition,
	operationID, actual, desired string,
) error {
	if !domain.SafeID(operationID) {
		return errors.New("profile runtime refresh requires a valid operation ID")
	}
	observations, err := runtime.observeProfileRuntimes(ctx, []profile.Definition{definition})
	if err != nil {
		return err
	}
	observation := observations[definition.Runtime.ActivationID]
	if observation.State == ports.RuntimeStateCurrent && observation.Actual == desired && observation.Desired == desired {
		return nil
	}
	if observation.State != ports.RuntimeStateStale || observation.Actual != actual || observation.Desired != desired {
		return fmt.Errorf("%w: profile runtime assessment changed before apply", domain.ErrPlanStale)
	}
	stdout, stderr, err := runtime.runProfileRuntimeHandler(ctx, definition, "apply", operationID, actual, desired)
	if err != nil {
		if runtime.Stderr != nil && len(stderr) != 0 {
			_, _ = runtime.Stderr.Write(stderr)
		}
		return err
	}
	applied, err := decodeProfileRuntimeObservation(stdout)
	if err != nil {
		return fmt.Errorf("validate profile runtime apply result: %w", err)
	}
	if applied.State != ports.RuntimeStateCurrent || applied.Actual != desired || applied.Desired != desired {
		return errors.New("profile runtime refresh did not produce the expected contract")
	}
	verified, err := runtime.observeProfileRuntimes(ctx, []profile.Definition{definition})
	if err != nil {
		return fmt.Errorf("verify profile runtime refresh: %w", err)
	}
	result := verified[definition.Runtime.ActivationID]
	if result.State != ports.RuntimeStateCurrent || result.Actual != desired || result.Desired != desired {
		return errors.New("profile runtime refresh did not retain the expected contract")
	}
	return nil
}

func (runtime Runtime) profileRuntimeDefinitions() ([]profile.Definition, error) {
	definitions, err := runtime.profileDefinitions()
	if err != nil {
		return nil, err
	}
	result := make([]profile.Definition, 0, len(definitions))
	for _, definition := range definitions {
		if definition.Runtime != nil {
			result = append(result, definition)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Runtime.ActivationID < result[j].Runtime.ActivationID
	})
	return result, nil
}

func (runtime Runtime) profileDefinitions() ([]profile.Definition, error) {
	definitions := runtime.Profiles
	if definitions == nil {
		var err error
		definitions, err = profile.Load(runtime.RepositoryRoot)
		if err != nil {
			return nil, fmt.Errorf("load profile runtime declarations: %w", err)
		}
	}
	return append([]profile.Definition(nil), definitions...), nil
}

func (runtime Runtime) runProfileRuntimeHandler(
	ctx context.Context,
	definition profile.Definition,
	action string,
	arguments ...string,
) ([]byte, []byte, error) {
	cleanPath, err := definition.ExecutablePath(definition.Runtime.Handler)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve profile runtime handler: %w", err)
	}
	root := strings.TrimSuffix(cleanPath, string(filepath.Separator)+definition.Runtime.Handler)
	timeout := profileRuntimeObserveLimit
	if action == "apply" {
		timeout = profileRuntimeApplyLimit
	}
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(callContext, cleanPath, append([]string{action}, arguments...)...)
	command.Dir = root
	command.Env = runtime.Environment
	if strings.HasPrefix(runtime.RepositoryRoot, "/proc/self/fd/") {
		// The hook does not inherit the engine's repository descriptor.
		parentRoot := strings.Replace(runtime.RepositoryRoot, "/proc/self/", fmt.Sprintf("/proc/%d/", os.Getpid()), 1)
		command.Env = append(command.Environ(), "SUBYARD_REPOSITORY_ROOT="+parentRoot)
	}
	command.Stdin = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	stdout := &profileRuntimeBoundedBuffer{limit: profileRuntimeOutputLimit}
	stderr := &profileRuntimeBoundedBuffer{limit: profileRuntimeOutputLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	err = command.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, stderr.Bytes(), errors.New("profile runtime handler output exceeded limit")
	}
	if callContext.Err() != nil {
		return nil, stderr.Bytes(), fmt.Errorf("profile runtime handler cancelled: %w", callContext.Err())
	}
	if err != nil {
		return nil, stderr.Bytes(), fmt.Errorf("profile runtime handler %s failed: %w", action, err)
	}
	return bytes.Clone(stdout.Bytes()), bytes.Clone(stderr.Bytes()), nil
}

func (runtime Runtime) readProfileHook(definition profile.Definition, handler string) ([]byte, error) {
	return definition.ReadExecutable(handler)
}

func decodeProfileRuntimeObservation(payload []byte) (ports.RuntimeObservation, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var observation ports.RuntimeObservation
	if err := decoder.Decode(&observation); err != nil {
		return ports.RuntimeObservation{}, fmt.Errorf("decode runtime observation: %w", err)
	}
	if observation.HookBinding != "" && !profileRuntimeDigest.MatchString(observation.HookBinding) {
		return ports.RuntimeObservation{}, errors.New("runtime observation has an invalid hook binding")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ports.RuntimeObservation{}, errors.New("runtime observation contains trailing data")
	}
	switch observation.State {
	case ports.RuntimeStateAbsent, ports.RuntimeStateDeferred:
		if observation.Actual != "" || observation.Desired != "" {
			return ports.RuntimeObservation{}, fmt.Errorf("runtime state %q contains digests", observation.State)
		}
	case ports.RuntimeStateCurrent:
		if !profileRuntimeDigest.MatchString(observation.Actual) || observation.Actual != observation.Desired {
			return ports.RuntimeObservation{}, errors.New("current runtime observation has invalid digests")
		}
	case ports.RuntimeStateStale:
		if (observation.Actual != "" && !profileRuntimeDigest.MatchString(observation.Actual)) ||
			!profileRuntimeDigest.MatchString(observation.Desired) {
			return ports.RuntimeObservation{}, errors.New("stale runtime observation has invalid digests")
		}
	default:
		return ports.RuntimeObservation{}, fmt.Errorf("unknown runtime state %q", observation.State)
	}
	return observation, nil
}

func (runtime Runtime) reportProfileRuntimeDeferred(activationID string) {
	if runtime.Stderr != nil {
		fmt.Fprintf(runtime.Stderr, "  [ .. ] Profile runtime %s refresh deferred while the yard is stopped; it will be inspected after the yard starts\n", activationID)
	}
}

type profileRuntimeBoundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

// io.Copy must use Write rather than the embedded buffer's unbounded ReadFrom.
func (buffer *profileRuntimeBoundedBuffer) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{buffer}, reader)
}

func (buffer *profileRuntimeBoundedBuffer) Write(value []byte) (int, error) {
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
