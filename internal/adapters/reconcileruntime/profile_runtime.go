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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/uninstall"
)

const (
	profileRuntimeOutputLimit  = 64 << 10
	profileRuntimeObserveLimit = profile.ReadinessTimeout
	profileRuntimeApplyLimit   = 2 * time.Minute
)

var profileRuntimeDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

var profileFailureLocation = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]{0,63}\.py):([1-9][0-9]{0,5})$`)

// Only a closed error class and a source-owned basename/line cross readiness.
// Arbitrary stderr, exception messages, paths and configuration remain private.
func profileFailureDetail(definition profile.Definition, stderr []byte) string {
	const prefix = "subyard-profile-observation-diagnostic-v1 "
	var detail string
	count := 0
	for _, line := range bytes.Split(stderr, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte(strings.TrimSpace(prefix))) {
			continue
		}
		count++
		if !bytes.HasPrefix(line, []byte(prefix)) {
			return ""
		}
		fields := strings.Split(strings.TrimPrefix(string(line), prefix), " ")
		if len(fields) != 2 || !slices.Contains([]string{"OSError", "PermissionError", "FileNotFoundError", "FileExistsError", "NotADirectoryError", "IsADirectoryError", "BlockingIOError", "TimeoutError", "ValueError", "KeyError", "TimeoutExpired", "CalledProcessError", "SubprocessError", "UnicodeDecodeError", "JSONDecodeError"}, fields[0]) {
			return ""
		}
		location := profileFailureLocation.FindStringSubmatch(fields[1])
		if location == nil {
			return ""
		}
		number, err := strconv.Atoi(location[2])
		if err != nil || strconv.Itoa(number) != location[2] {
			return ""
		}
		info, err := os.Lstat(filepath.Join(definition.Root, location[1]))
		if err != nil || !info.Mode().IsRegular() {
			return ""
		}
		detail = " (" + fields[0] + " at " + fields[1] + ")"
	}
	if count != 1 {
		return ""
	}
	return detail
}

func (runtime Runtime) profileRuntimeObservationFailure(definition profile.Definition, cause error, stderr []byte) error {
	failure := runtime.profileObservationFailure(definition.Runtime.ActivationID, "", cause).(*integrationLifecycleError)
	failure.message += profileFailureDetail(definition, stderr)
	failure.retry = "run yard config paths"
	if domain.SafeName(definition.Name) && domain.SafeName(runtime.Yard.YardName) {
		failure.retry = "run yard -Y " + runtime.Yard.YardName + " config paths"
	}
	// Only explicitly public owner refusals may add a path and executable step.
	// Exception text and ordinary stderr remain private.
	const prefix = "subyard-profile-observation-refusal-v1 "
	var records []string
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.HasPrefix(line, prefix) {
			records = append(records, strings.TrimPrefix(line, prefix))
		}
	}
	if len(records) == 1 && len(records[0]) <= 2048 {
		var record struct {
			Message string `json:"message"`
			Retry   string `json:"retry"`
		}
		decoder := json.NewDecoder(strings.NewReader(records[0]))
		decoder.DisallowUnknownFields()
		valid := func(value string) bool {
			if value == "" || len(value) > 480 {
				return false
			}
			for _, r := range value {
				if unicode.IsControl(r) {
					return false
				}
			}
			return true
		}
		var tail any
		if decoder.Decode(&record) == nil && decoder.Decode(&tail) == io.EOF && valid(record.Message) && valid(record.Retry) && strings.HasPrefix(record.Retry, "run ") {
			failure.message, failure.retry = record.Message, record.Retry
		}
	}
	return failure
}

// A runtime hook may state one short plan consequence of applying a stale contract.
const profileRuntimeConsequenceBytes = 200

var profileRuntimeConsequence = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ,;:._()'/-]*$`)

func (runtime Runtime) profileObservationFailure(owner, reason string, cause error) error {
	if reason == "" {
		reason = "unclassified"
		var exit *exec.ExitError
		switch {
		case errors.Is(cause, context.Canceled):
			reason = "cancelled"
		case errors.Is(cause, context.DeadlineExceeded):
			reason = "deadline"
		case errors.Is(cause, os.ErrPermission):
			reason = "permission-denied"
		case errors.Is(cause, os.ErrNotExist):
			reason = "command-not-found"
		case errors.As(cause, &exit):
			reason = fmt.Sprintf("native-exit-%d", exit.ExitCode())
		}
	}
	yard := runtime.Yard.YardName
	if len(owner) > 63 || !domain.SafeID(owner) {
		owner = "unknown"
	}
	if len(yard) > 63 || !domain.SafeName(yard) {
		yard = "unknown"
	}
	return &integrationLifecycleError{cause: cause, message: fmt.Sprintf("yard %s: profile %s observation failed (%s)", yard, owner, reason), retry: "run yard update --check"}
}

func (runtime Runtime) ObserveProfileRuntimes(ctx context.Context) (map[string]ports.RuntimeObservation, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	return runtime.observeProfileRuntimes(ctx, definitions)
}

