package reconcileruntime

import (
	"maps"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
)

var inheritedReleaseRoot = regexp.MustCompile(`^/proc/[0-9]+/fd/[0-9]+/(.+)$`)

// shippedReleasePath returns the shipped config path inside a release root:
// an inherited /proc/<pid>/fd/<n> handle or a directory under releases/<id>.
func shippedReleasePath(path string) (string, bool) {
	path = filepath.Clean(path)
	relative := ""
	if match := inheritedReleaseRoot.FindStringSubmatch(path); match != nil {
		relative = match[1]
	} else {
		parts := strings.Split(path, "/")
		for index := len(parts) - 3; index > 0; index-- {
			if parts[index] == "releases" && parts[index+1] != "" {
				relative = strings.Join(parts[index+2:], "/")
				break
			}
		}
	}
	if relative == "" || !strings.HasPrefix(relative, "config/") || !filepath.IsLocal(relative) {
		return "", false
	}
	return relative, true
}

// Applied snapshots record shipped integration sources under the root of the
// capturing release. That root may be an ephemeral process handle or a
// release that is later pruned. A shipped source follows the reading release
// when the current value is that release's own copy of the same shipped file;
// operator-authored paths stay exact even when shaped like a release path.
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
		if current == "" || current == applied {
			continue
		}
		appliedPath, appliedShipped := shippedReleasePath(applied)
		if appliedShipped && runtime.RepositoryRoot != "" && filepath.Clean(current) == filepath.Join(runtime.RepositoryRoot, appliedPath) {
			relocated[name] = current
		}
	}
	return relocated
}
