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
		if errors.Is(err, ports.ErrInstanceNotFound) || errors.Is(err, os.ErrNotExist) {
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
		return errors.New("profile runtime assessment changed before apply")
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
	cleanPath, err := profileHookPath(definition.Root, definition.Runtime.Handler)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve profile runtime handler: %w", err)
	}
	root, err := filepath.Abs(definition.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve profile runtime root: %w", err)
	}
	timeout := profileRuntimeObserveLimit
	if action == "apply" {
		timeout = profileRuntimeApplyLimit
	}
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(callContext, cleanPath, append([]string{action}, arguments...)...)
	command.Dir = root
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

func profileHookPath(root, handler string) (string, error) {
	if root == "" || handler == "" || filepath.IsAbs(handler) || filepath.Clean(handler) != handler || handler == "." ||
		handler == ".." || strings.HasPrefix(handler, ".."+string(filepath.Separator)) {
		return "", errors.New("profile hook path is invalid")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve profile hook root: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(root, handler))
	if err != nil {
		return "", fmt.Errorf("resolve profile hook path: %w", err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("profile hook path escapes profile root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	rootInfo, rootErr := os.Lstat(root)
	if err != nil || rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || resolvedRoot != root {
		return "", errors.New("profile hook root must be a real directory")
	}
	parent := root
	components := strings.Split(relative, string(filepath.Separator))
	for _, component := range components[:len(components)-1] {
		parent = filepath.Join(parent, component)
		parentInfo, parentErr := os.Lstat(parent)
		if parentErr != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("profile hook path must not traverse symlinks")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect profile hook: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("profile hook must be a non-symlink executable file")
	}
	return path, nil
}

func decodeProfileRuntimeObservation(payload []byte) (ports.RuntimeObservation, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var observation ports.RuntimeObservation
	if err := decoder.Decode(&observation); err != nil {
		return ports.RuntimeObservation{}, fmt.Errorf("decode runtime observation: %w", err)
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
