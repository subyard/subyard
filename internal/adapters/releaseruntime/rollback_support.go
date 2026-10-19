package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

// CaptureRollbackSupport captures owning component facts from retained roots.
// Roots may be descriptor read prefixes; they never enter the resulting binding.
// Release owners decide whether these observations permit a rollback.
func CaptureRollbackSupport(ctx context.Context, configuration Config, sourceRoot, targetRoot, configHome string, targetOwners []string, files *config.ProspectiveFiles) (ports.RollbackSupport, error) {
	result := ports.RollbackSupport{Consumers: []ports.RollbackConsumer{}}
	active, err := profile.Load(sourceRoot)
	if err != nil {
		return result, err
	}
	target, err := profile.Load(targetRoot)
	if err != nil {
		return result, err
	}
	targets := map[string]profile.Definition{}
	for _, definition := range target {
		targets[definition.Name] = definition
	}
	owners := map[string]bool{}
	for _, owner := range targetOwners {
		owners[owner] = true
	}
	applicable, err := retainedRuntimeApplicable(sourceRoot)
	if err != nil {
		return result, err
	}
	facts := map[string]any{}
	if applicable {
		environment := configuration.Inputs.environment(configuration.Context)
		operator := first(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"])
		var names []string
		if files != nil {
			names = append([]string{}, files.Yards...)
		} else {
			names, err = config.YardNames(filepath.Join(sourceRoot, "config"), configHome)
			if err != nil {
				return result, err
			}
		}
		for _, name := range names {
			loaded, err := config.Load(config.LoadOptions{RepositoryRoot: sourceRoot, OperatorHome: operator, YardName: name, Environment: environment, ProspectiveFiles: files, DisablePrivate: files != nil, ConfigLocked: files != nil})
			if err != nil {
				return result, errors.New("cannot capture retained runtime selection")
			}
			if loaded.Context.AccessKind != domain.AccessLocal {
				continue
			}
			for _, definition := range active {
				if definition.Runtime == nil {
					continue
				}
				consumer := ports.RollbackConsumer{Kind: "profile", Yard: name, Owner: definition.Runtime.ActivationID,
					Selected: definition.Selected(name, loaded.Environment), ManagedPaths: len(definition.ManagedPaths), RetainedContract: "absent", RetainedPayload: "same", RetainedSettings: "same"}
				retained := targets[definition.Name]
				if retained.Runtime != nil {
					consumer.RetainedContract = sameRollbackFact(retained.Runtime.ActivationID == definition.Runtime.ActivationID)
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
						return result, err
					}
					stat, ok := info.Sys().(*syscall.Stat_t)
					if !ok {
						return result, errors.New("installed runtime metadata unavailable")
					}
					consumer.ManagedPathsPresent = true
					facts[path] = struct {
						Mode              os.FileMode
						Device, Inode     uint64
						UID, GID          uint32
						Modified, Changed syscall.Timespec
					}{info.Mode(), stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mtim, stat.Ctim}
				}
				if (consumer.RetainedContract != "same" || !owners[consumer.Owner]) && !consumer.ManagedPathsPresent && !(consumer.ManagedPaths == 0 && consumer.Selected) {
					if configuration.ProfileRuntimeObservation == nil {
						return result, errors.New("installed profile runtime observer is unavailable")
					}
					observation, err := configuration.ProfileRuntimeObservation(ctx, sourceRoot, loaded, consumer.Owner)
					if err != nil {
						return result, err
					}
					consumer.Observation = &observation
				}
				result.Consumers = append(result.Consumers, consumer)
			}
			core, err := captureCoreRollbackSupport(ctx, configuration, sourceRoot, targetRoot, name, operator, environment, loaded, facts, files)
			if err != nil {
				return result, err
			}
			result.Consumers = append(result.Consumers, core...)
			facts["selection:"+name] = loaded.Environment["ENVIRONMENT_PROFILES"]
		}
	}
	raw, err := json.Marshal(struct {
		Consumers []ports.RollbackConsumer
		ReadSet   map[string]any
	}{result.Consumers, facts})
	if err != nil {
		return result, err
	}
	result.Binding = fmtRollbackDigest(raw)
	return result, nil
}

