package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
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
	active, err := profile.Load(activeRoot)
	if err != nil {
		return "", err
	}
	target, err := profile.Load(targetRoot)
	if err != nil {
		return "", err
	}
	targets := map[string]profile.Definition{}
	for _, definition := range target {
		targets[definition.Name] = definition
	}
	applicable, err := retainedRuntimeApplicable(activeRoot)
	if err != nil {
		return "", err
	}
	if !applicable {
		return fmt.Sprintf("%x", sha256.Sum256([]byte("no-installed-runtime-contract"))), nil
	}
	environment := runtime.config.Inputs.environment(runtime.config.Context)
	operator := first(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"])
	names, err := config.YardNames(filepath.Join(activeRoot, "config"), configHome)
	if err != nil {
		return "", err
	}
	facts := map[string]any{}
	for _, name := range names {
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: activeRoot, OperatorHome: operator, YardName: name, Environment: environment})
		if err != nil {
			return "", errors.New("cannot assess installed runtime rollback selection")
		}
		if loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		for _, definition := range active {
			if definition.Runtime == nil {
				continue
			}
			retained := targets[definition.Name]
			losesOwner := retained.Runtime == nil || retained.Runtime.ActivationID != definition.Runtime.ActivationID || targetOwners != nil && !targetOwners[definition.Runtime.ActivationID]
			if losesOwner && len(definition.ManagedPaths) == 0 && definition.Selected(name, loaded.Environment) {
				return "", fmt.Errorf("retained release cannot activate selected profile runtime %s", definition.Runtime.ActivationID)
			}
			for _, managed := range definition.ManagedPaths {
				root := loaded.Context.Paths.DataHome
				if managed.Root == "operator" {
					root = loaded.Context.Paths.OperatorHome
				}
				path := filepath.Join(root, strings.ReplaceAll(managed.Path, "{yard}", name))
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return "", err
				}
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok {
					return "", errors.New("installed runtime metadata unavailable")
				}
				facts[path] = struct {
					Mode              os.FileMode
					Device, Inode     uint64
					UID, GID          uint32
					Modified, Changed syscall.Timespec
				}{info.Mode(), stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mtim, stat.Ctim}
				retained := targets[definition.Name]
				if retained.Runtime == nil || retained.Runtime.ActivationID != definition.Runtime.ActivationID {
					return "", fmt.Errorf("retained release cannot activate installed profile runtime %s", definition.Runtime.ActivationID)
				}
				if targetOwners != nil && !targetOwners[definition.Runtime.ActivationID] {
					return "", fmt.Errorf("retained release does not own installed runtime %s", definition.Runtime.ActivationID)
				}
			}
			// Host receipts do not describe guest leftovers after deselection or
			// partial cleanup. Only native absence permits losing their owner.
			if losesOwner {
				if runtime.config.ProfileRuntimeObservation == nil {
					return "", errors.New("installed profile runtime observer is unavailable")
				}
				observation, err := runtime.config.ProfileRuntimeObservation(ctx, activeRoot, loaded, definition.Runtime.ActivationID)
				if err != nil {
					return "", err
				}
				facts["profile:"+name+":"+definition.Runtime.ActivationID] = observation
				if observation.State != ports.RuntimeStateAbsent && !(observation.State == ports.RuntimeStateCurrent && observation.OwnershipAbsent) {
					return "", fmt.Errorf("retained release cannot activate installed profile runtime %s", definition.Runtime.ActivationID)
				}
			}
		}
		if err := runtime.retainedCoreRuntimeBinding(ctx, activeRoot, targetRoot, name, operator, environment, loaded, targetOwners, facts); err != nil {
			return "", err
		}
		facts["selection:"+name] = loaded.Environment["ENVIRONMENT_PROFILES"]
	}
	payload, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

