package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/uninstall"
	"io"
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
		owned = []string{"release-transition-replaced-journals", "config.env", "settings.env", "host.env", "overrides.env", "hosts", "host", "yards", "generated", "projects", "keys", "tools", "git", "recovery", "migrations", "owner-inventory", "host-id", "host-id.json", "configuration-source.json", "config-source.json", "source.json", "settings", ".sync", "overrides", "secrets", "release-transition", "host-id-rename.json", "reconcile"}
		for _, yard := range execution.Yards {
			path, err := ownedIdentityRemovalMarker(root, yard)
			if err != nil {
				return nil, false, err
			}
			if path != "" {
				owned = append(owned, path)
			}
		}
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

// A full removal may retire a pending identity fence only for a yard already
// included in its independently verified native deletion scope. The scoped
// uninstall tree subsequently binds bytes/inode/mode and refuses changed files.
func ownedIdentityRemovalMarker(root string, yard uninstall.RecoveryYard) (string, error) {
	path := config.DeveloperIdentityMarkerPath(root, yard.Context.YardName)
	snapshot, err := config.ReadPersistentFileSnapshot(root, path)
	if err != nil {
		return "", err
	}
	if !snapshot.Exists {
		return "", nil
	}
	var marker config.DeveloperIdentityMarker
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Content))
	decoder.DisallowUnknownFields()
	if snapshot.Identity.Mode&0777 != 0600 || len(snapshot.Content) > 2<<20 || decoder.Decode(&marker) != nil || decoder.Decode(new(any)) != io.EOF || marker.Schema != 1 || marker.Yard != yard.Context.YardName || len(marker.Inputs) != 64 || len(marker.Release) != 64 {
		return "", fmt.Errorf("unsafe pending identity removal marker for yard %s", yard.Context.YardName)
	}
	var proof map[string]any
	decoder = json.NewDecoder(strings.NewReader(marker.Proof))
	decoder.UseNumber()
	if decoder.Decode(&proof) != nil || decoder.Decode(new(any)) != io.EOF || operationStateDigest(proof) != marker.Identity {
		return "", fmt.Errorf("invalid pending identity removal proof")
	}
	native, ok := proof["yard"].(map[string]any)
	if !ok || native["name"] != yard.Context.YardName || native["project"] != yard.Context.IncusProject || native["instance"] != yard.Context.YardInstanceName {
		return "", fmt.Errorf("pending identity removal scope changed")
	}
	uuid, ok := native["uuid"].(string)
	if !ok || len(uuid) != 36 || yard.Snapshot == nil || !yard.Snapshot.State.InstanceFound || yard.Snapshot.State.Instance.Config["volatile.uuid"] != uuid {
		return "", fmt.Errorf("pending identity removal native yard changed")
	}
	return filepath.Base(path), nil
}
