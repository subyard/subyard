package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

// A retained owner must still own installed profile runtimes. Historical core
// helper owners may retain an identical public payload/pin contract; they may
// not silently accept a different contract they cannot activate.
func retainedRuntimeApplicable(root string) (bool, error) {
	definitions, err := profile.Load(root)
	if err != nil {
		return false, err
	}
	for _, definition := range definitions {
		if definition.Runtime != nil || definition.InfrastructureService != nil {
			return true, nil
		}
	}
	_, err = os.Stat(filepath.Join(root, "config/preview/subyard-preview"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (runtime *Runtime) guardRetainedRuntimeInputs(prepared Prepared, approved, activeRoot, targetRoot, configHome string, targetOwners map[string]bool) Prepared {
	execute := prepared.run
	prepared.run = func(ctx context.Context) error {
		binding, err := runtime.retainedRuntimeBinding(ctx, activeRoot, targetRoot, configHome, targetOwners)
		if err != nil || binding != approved {
			return fmt.Errorf("%w: installed runtime rollback inputs changed", domain.ErrPlanStale)
		}
		return execute(ctx)
	}
	return prepared
}

func (runtime *Runtime) retainedRuntimeBinding(ctx context.Context, activeRoot, targetRoot, configHome string, targetOwners map[string]bool) (string, error) {
	owners := []string{}
	if targetOwners == nil {
		definitions, err := profile.Load(activeRoot)
		if err != nil {
			return "", err
		}
		for _, definition := range definitions {
			if definition.Runtime != nil {
				owners = append(owners, definition.Runtime.ActivationID)
			}
		}
	} else {
		for owner, supported := range targetOwners {
			if supported {
				owners = append(owners, owner)
			}
		}
	}
	support, err := CaptureRollbackSupport(ctx, runtime.config, activeRoot, targetRoot, configHome, owners, nil)
	if err != nil {
		return "", err
	}
	refusals := []error{}
	for _, consumer := range support.Consumers {
		if consumer.Kind == "profile" {
			loses := consumer.RetainedContract != "same" || targetOwners != nil && !targetOwners[consumer.Owner]
			refusal := retainedProfileRuntimeRefusal{yard: consumer.Yard, profile: consumer.Profile, owner: consumer.Owner,
				disable: consumer.Selected && consumer.Disableable}
			if loses && (consumer.ManagedPathsPresent || consumer.ManagedPaths == 0 && consumer.Selected) {
				refusals = append(refusals, refusal)
				continue
			}
			// An absent runtime, or a converged one that owns nothing or is deactivated,
			// leaves the retained release no installed activation to take over.
			if loses && (consumer.Observation == nil || consumer.Observation.State != ports.RuntimeStateAbsent &&
				!(consumer.Observation.State == ports.RuntimeStateCurrent && (consumer.Observation.OwnershipAbsent || consumer.Observation.Deactivated))) {
				refusals = append(refusals, refusal)
			}
		} else if !targetOwners[consumer.Owner] {
			for _, part := range consumer.Installed {
				if part.Present && (part.Payload != "same" || part.Settings != "same") {
					refusals = append(refusals, errors.New("retained release cannot activate installed core helper contract"))
					break
				}
			}
		}
	}
	if err := domain.CollectOrderedRefusals(refusals...); err != nil {
		return "", err
	}
	return support.Binding, nil
}

// retainedProfileRuntimeRefusal names an installed profile runtime that a
// retained release cannot activate. disable reports an enabled profile whose
// `profile disable` in that yard deactivates the runtime.
type retainedProfileRuntimeRefusal struct {
	yard, profile, owner string
	disable              bool
}

func (refusal retainedProfileRuntimeRefusal) Error() string {
	return fmt.Sprintf("retained release cannot activate installed profile runtime %s in yard %s", refusal.owner, refusal.yard)
}

// olderRuntimeOutcomeText is the public message and next step for a refused
// older target. Enabled profiles the operator can disable are named with the
// command; any other refusal keeps the general message and fallback.
func olderRuntimeOutcomeText(refusal error, fallback string) (string, string) {
	causes := []error{refusal}
	if joined, ok := refusal.(interface{ Unwrap() []error }); ok {
		causes = joined.Unwrap()
	}
	named, commands := []string{}, []string{}
	for _, cause := range causes {
		var profile retainedProfileRuntimeRefusal
		if !errors.As(cause, &profile) || !profile.disable || !domain.SafeName(profile.profile) || !domain.SafeName(profile.yard) {
			return olderRuntimeRefusal, fallback
		}
		named = append(named, fmt.Sprintf("profile %s in yard %s", profile.profile, profile.yard))
		commands = append(commands, fmt.Sprintf("profile %s in yard %s (yard -Y %s profile disable %s)", profile.profile, profile.yard, profile.yard, profile.profile))
	}
	message := "the retained release cannot activate the installed runtime of enabled " + strings.Join(named, ", ")
	retry := "disable " + strings.Join(commands, " and ") + ", then run yard update --check"
	if len(message) > 512 || len(retry) > 512 {
		return olderRuntimeRefusal, fallback
	}
	return message, retry
}
