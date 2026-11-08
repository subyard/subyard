package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func (cli *CLI) configDiagnosticState(ctx context.Context, target configTarget) (string, error) {
	if target.Loaded.Context.AccessKind == domain.AccessRemote {
		return "remote", nil
	}
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) || errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(instance.Status, "running") {
		return "stopped", nil
	}
	return "running", nil
}

// installedConfigurationEvidence accompanies the release projection without
// turning desired fallback values into evidence of installed state.
type installedConfigurationEvidence struct {
	applied reconcileruntime.AppliedIntegrationConfiguration
	known   map[string]bool
}

func configContentDiagnostics(target configTarget, applied reconcileruntime.AppliedIntegrationConfiguration) domain.YardConfigurationDiagnostic {
	result := domain.YardConfigurationDiagnostic{Yard: target.Name}
	// Reuse the materializer's consumer selection for instruction files; package
	// settings are owned by their selected integration's catalog namespace.
	sources, _ := config.SelectMaterializationSources(target.Loaded.Environment, target.Loaded.Context.DevUser, config.MaterializationRuntime, target.Loaded.Catalog.Integrations()...)
	active := map[string]bool{}
	for _, source := range sources {
		active[source.Asset.Setting] = true
	}
	consumer := func(name string) bool {
		if active[name] {
			return true
		}
		if !target.Loaded.Integrations.AllowsCodingTools {
			return false
		}
		if strings.HasPrefix(name, "CCUSAGE_") {
			return true
		}
		definition, _ := target.Loaded.Catalog.LookupSetting(name)
		for _, selected := range target.Loaded.Integrations.Effective {
			if strings.HasPrefix(name, "AGENT_"+selected+"_") || definition.Owner == "integration:"+selected {
				return true
			}
		}
		return false
	}
	if applied.Diagnostics == nil {
		for name := range target.Loaded.Environment {
			definition, _ := target.Loaded.Catalog.LookupSetting(name)
			if consumer(name) && target.Loaded.Environment[name] != "" && (definition.Sensitive || target.Loaded.Catalog.IntegrationDiagnosticFile(name)) {
				result.Unavailable = "some installed content unknown (no diagnostic baseline)"
				break
			}
		}

	} else {
		key, err := hex.DecodeString(applied.Diagnostics.Key)
		if err == nil && target.Loaded.Catalog.ValidIntegrationDiagnosticEvidence(applied.Diagnostics.Key, applied.Diagnostics.Digests) {
			now := target.Loaded.Catalog.IntegrationDiagnosticDigests(target.Loaded.Environment, key, target.Loaded.ProspectiveFiles.ReadAsset)
			names := map[string]bool{}
			for name := range applied.Diagnostics.Digests {
				names[name] = true
			}
			for name := range target.Loaded.Environment {
				if target.Loaded.Catalog.IntegrationDiagnosticSetting(name) {
					names[name] = true
				}
			}
			for name := range names {
				definition, ok := target.Loaded.Catalog.LookupSetting(name)
				file := target.Loaded.Catalog.IntegrationDiagnosticFile(name)
				if !consumer(name) || !ok || (!definition.Sensitive && !file) {
					continue
				}
				before, beforeOK := applied.Diagnostics.Digests[name]
				after, afterOK := now[name]
				oldValue, oldSet := applied.Settings[name]
				newValue, newSet := target.Loaded.Environment[name]
				if !definition.Sensitive && (oldSet != newSet || oldValue != newValue) {
					continue
				}
				if beforeOK && afterOK && before == after {
					continue
				}
				row := domain.ConfigurationDifference{Name: name, Installed: oldValue, Desired: newValue, InstalledSet: oldSet, DesiredSet: newSet}
				result.Differences = append(result.Differences, configContentDifference(row, definition, file, before, after))
			}
		}
		if key, err := hex.DecodeString(applied.Diagnostics.Key); err == nil {
			assets, err := effectiveConfigAssets(target.Loaded)
			if err == nil {
				for _, asset := range assets {
					installed, known := applied.Diagnostics.Files[asset.Setting]
					if !known {
						continue
					}
					if installed.Path != asset.Source {
						result.Differences = append(result.Differences, domain.ConfigurationDifference{Name: asset.Setting, Installed: installed.Path, Desired: asset.Source, InstalledSet: true, DesiredSet: true})
						continue
					}
					payload, err := target.Loaded.ProspectiveFiles.ReadAsset(config.MaterializedAsset{Source: asset.Source})
					definition, ok := target.Loaded.Catalog.LookupSetting(asset.Setting)
					if err != nil || !ok {
						result.Differences = append(result.Differences, domain.ConfigurationDifference{Name: asset.Setting, Kind: domain.ConfigurationContentUnknown})
					} else if config.ConfigurationInputDigest(asset.Setting, definition.Kind, key, payload) != installed.Digest {
						result.Differences = append(result.Differences, domain.ConfigurationDifference{Name: asset.Setting, Installed: installed.Path, Desired: asset.Source, InstalledSet: true, DesiredSet: true, Kind: domain.ConfigurationContentChanged})
					}
				}
			}
		}
	}
	return result
}