func sameRollbackFact(same bool) string {
	if same {
		return "same"
	}
	return "different"
}
func fmtRollbackDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func captureCoreRollbackSupport(ctx context.Context, configuration Config, sourceRoot, targetRoot, name, operator string, environment map[string]string, loaded config.Loaded, facts map[string]any, files *config.ProspectiveFiles) ([]ports.RollbackConsumer, error) {
	if _, err := os.Stat(filepath.Join(sourceRoot, "config/preview/subyard-preview")); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	retained, err := config.Load(config.LoadOptions{RepositoryRoot: targetRoot, OperatorHome: operator, YardName: name, Environment: environment, ProspectiveFiles: files, DisablePrivate: files != nil, ConfigLocked: files != nil})
	if err != nil {
		return nil, err
	}
	result := []ports.RollbackConsumer{}
	for _, owner := range []string{"core-guest-runtime", "key-tools-runtime"} {
		consumer := ports.RollbackConsumer{Kind: "core", Yard: name, Owner: owner, RetainedContract: "same", RetainedPayload: "same", RetainedSettings: "same"}
		paths := []string{"config/preview/subyard-preview"}
		settings := []string{"CCUSAGE_VERSION", "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64"}
		if owner == "key-tools-runtime" {
			paths = []string{"config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in"}
			settings = []string{"SUBYARD_AGE_VERSION", "SUBYARD_SOPS_VERSION", "SUBYARD_AGE_SHA256_AMD64", "SUBYARD_AGE_SHA256_ARM64", "SUBYARD_SOPS_SHA256_AMD64", "SUBYARD_SOPS_SHA256_ARM64", "SUBYARD_KEYS_ROOT", "SUBYARD_KEYS_TOOLS_DIR", "SUBYARD_KEYS_SYSTEMD_DIR", "YARD_RUNTIME_ROOT"}
		}
		comparePaths := func(paths []string, missingSourceAllowed bool) (bool, error) {
			same := true
			for _, path := range paths {
				current, err := os.ReadFile(filepath.Join(sourceRoot, path))
				if os.IsNotExist(err) && missingSourceAllowed {
					continue
				}
				if err != nil {
					return false, err
				}
				previous, err := os.ReadFile(filepath.Join(targetRoot, path))
				same = same && err == nil && string(current) == string(previous)
				facts["payload:"+name+":"+owner+":"+path] = [2]string{fmtRollbackDigest(current), fmtRollbackDigest(previous)}
			}
			return same, nil
		}
		compareSettings := func(keys []string) bool {
			same := true
			values := map[string][2]string{}
			for _, key := range keys {
				values[key] = [2]string{loaded.Environment[key], retained.Environment[key]}
				same = same && loaded.Environment[key] == retained.Environment[key]
			}
			facts["settings:"+name+":"+owner+":"+strings.Join(keys, ",")] = values
			return same
		}
		payloadSame, err := comparePaths(paths, true)
		if err != nil {
			return nil, err
		}
		settingsSame := compareSettings(settings)
		consumer.RetainedPayload, consumer.RetainedSettings = sameRollbackFact(payloadSame), sameRollbackFact(settingsSame)
		if !payloadSame || !settingsSame {
			if configuration.InstalledRuntimePresence == nil {
				return nil, errors.New("installed runtime presence observer is unavailable")
			}
			presence, err := configuration.InstalledRuntimePresence(ctx, sourceRoot, loaded, owner)
			if err != nil {
				return nil, err
			}
			facts["presence:"+name+":"+owner] = presence
			consumer.Installed = []ports.RollbackInstalledPart{}
			parts := []struct {
				kind            string
				present         bool
				paths, settings []string
			}{
				{"preview", presence.Preview, []string{"config/preview/subyard-preview"}, nil},
				{"ccusage", presence.CCUsage, nil, []string{"CCUSAGE_VERSION", "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64"}},
				{"key-tools", presence.KeyTools, nil, []string{"SUBYARD_AGE_VERSION", "SUBYARD_SOPS_VERSION", "SUBYARD_AGE_SHA256_AMD64", "SUBYARD_AGE_SHA256_ARM64", "SUBYARD_SOPS_SHA256_AMD64", "SUBYARD_SOPS_SHA256_ARM64", "SUBYARD_KEYS_ROOT", "SUBYARD_KEYS_TOOLS_DIR"}},
				{"key-timer", presence.KeyTimer, []string{"config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in"}, []string{"SUBYARD_KEYS_SYSTEMD_DIR", "YARD_RUNTIME_ROOT"}},
			}
			for _, part := range parts {
				p := ports.RollbackInstalledPart{Kind: part.kind, Present: part.present, Payload: "same", Settings: "same"}
				if part.present {
					same, err := comparePaths(part.paths, false)
					if err != nil {
						return nil, err
					}
					p.Payload = sameRollbackFact(same)
					p.Settings = sameRollbackFact(compareSettings(part.settings))
				}
				consumer.Installed = append(consumer.Installed, p)
			}
		}
		result = append(result, consumer)
	}
	return result, nil
}
