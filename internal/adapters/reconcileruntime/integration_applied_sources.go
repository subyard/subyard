package reconcileruntime

import (
	"maps"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
)

var inheritedReleaseRoot = config.InheritedReleaseRoot

func shippedReleasePath(path string) (string, bool) { return config.ShippedReleasePath(path) }

// Applied snapshots record shipped integration sources under the root of the
// capturing release. That root may be an ephemeral process handle or a
// release that is later pruned. A shipped source follows the reading release
// when the current value is that release's own copy of the same shipped file;
// operator-authored paths stay exact even when shaped like a release path.
// A process handle names no file once its process exits and is never an
// operator path, so it also follows the reading release's copy of that shipped
// file when the current value is unset or awaits init; without that copy the
// capture stays exact.
func (runtime Runtime) relocateAppliedShippedSources(catalog config.Catalog, settings map[string]string) map[string]string {
	if settings == nil {
		return nil
	}
	relocated := maps.Clone(settings)
	for name, applied := range settings {
		definition, ok := catalog.LookupSetting(name)
		if !ok || definition.Type != config.SettingRegularFilePath {
			continue
		}
		current := runtime.environmentValue(name)
		if current == applied || runtime.RepositoryRoot == "" {
			continue
		}
		appliedPath, appliedShipped := shippedReleasePath(applied)
		if !appliedShipped {
			continue
		}
		release := filepath.Join(runtime.RepositoryRoot, appliedPath)
		if current != "" && filepath.Clean(current) == release {
			relocated[name] = current
		} else if inheritedReleaseRoot.MatchString(filepath.Clean(applied)) && shippedRegularFile(release) {
			relocated[name] = release
		}
	}
	return relocated
}

func shippedRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
