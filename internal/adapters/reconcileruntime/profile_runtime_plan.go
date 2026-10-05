package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

// ProfileRuntimePlan retains owner-native observations, including explicit slots
// for declared activations that cannot be observed until approved prerequisites run.
type ProfileRuntimePlan struct {
	definitions []profile.Definition
	approved    map[string]ports.RuntimeObservation
	current     map[string]ports.RuntimeObservation
	conditional map[string]bool
	inputs      string
	resolved    map[string]ports.RuntimeObservation
	unavailable bool
}

func (runtime Runtime) PrepareProfileRuntimes(ctx context.Context, unavailable, reconstruct bool, dependent ...bool) (*ProfileRuntimePlan, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	plan := &ProfileRuntimePlan{definitions: definitions, approved: map[string]ports.RuntimeObservation{}, current: map[string]ports.RuntimeObservation{}, conditional: map[string]bool{}, resolved: map[string]ports.RuntimeObservation{}}
	plan.inputs, err = runtime.profileRuntimeInputs(definitions)
	if err != nil {
		return nil, err
	}
	observations, err := runtime.observeProfileRuntimes(ctx, definitions)
	if err != nil && (!unavailable || !runtime.nativeObservationUnavailable(err)) {
		return nil, err
	}
	if err != nil {
		plan.unavailable = true
		observations = map[string]ports.RuntimeObservation{}
		for _, definition := range definitions {
			observations[definition.Runtime.ActivationID] = ports.RuntimeObservation{State: ports.RuntimeStateDeferred}
		}
	}
	plan.approved = observations
	guestUnavailable := false
	for _, observation := range observations {
		if observation.State == ports.RuntimeStateAbsent {
			instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
			if err != nil {
				if errors.Is(err, ports.ErrInstanceNotFound) {
					guestUnavailable = true
				} else {
					return nil, err
				}
			} else {
				guestUnavailable = !strings.EqualFold(instance.Status, "running")
			}
			break
		}
	}
	for id, observation := range observations {
		plan.conditional[id] = reconstruct || observation.State == ports.RuntimeStateDeferred || observation.State == ports.RuntimeStateAbsent && (guestUnavailable || len(dependent) != 0 && dependent[0])
	}

	for id, observation := range plan.approved {
		plan.current[id] = observation
	}
	return plan, nil
}

