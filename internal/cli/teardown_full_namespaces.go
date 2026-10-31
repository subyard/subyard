package cli

import (
	"fmt"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/uninstall"
	"path/filepath"
	"strings"
)

// Installation roots are containers for explicit owner namespaces. An ordinary
// foreign file beside those namespaces is never approved by tree capture alone.
func fullRootNamespaces(root string, installation uninstall.Installation, execution *fullTeardown, definitions []profile.Definition) ([]string, bool, error) {
	owned := []string{}
	scoped := false
	switch root {
	case installation.DataHome:
		scoped = true
		owned = []string{uninstall.ManifestName, "runtime", "releases", "ownership", "logs", "ssh", "owner-inventory", "services", "exports", "cache", "recovery", "workspaces", "tools", "keys", "state", "projects", "space.cache", "space.cache.lock", "space.cache.tmp"}
		for _, yard := range execution.Yards {
			if yard.Context.YardName != "default" {
				suffix := "space-" + yard.Context.YardName + ".cache"
				owned = append(owned, suffix, suffix+".lock", suffix+".tmp")
			}
			for _, definition := range definitions {
				for _, managed := range definition.ManagedPaths {
					if managed.Root == "data" {
						owned = append(owned, strings.ReplaceAll(managed.Path, "{yard}", yard.Context.YardName))
					}
				}
			}
		}
	case installation.ConfigHome:
		scoped = true
		owned = []string{"config.env", "settings.env", "host.env", "overrides.env", "hosts", "host", "yards", "generated", "projects", "keys", "tools", "git", "recovery", "migrations", "owner-inventory", "host-id", "host-id.json", "configuration-source.json", "config-source.json", "source.json", "settings", ".sync", "overrides", "secrets", "release-transition", "host-id-rename.json", "reconcile"}
	case installation.RuntimeRoot:
		scoped = true
		owned = []string{"current", "previous", "releases", uninstall.ManifestName, "update.lock"}
	case installation.CacheRoot:
		scoped = true
		for _, original := range installation.CacheFiles {
			current, err := uninstall.Capture(filepath.Join(root, original.Path))
			if err != nil {
				return nil, false, err
			}
			if len(current.Entries) > 0 {
				fresh := current.Entries[0]
				fresh.Path = original.Path
				if len(current.Entries) != 1 || fresh != original {
					return nil, false, fmt.Errorf("cache artifact no longer matches its installer ownership: %s", filepath.Join(root, original.Path))
				}
			}
			owned = append(owned, original.Path)
		}
	}
	for _, yard := range execution.Yards {
		for _, path := range []string{yard.Context.Paths.StateDir, yard.Environment["SUBYARD_KEYS_ROOT"], yard.Environment["SUBYARD_KEYS_CONSUMER_ROOT"], yard.Environment["SUBYARD_KEYS_TOOLS_DIR"]} {
			if path != "" && strings.HasPrefix(path, root+"/") {
				relative, _ := filepath.Rel(root, path)
				owned = append(owned, relative)
			}
		}
	}
	for _, path := range execution.ManagedRoots {
		if strings.HasPrefix(path, root+"/") {
			relative, _ := filepath.Rel(root, path)
			owned = append(owned, relative)
		}
	}
	return owned, scoped, nil
}