// Keep raw sensitive values outside the diagnostic row and operation-plan digest.
func configContentDifference(row domain.ConfigurationDifference, definition config.SettingDefinition, file bool, before, after string) domain.ConfigurationDifference {
	row.Kind = domain.ConfigurationContentChanged
	if definition.Sensitive {
		row.Kind = domain.ConfigurationSensitive
		row.Installed, row.Desired = "", ""
	}
	if (!definition.Sensitive || file) && (before == "" || after == "" && row.DesiredSet) {
		row.Kind = domain.ConfigurationContentUnknown
	}
	return row
}

// configInstalledDiagnostic distinguishes unavailable inspection from an empty
// applied settings snapshot. It never substitutes desired values for a baseline.
func (cli *CLI) configInstalledDiagnostic(ctx context.Context, target configTarget, state string) (domain.YardConfigurationDiagnostic, error) {
	result := domain.YardConfigurationDiagnostic{Yard: target.Name}
	if state == "stopped" {
		result.Unavailable = "stopped; not inspected"
		return result, nil
	}
	if state == "absent" || strings.HasPrefix(state, "remote") {
		result.Unavailable = "installed configuration unknown (not inspected)"
		return result, nil
	}
	installed, baseline, err := cli.installedReleaseTarget(ctx, target)
	if err != nil {
		return result, err
	}
	return cli.compareInstalledConfiguration(target, installed, baseline)
}

// Both status and pending-init use the same normalized, consumer-filtered rows.
// The release projection itself keeps its existing fallback and repair semantics.
func (cli *CLI) compareInstalledConfiguration(target, installed configTarget, baseline bool) (domain.YardConfigurationDiagnostic, error) {
	result := domain.YardConfigurationDiagnostic{Yard: target.Name}
	evidence := installed.installed
	if evidence == nil {
		evidence = &installedConfigurationEvidence{}
	}
	if !baseline {
		result.Unavailable = "installed integration settings unknown (no applied baseline)"
	}
	settings := pendingInitSettings(target.Loaded, installed.Loaded)
	settings = slices.DeleteFunc(settings, func(row pendingInitSetting) bool {
		return !evidence.known[row.Name] && !(baseline && (target.Loaded.Catalog.IntegrationInitSetting(row.Name) || row.Name == "INTEGRATION_HOST_LINKS"))
	})
	settings, err := cli.pendingEffectiveSettings(target.Loaded, installed.Loaded, settings)
	result.Differences = settings
	content := configContentDiagnostics(target, evidence.applied)
	if result.Unavailable == "" {
		result.Unavailable = content.Unavailable
	}
	result.Differences = append(result.Differences, content.Differences...)
	slices.SortFunc(result.Differences, func(a, b domain.ConfigurationDifference) int { return strings.Compare(a.Name, b.Name) })
	return result, err
}