func (runtime Runtime) profileRuntimeInputs(definitions []profile.Definition) (string, error) {
	environment := slices.Clone(runtime.Environment)
	// Authorization bookkeeping changes after consent; it is not a desired
	// runtime, handler or guest input. The sudo adapter validates it separately.
	environment = slices.DeleteFunc(environment, func(value string) bool {
		return strings.HasPrefix(value, "SUBYARD_OPERATION_ID=") || strings.HasPrefix(value, "SUBYARD_SUDO_PREAUTHORIZED=")
	})
	slices.Sort(environment)
	handlers := map[string][]byte{}
	for _, definition := range definitions {
		path, err := definition.ExecutablePath(definition.Runtime.Handler)
		if err != nil {
			return "", err
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		handlers[definition.Runtime.ActivationID] = payload
	}
	sources := map[string]string{}
	count := 0
	for _, root := range []string{filepath.Join(runtime.RepositoryRoot, "config"), filepath.Join(runtime.RepositoryRoot, "scripts")} {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "tests" || entry.Name() == "__pycache__" || entry.Name() == ".git" {
					return filepath.SkipDir
				}
			}
			if strings.HasSuffix(entry.Name(), ".pyc") {
				return nil
			}
			count++
			if count > 4096 {
				return fmt.Errorf("native runtime source scope exceeds observation limit")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				sources[path] = "symlink:" + target
				return nil
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("native source metadata is unavailable")
			}
			if info.IsDir() {
				sources[path] = fmt.Sprintf("directory:%o:%d:%d:%d:%d", info.Mode(), stat.Dev, stat.Ino, stat.Uid, stat.Gid)
				return nil
			}
			sources[path] = fmt.Sprintf("%o:%d:%d:%d:%d:%d:%v:%v", info.Mode(), info.Size(), stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mtim, stat.Ctim)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	payload, err := json.Marshal(struct {
		Definitions []profile.Definition
		Environment []string
		Handlers    map[string][]byte
		Sources     map[string]string
	}{definitions, environment, handlers, sources})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func (plan *ProfileRuntimePlan) Binding() string {
	if plan == nil {
		return ""
	}
	payload, _ := json.Marshal(struct {
		Inputs      string
		Approved    map[string]ports.RuntimeObservation
		Conditional map[string]bool
		Unavailable bool
	}{plan.inputs, plan.approved, plan.conditional, plan.unavailable})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func (runtime Runtime) CheckProfileRuntimePlan(ctx context.Context) error {
	plan := runtime.RuntimePlan
	if plan == nil {
		return nil
	}
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return err
	}
	inputs, err := runtime.profileRuntimeInputs(definitions)
	if err != nil {
		return err
	}
	if inputs != plan.inputs {
		return fmt.Errorf("%w: profile runtime inputs changed", domain.ErrPlanStale)
	}
	observations, err := runtime.observeProfileRuntimes(ctx, definitions)
	if err != nil {
		if plan.unavailable && runtime.nativeObservationUnavailable(err) {
			return nil
		}
		return err
	}

	for _, definition := range plan.definitions {
		id := definition.Runtime.ActivationID
		approved, current := plan.approved[id], observations[id]
		if resolved, ok := plan.resolved[id]; ok {
			approved = resolved
		}
		_, resolved := plan.resolved[id]
		if (!plan.conditional[id] || resolved) && current != approved && !(approved.State == ports.RuntimeStateStale && current.State == ports.RuntimeStateCurrent && current.Actual == approved.Desired && current.Desired == approved.Desired) {
			return fmt.Errorf("%w: profile runtime %s assessment changed", domain.ErrPlanStale, id)
		}
	}
	plan.current = observations
	return nil
}

func (plan *ProfileRuntimePlan) OperationSteps(target string) []domain.OperationStep {
	if plan == nil {
		return nil
	}
	steps := make([]domain.OperationStep, 0, len(plan.definitions))
	for _, definition := range plan.definitions {
		id := definition.Runtime.ActivationID
		approved, current := plan.approved[id], plan.current[id]
		decision := domain.StepSkip
		desired := approved.Desired
		if plan.conditional[id] {
			desired = "native contract from captured runtime declarations and inputs"
			decision = domain.StepConditional
		} else if current.State == ports.RuntimeStateStale {
			decision = domain.StepApply
		}
		if desired == "" {
			desired = "absent"
		}
		observed := string(current.State)
		if current.Actual != "" {
			observed += " " + current.Actual
		}
		steps = append(steps, domain.OperationStep{ID: "init.runtime." + id, Target: target + ":" + id, Observed: observed, Desired: desired, Decision: decision,
			Preconditions: []string{"captured native runtime declarations, handlers and configuration are unchanged", "an observed converged runtime cannot acquire new work"},
			Verify:        "native observe returns the approved desired contract after apply", Consequence: "refresh installed profile runtime " + id})
	}
	return steps
}

func (runtime Runtime) resolveProfileRuntimePlan() {
	if runtime.RuntimePlan == nil {
		return
	}
	plan := runtime.RuntimePlan
	for id, observation := range plan.current {
		if plan.conditional[id] && (observation.State == ports.RuntimeStateCurrent || observation.State == ports.RuntimeStateStale) {
			if _, exists := plan.resolved[id]; !exists {
				plan.resolved[id] = observation
			}
		}
	}
}

func (runtime Runtime) nativeObservationUnavailable(err error) bool {
	if probe, ok := runtime.Incus.(interface{ LocalInstallationAbsent() bool }); ok && probe.LocalInstallationAbsent() {
		return true
	}
	var networkError *net.OpError
	return errors.Is(err, os.ErrPermission) && errors.As(err, &networkError) && networkError.Op == "dial" && networkError.Net == "unix"
}