// Installed applicability and payload compatibility are separate. An absent
// consumer needs no retained activation owner; every installed local yard uses
// its own effective settings, including named-yard pins and roots.
func (runtime *Runtime) retainedCoreRuntimeBinding(ctx context.Context, activeRoot, targetRoot, name, operator string, environment map[string]string, loaded config.Loaded, targetOwners map[string]bool, facts map[string]any) error {
	if _, err := os.Stat(filepath.Join(activeRoot, "config/preview/subyard-preview")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	retained, err := config.Load(config.LoadOptions{RepositoryRoot: targetRoot, OperatorHome: operator, YardName: name, Environment: environment})
	if err != nil {
		return err
	}
	for _, id := range []string{"core-guest-runtime", "key-tools-runtime"} {
		// An identical retained class adds no new applicability prerequisite.
		// In particular, a host-only change must not require a guest transport.
		pathsToCompare := []string{"config/preview/subyard-preview"}
		settingsToCompare := []string{"CCUSAGE_VERSION", "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64"}
		if id == "key-tools-runtime" {
			pathsToCompare = []string{"config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in"}
			settingsToCompare = []string{"SUBYARD_AGE_VERSION", "SUBYARD_SOPS_VERSION", "SUBYARD_AGE_SHA256_AMD64", "SUBYARD_AGE_SHA256_ARM64", "SUBYARD_SOPS_SHA256_AMD64", "SUBYARD_SOPS_SHA256_ARM64", "SUBYARD_KEYS_ROOT", "SUBYARD_KEYS_TOOLS_DIR", "SUBYARD_KEYS_SYSTEMD_DIR", "YARD_RUNTIME_ROOT"}
		}
		different := false
		for _, path := range pathsToCompare {
			current, err := os.ReadFile(filepath.Join(activeRoot, path))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			previous, err := os.ReadFile(filepath.Join(targetRoot, path))
			different = different || err != nil || string(current) != string(previous)
		}
		for _, key := range settingsToCompare {
			different = different || loaded.Environment[key] != retained.Environment[key]
		}
		if !different {
			continue
		}
		if runtime.config.InstalledRuntimePresence == nil {
			return errors.New("installed runtime presence observer is unavailable")
		}
		presence, err := runtime.config.InstalledRuntimePresence(ctx, activeRoot, loaded, id)
		if err != nil {
			return err
		}
		facts["presence:"+name+":"+id] = presence
		paths := []string{}
		pins := []string{}
		roots := []string{}
		if presence.Preview {
			paths = append(paths, "config/preview/subyard-preview")
		}
		if presence.CCUsage {
			pins = append(pins, "CCUSAGE_VERSION", "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64")
		}
		if presence.KeyTools {
			pins = append(pins, "SUBYARD_AGE_VERSION", "SUBYARD_SOPS_VERSION", "SUBYARD_AGE_SHA256_AMD64", "SUBYARD_AGE_SHA256_ARM64", "SUBYARD_SOPS_SHA256_AMD64", "SUBYARD_SOPS_SHA256_ARM64")
			roots = append(roots, "SUBYARD_KEYS_ROOT", "SUBYARD_KEYS_TOOLS_DIR")
		}
		if presence.KeyTimer {
			paths = append(paths, "config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in")
			roots = append(roots, "SUBYARD_KEYS_SYSTEMD_DIR", "YARD_RUNTIME_ROOT")
		}
		settings := map[string][2]string{}
		for _, key := range append(pins, roots...) {
			settings[key] = [2]string{loaded.Environment[key], retained.Environment[key]}
			if loaded.Environment[key] != retained.Environment[key] && !targetOwners[id] {
				return errors.New("retained release cannot activate installed native tool settings")
			}
		}
		facts["settings:"+name+":"+id] = settings
		for _, path := range paths {
			current, err := os.ReadFile(filepath.Join(activeRoot, path))
			if err != nil {
				return err
			}
			previous, err := os.ReadFile(filepath.Join(targetRoot, path))
			if (err != nil || string(current) != string(previous)) && !targetOwners[id] {
				return errors.New("retained release cannot activate installed core helper contract")
			}
		}
	}
	return nil
}
