package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func fullOwnedConsumer(raw string, projects map[string]bool) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return projects[parsed.Query().Get("project")]
}

func (cli *CLI) captureFullShared(ctx context.Context, execution *fullTeardown, knownProjects, legacyProjects map[string]bool) ([]uninstall.Tree, error) {
	incus, _ := cli.statusPorts()
	reader, ok := incus.(ports.InstallationSharedReader)
	if !ok {
		if len(execution.Yards) == 0 {
			return nil, nil
		}
		return nil, errors.New("full uninstall requires exact shared consumer and backing-path inventory")
	}
	all := map[string]ports.TeardownResource{}
	legacyShared := map[string]bool{}
	storagePaths := map[string]bool{}
	extraRoots := []uninstall.Tree{}
	owned, err := reader.InstallationSharedInventory(ctx, "", "")
	if err != nil {
		return nil, err
	}
	for _, resource := range owned {
		if resource.Kind == "host-profile" {
			all[resource.Kind+"/"+resource.Name] = resource
			continue
		}
		if resource.Owner == nil || resource.Owner.UID != strconv.Itoa(int(execution.Installation.OperatorUID)) || resource.Owner.DataHome != execution.Installation.DataHome {
			continue
		}
		if resource.Owner.ConfigHome != execution.Installation.ConfigHome || resource.Owner.Context == nil {
			return nil, fmt.Errorf("shared %s %s has an incomplete installation boundary; restore its recorded host context before full removal", resource.Kind, resource.Name)
		}
		storagePaths[resource.Owner.Context.Paths.StoragePath] = true
		extraRoots = append(extraRoots, uninstall.Tree{Path: resource.Owner.Context.Paths.HostBase, Category: "host"})
		all[resource.Kind+"/"+resource.Name] = resource
	}

	for _, yard := range execution.Yards {
		if !yard.Snapshot.State.ProjectFound && !yard.Snapshot.State.InstanceFound {
			continue
		}
		storagePaths[yard.Context.Paths.StoragePath] = true
		resources, err := reader.InstallationSharedInventory(ctx, firstNonempty(yard.Environment["STORAGE_POOL"], yard.Environment["SRV_POOL"]), yard.Context.IncusBridge)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			key := resource.Kind + "/" + resource.Name
			if legacyProjects[yard.Context.IncusProject] {
				legacyShared[key] = true
			}
			if existing, exists := all[key]; exists && existing.Binding != resource.Binding {
				return nil, errors.New("shared teardown ownership changed during inventory")
			}
			all[key] = resource
		}
	}
	profile, hasProfile := all["host-profile/default"]
	foreignProfile := false
	if hasProfile {
		for _, consumer := range profile.UsedBy {
			if !fullOwnedConsumer(consumer, knownProjects) {
				foreignProfile = true
			}
		}
	}
	trees := extraRoots
	keys := []string{}
	for key := range all {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		resource := all[key]
		if legacyShared[key] {
			execution.Preserved = append(execution.Preserved, "legacy shared "+resource.Kind+" "+resource.Name+" has no installation ownership record; preserved")
			if resource.Kind == "pool" && filepath.IsAbs(resource.Path) {
				trees = append(trees, uninstall.Tree{Path: resource.Path, Category: "preserved-storage"})
			}
			continue
		}
		if resource.Kind == "host-profile" {
			execution.Shared = append(execution.Shared, resource)
			continue
		}
		foreign := false
		for _, consumer := range resource.UsedBy {
			parsed, err := url.Parse(consumer)
			defaultProfile := err == nil && parsed.Path == "/1.0/profiles/default" && (parsed.Query().Get("project") == "" || parsed.Query().Get("project") == "default")
			if defaultProfile && !foreignProfile {
				continue
			}
			if !fullOwnedConsumer(consumer, knownProjects) {
				foreign = true
			}
		}
		if foreign {
			execution.Preserved = append(execution.Preserved, "shared "+resource.Kind+" "+resource.Name+" and its backing data used by foreign consumers")
			// A shared pool's backing path is explicitly excluded even if it overlaps
			// one of this installation's data roots.
			if resource.Kind == "pool" && filepath.IsAbs(resource.Path) {
				tree := uninstall.Tree{Path: resource.Path, Category: "preserved-storage"}
				trees = append(trees, tree)
			}
			continue
		}
		if resource.Kind == "pool" && resource.Path != "" {
			if !filepath.IsAbs(resource.Path) || filepath.Clean(resource.Path) != resource.Path || !storagePaths[resource.Path] {
				return nil, fmt.Errorf("storage pool %s backing path has unknown installation ownership; preserve it and restore its recorded storage context", resource.Name)
			}
			tree, err := uninstall.CapturePrivate(resource.Path)
			if err != nil {
				return nil, err
			}
			if len(tree.Entries) > 0 {
				tree.Entries = tree.Entries[:1]
				tree.Opaque = true
			}
			tree.Category = "storage"
			tree.Resource = &resource
			trees = append(trees, tree)
		}
		execution.Shared = append(execution.Shared, resource)
	}
	return trees, nil
}

func (execution *fullTeardown) sharedSteps() []domain.OperationStep {
	steps := []domain.OperationStep{}
	for index, resource := range execution.Shared {
		if resource.Kind == "host-profile" {
			continue
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("shared-%d", index), Target: resource.Kind + "/" + resource.Name, Observed: "owned consumers", Desired: "absent", Decision: domain.StepApply, Preconditions: []string{"exact identity unchanged and no foreign consumers remain"}, Verify: "resource absent before removing its backing storage", Consequence: "remove unused " + resource.Kind + " " + resource.Name})
	}
	return steps
}
