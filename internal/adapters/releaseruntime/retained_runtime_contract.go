package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
		if definition.Runtime != nil {
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
	for _, consumer := range support.Consumers {
		if consumer.Kind == "profile" {
			loses := consumer.RetainedContract != "same" || targetOwners != nil && !targetOwners[consumer.Owner]
			if loses && (consumer.ManagedPathsPresent || consumer.ManagedPaths == 0 && consumer.Selected) {
				return "", fmt.Errorf("retained release cannot activate installed profile runtime %s", consumer.Owner)
			}
			if loses && (consumer.Observation == nil || consumer.Observation.State != ports.RuntimeStateAbsent && !(consumer.Observation.State == ports.RuntimeStateCurrent && consumer.Observation.OwnershipAbsent)) {
				return "", fmt.Errorf("retained release cannot activate installed profile runtime %s", consumer.Owner)
			}
		} else if !targetOwners[consumer.Owner] {
			for _, part := range consumer.Installed {
				if part.Present && (part.Payload != "same" || part.Settings != "same") {
					return "", errors.New("retained release cannot activate installed core helper contract")
				}
			}
		}
	}
	return support.Binding, nil
}