// ObserveProfileRuntimeDiagnostics opts into the current owners' read-only
// diagnostic extension. Ordinary activation retains its historical wire shape.
func (runtime Runtime) ObserveProfileRuntimeDiagnostics(ctx context.Context) (map[string]ports.RuntimeObservation, error) {
	runtime.profileDiagnostics = true
	return runtime.ObserveProfileRuntimes(ctx)
}

// ObserveProfileRuntime narrows native observation to one declared owner. A
// retained-release admission check must not invoke unrelated profile hooks.
func (runtime Runtime) ObserveProfileRuntime(ctx context.Context, id string) (ports.RuntimeObservation, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	for _, definition := range definitions {
		if definition.Runtime.ActivationID == id {
			if probe, ok := runtime.Incus.(interface{ LocalInstallationAbsent() bool }); ok && probe.LocalInstallationAbsent() {
				return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
			}
			observations, err := runtime.observeProfileRuntimes(ctx, []profile.Definition{definition})
			return observations[id], err
		}
	}
	return ports.RuntimeObservation{}, errors.New("profile runtime owner is unavailable")
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
	outputs := make([][]byte, len(definitions))
	diagnostics := make([][]byte, len(definitions))
	failures := make([]error, len(definitions))
	slots := make(chan struct{}, 3)
	var workers sync.WaitGroup
	for index, definition := range definitions {
		slots <- struct{}{}
		workers.Go(func() {
			defer func() { <-slots }()
			outputs[index], diagnostics[index], failures[index] = runtime.runProfileRuntimeHandler(ctx, definition, "observe")
		})
	}
	workers.Wait()
	// Every read-only native process has finished before decoding in declaration
	// order. Errors never leave sibling observations running into later effects.
	var refusals []error
	for index, definition := range definitions {
		if failures[index] != nil {
			refusals = append(refusals, runtime.profileRuntimeObservationFailure(definition, failures[index], diagnostics[index]))
			continue
		}
		observation, err := decodeProfileRuntimeObservation(outputs[index])
		if err != nil {
			refusals = append(refusals, runtime.profileObservationFailure(definition.Runtime.ActivationID, "invalid-schema", err))
			continue
		}
		if observation.Diagnostics != nil && !runtime.profileDiagnostics && observation.State != ports.RuntimeStatePending {
			refusals = append(refusals, runtime.profileObservationFailure(definition.Runtime.ActivationID, "invalid-schema", errors.New("unexpected runtime diagnostics")))
			continue
		}
		observations[definition.Runtime.ActivationID] = observation
	}
	return observations, domain.CollectRefusals(refusals...)
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
	var pending []error
	for _, definition := range definitions {
		observation := observations[definition.Runtime.ActivationID]
		if observation.State == ports.RuntimeStatePending {
			pending = append(pending, PendingProfileRuntimeError(runtime.Yard.YardName, definition.Runtime.ActivationID, observation))
		}
	}
	if err := domain.CollectRefusals(pending...); err != nil {
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
	custody ...string,
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
	action := "apply"
	arguments := []string{operationID, actual, desired}
	if len(custody) != 0 {
		if !definition.Runtime.Custody || len(custody) != 2 {
			return domain.ErrPlanStale
		}
		action = "custody-apply"
		arguments = append(arguments, custody...)
	}
	stdout, stderr, err := runtime.runProfileRuntimeHandler(ctx, definition, action, arguments...)
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
	timeout := profileRuntimeObserveLimit
	if action == "apply" || action == "custody-apply" || action == "custody-fence" {
		timeout = profileRuntimeApplyLimit
	}
	return runtime.runShippedHandler(ctx, definition, definition.Runtime.Handler, runtime.Environment, timeout, action, arguments...)
}

func (runtime Runtime) runShippedHandler(ctx context.Context, definition profile.Definition, handler string, environment []string, timeout time.Duration, action string, arguments ...string) ([]byte, []byte, error) {
	environment = slices.DeleteFunc(slices.Clone(environment), func(value string) bool { return strings.HasPrefix(value, "SUBYARD_PROFILE_DIAGNOSTICS=") })
	if action == "observe" && runtime.profileDiagnostics {
		environment = append(environment, "SUBYARD_PROFILE_DIAGNOSTICS=1")
	}
	cleanPath, err := definition.ExecutablePath(handler)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve shipped handler: %w", err)
	}
	root := strings.TrimSuffix(cleanPath, string(filepath.Separator)+handler)
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(callContext, cleanPath, append([]string{action}, arguments...)...)
	command.Dir = root
	command.Env = environment
	if strings.HasPrefix(runtime.RepositoryRoot, "/proc/self/fd/") {
		// The hook does not inherit the engine's repository descriptor.
		parentRoot := strings.Replace(runtime.RepositoryRoot, "/proc/self/", fmt.Sprintf("/proc/%d/", os.Getpid()), 1)
		command.Env = append(command.Environ(), "SUBYARD_REPOSITORY_ROOT="+parentRoot)
	}
	command.Stdin = nil
	if ports.NativeOperationObserverFrom(ctx) != nil && action != "observe" && action != "custody-observe" {
		home := runtime.Yard.Paths.DataHome
		if home == "" {
			home = runtime.environmentValue("SUBYARD_HOME")
		}
		owner, err := uninstall.BorrowOwnerLock(home)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return nil, nil, errors.New("native profile transport requires its original owner barrier")
		}
		defer owner.Close()
		command.ExtraFiles = append(command.ExtraFiles, owner)
		command.Env = append(command.Env, "SUBYARD_NATIVE_OWNER_FD3=1")
	}
	if action == "observe" && len(runtime.projectMetadataProjection) != 0 {
		command.Stdin = bytes.NewReader(runtime.projectMetadataProjection)
	}
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
	observationContext := ctx
	if action == "observe" || action == "custody-observe" {
		observationContext = ports.WithoutNativeOperationObserver(ctx)
	}
	finish, err := runtime.beginNativeCommand(observationContext, "shipped-handler", cleanPath, command.Env, append([]string{action}, arguments...))
	if err != nil {
		return nil, nil, err
	}
	err = command.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, stderr.Bytes(), finish(errors.New("profile runtime handler output exceeded limit"))
	}
	if callContext.Err() != nil {
		return nil, stderr.Bytes(), finish(fmt.Errorf("profile runtime handler cancelled: %w", callContext.Err()))
	}
	if err != nil {
		return nil, stderr.Bytes(), finish(fmt.Errorf("profile runtime handler %s failed: %w", action, err))
	}
	if err := finish(nil); err != nil {
		return nil, nil, err
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
	if observation.OwnershipAbsent && observation.State != ports.RuntimeStateCurrent {
		return ports.RuntimeObservation{}, errors.New("runtime ownership absence requires current state")
	}
	if observation.Consequence != "" && (observation.State != ports.RuntimeStateStale ||
		len(observation.Consequence) > profileRuntimeConsequenceBytes || !profileRuntimeConsequence.MatchString(observation.Consequence) ||
		strings.HasSuffix(observation.Consequence, " ")) {
		return ports.RuntimeObservation{}, errors.New("runtime observation has an invalid consequence")
	}
	if observation.Diagnostics != nil && (len(*observation.Diagnostics) > 128 || observation.State != ports.RuntimeStateStale && observation.State != ports.RuntimeStatePending) {
		return ports.RuntimeObservation{}, errors.New("runtime observation has invalid diagnostics")
	}
	seen := map[string]bool{}
	var diagnostics ports.RuntimeDiagnostics
	if observation.Diagnostics != nil {
		diagnostics = *observation.Diagnostics
	}
	for _, item := range diagnostics {
		validValue := func(value string) bool {
			return slices.Contains([]string{"unknown", "absent", "present", "active", "inactive", "enabled", "disabled", "not-found", "failed", "drift", "current", "changed", "matched", "mismatch", "ready", "not-ready", "missing"}, value)
		}
		if len(item.Parameter) > 96 || !domain.SafeID(item.Parameter) || seen[item.Parameter] || !validValue(item.Installed) || !validValue(item.Desired) {
			return ports.RuntimeObservation{}, errors.New("runtime observation has invalid diagnostics")
		}
		seen[item.Parameter] = true
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ports.RuntimeObservation{}, errors.New("runtime observation contains trailing data")
	}
	if observation.State == ports.RuntimeStatePending && (observation.Diagnostics == nil || len(*observation.Diagnostics) == 0) {
		return ports.RuntimeObservation{}, errors.New("pending runtime requires diagnostics")
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
	case ports.RuntimeStateStale, ports.RuntimeStatePending:
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

// ProfileServiceBinding binds scoped service approval to its declared runtime,
// including pending work. Profiles without a runtime retain their existing hook.
func (runtime Runtime) ProfileServiceBinding(ctx context.Context, name string) (string, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return "", err
	}
	for _, definition := range definitions {
		if definition.Name != name {
			continue
		}
		observations, err := runtime.observeProfileRuntimes(ctx, []profile.Definition{definition})
		if err != nil {
			return "", err
		}
		payload, err := json.Marshal(observations[definition.Runtime.ActivationID])
		return string(payload), err
	}
	return "", nil
}

// PendingProfileRuntimeError directs another owner's repair without claiming convergence.
func PendingProfileRuntimeError(yard, id string, observation ports.RuntimeObservation) error {
	message := "profile " + id + " is pending input repair"
	if observation.Diagnostics != nil {
		for index, detail := range *observation.Diagnostics {
			text := "; " + detail.Parameter + ": " + detail.Installed + " -> " + detail.Desired
			if len(message)+len(text) > 400 {
				message += fmt.Sprintf("; %d more inputs: run yard config status", len(*observation.Diagnostics)-index)
				break
			}
			message += text
		}
	}
	retry := "run yard config apply"
	if domain.SafeName(yard) {
		retry = "run yard -Y " + yard + " config apply"
	}
	return &integrationLifecycleError{message: message, retry: retry}
}
