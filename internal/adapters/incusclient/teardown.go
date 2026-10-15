package incusclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/lxc/incus/v6/shared/api"
)

func (client *Client) InstallationYards(ctx context.Context) ([]ports.InstallationYard, error) {
	server, err := client.reconcileServer(ctx)
	if err != nil {
		if client.LocalInstallationAbsent() {
			return []ports.InstallationYard{}, nil
		}
		return nil, err
	}
	projects, err := server.GetProjects()
	if err != nil {
		return nil, normalizeError("read installation yard ownership", err)
	}
	result := []ports.InstallationYard{}
	for _, project := range projects {
		values := project.Config
		if values["user.subyard.installation"] == "" {
			continue
		}
		if values["user.subyard.installation"] != "1" {
			return nil, fmt.Errorf("unsupported Subyard installation ownership in project %s", project.Name)
		}
		var ownerContext *domain.Context
		if raw := values["user.subyard.context"]; raw != "" {
			if len(raw) > 16384 {
				return nil, fmt.Errorf("oversized installation context in project %s", project.Name)
			}
			ownerContext = &domain.Context{}
			if err := json.Unmarshal([]byte(raw), ownerContext); err != nil {
				return nil, fmt.Errorf("invalid installation context in project %s", project.Name)
			}
			if err := ownerContext.Validate(); err != nil || ownerContext.AccessKind != domain.AccessLocal || ownerContext.IncusProject != project.Name || ownerContext.Paths.DataHome != values["user.subyard.data_home"] || ownerContext.Paths.ConfigHome != values["user.subyard.config_home"] {
				return nil, fmt.Errorf("inconsistent installation context in project %s", project.Name)
			}
		}
		result = append(result, ports.InstallationYard{
			Context: ownerContext,
			Project: project.Name, Name: values["user.subyard.yard_name"], Instance: values["user.subyard.instance_name"],
			OperatorUID: values["user.subyard.operator_uid"], DataHome: values["user.subyard.data_home"], ConfigHome: values["user.subyard.config_home"],
			HostBase: values["user.subyard.host_base"], Bridge: values["user.subyard.bridge"], Pool: values["user.subyard.srv_pool"],
			Volume: values["user.subyard.srv_volume"], StoragePool: values["user.subyard.storage_pool"], StoragePath: values["user.subyard.storage_path"],
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Project < result[j].Project })
	return result, nil
}

func (client *Client) TeardownInventory(ctx context.Context, project string) ([]ports.TeardownResource, error) {
	if project == "" || project == "default" {
		return nil, fmt.Errorf("teardown requires a dedicated project")
	}
	server, err := client.reconcileServer(ctx)
	if err != nil {
		if client.LocalInstallationAbsent() {
			return []ports.TeardownResource{}, nil
		}
		return nil, err
	}
	config, _, err := server.GetProject(project)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, normalizeError("read teardown project", err)
	}
	scoped := server.UseProject(project)
	var result []ports.TeardownResource
	add := func(kind, name, pool string, value any) error {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(payload)
		result = append(result, ports.TeardownResource{Kind: kind, Name: name, Pool: pool, Binding: hex.EncodeToString(digest[:])})
		return nil
	}
	if err := add("project", project, "", map[string]any{"name": config.Name, "config": config.Config, "description": config.Description}); err != nil {
		return nil, err
	}
	instances, err := scoped.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, normalizeError("read teardown instances", err)
	}
	for _, instance := range instances {
		snapshots, err := scoped.GetInstanceSnapshotNames(instance.Name)
		if err != nil {
			return nil, normalizeError("read teardown instance snapshots", err)
		}
		sort.Strings(snapshots)
		if snapshots == nil {
			snapshots = []string{}
		}
		if err := add("instance", instance.Name, "", map[string]any{"name": instance.Name, "type": instance.Type, "created_at": instance.CreatedAt.UTC().Format(time.RFC3339Nano), "config": instance.Config, "devices": instance.Devices, "profiles": instance.Profiles, "snapshots": snapshots}); err != nil {
			return nil, err
		}
	}
	if config.Config["features.profiles"] != "false" {
		profiles, err := scoped.GetProfiles()
		if err != nil {
			return nil, normalizeError("read teardown profiles", err)
		}
		for _, profile := range profiles {
			if err := add("profile", profile.Name, "", map[string]any{"name": profile.Name, "config": profile.Config, "devices": profile.Devices, "description": profile.Description}); err != nil {
				return nil, err
			}
		}
	}
	if config.Config["features.images"] == "true" {
		images, err := scoped.GetImages()
		if err != nil {
			return nil, normalizeError("read teardown images", err)
		}
		for _, image := range images {
			if err := add("image", image.Fingerprint, "", image.Fingerprint); err != nil {
				return nil, err
			}
		}
	}
	var pools []api.StoragePool
	if config.Config["features.storage.volumes"] != "false" {
		pools, err = server.GetStoragePools()
		if err != nil {
			return nil, normalizeError("read teardown storage pools", err)
		}
	}
	for _, pool := range pools {
		volumes, err := scoped.GetStoragePoolVolumes(pool.Name)
		if err != nil {
			return nil, normalizeError("read teardown volumes", err)
		}
		for _, volume := range volumes {
			if volume.Type != "custom" || strings.Contains(volume.Name, "/") || config.Config["features.storage.volumes"] == "false" {
				continue
			}
			snapshots, err := scoped.GetStoragePoolVolumeSnapshotNames(pool.Name, "custom", volume.Name)
			if err != nil {
				return nil, normalizeError("read teardown volume snapshots", err)
			}
			sort.Strings(snapshots)
			if snapshots == nil {
				snapshots = []string{}
			}
			if err := add("volume", volume.Name, pool.Name, map[string]any{"name": volume.Name, "type": volume.Type, "content_type": volume.ContentType, "config": volume.Config, "description": volume.Description, "snapshots": snapshots}); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		return left.Kind+"/"+left.Pool+"/"+left.Name < right.Kind+"/"+right.Pool+"/"+right.Name
	})
	if len(result) > 256 {
		return nil, fmt.Errorf("teardown inventory exceeds 256 resources")
	}
	return result, nil
}

func (client *Client) TeardownSharedInventory(ctx context.Context, poolName, networkName string) ([]ports.TeardownResource, error) {
	return client.sharedTeardownInventory(ctx, poolName, networkName, false)
}

// InstallationSharedInventory adds exact native backing paths and consumers
// only for aggregate full removal. Ordinary selected-yard semantics are intact.
func (client *Client) InstallationSharedInventory(ctx context.Context, poolName, networkName string) ([]ports.TeardownResource, error) {
	return client.sharedTeardownInventory(ctx, poolName, networkName, true)
}

func (client *Client) sharedTeardownInventory(ctx context.Context, poolName, networkName string, installation bool) ([]ports.TeardownResource, error) {
	server, err := client.reconcileServer(ctx)
	if err != nil {
		if client.LocalInstallationAbsent() {
			return []ports.TeardownResource{}, nil
		}
		return nil, err
	}
	resources := []ports.TeardownResource{}
	pools := []string{poolName}
	networks := []string{networkName}
	if installation && poolName == "" && networkName == "" {
		pools, err = server.GetStoragePoolNames()
		if err != nil {
			return nil, err
		}
		networks, err = server.GetNetworkNames()
		if err != nil {
			return nil, err
		}
	}
	owner := func(values map[string]string) (*ports.InstallationResourceOwner, error) {
		if values["user.subyard.data_home"] == "" {
			return nil, nil
		}
		result := &ports.InstallationResourceOwner{UID: values["user.subyard.operator_uid"], DataHome: values["user.subyard.data_home"], ConfigHome: values["user.subyard.config_home"]}
		raw := values["user.subyard.context"]
		if raw != "" {
			result.Context = &domain.Context{}
			if len(raw) > 16384 || json.Unmarshal([]byte(raw), result.Context) != nil || result.Context.Validate() != nil || result.Context.AccessKind != domain.AccessLocal || result.Context.Paths.DataHome != result.DataHome || result.Context.Paths.ConfigHome != result.ConfigHome {
				return nil, fmt.Errorf("invalid shared installation context")
			}
		}
		return result, nil
	}
	add := func(kind, name, path string, usedBy []string, values map[string]string, value any) error {
		var ownership *ports.InstallationResourceOwner
		var err error
		if installation {
			ownership, err = owner(values)
		}
		if !installation {
			path = ""
		}
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(value)
		hash := sha256.Sum256(payload)
		resources = append(resources, ports.TeardownResource{Kind: kind, Name: name, Path: path, UsedBy: usedBy, Binding: hex.EncodeToString(hash[:]), Owner: ownership})
		return nil
	}
	for _, name := range pools {
		if name == "" {
			continue
		}
		pool, _, err := server.GetStoragePool(name)
		if api.StatusErrorCheck(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := add("pool", name, pool.Config["source"], pool.UsedBy, pool.Config, map[string]any{"name": pool.Name, "driver": pool.Driver, "config": pool.Config, "description": pool.Description}); err != nil {
			return nil, err
		}
	}
	for _, name := range networks {
		if name == "" {
			continue
		}
		network, _, err := server.GetNetwork(name)
		if api.StatusErrorCheck(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := add("network", name, "", network.UsedBy, network.Config, map[string]any{"name": network.Name, "type": network.Type, "managed": network.Managed, "config": network.Config, "description": network.Description}); err != nil {
			return nil, err
		}
	}
	if len(resources) == 0 {
		return resources, nil
	}
	profile, _, err := server.GetProfile("default")
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"name": profile.Name, "config": profile.Config, "devices": profile.Devices, "description": profile.Description})
	digest := sha256.Sum256(payload)
	bindings := []string{}
	for _, mask := range []int{1, 2, 3} {
		devices := cloneDevices(profile.Devices)
		for index, name := range []string{"root", "eth0"} {
			if mask&(1<<index) != 0 {
				delete(devices, name)
			}
		}
		payload, _ := json.Marshal(map[string]any{"name": profile.Name, "config": profile.Config, "devices": devices, "description": profile.Description})
		hash := sha256.Sum256(payload)
		bindings = append(bindings, hex.EncodeToString(hash[:]))
	}
	resources = append(resources, ports.TeardownResource{Kind: "host-profile", Name: "default", Binding: hex.EncodeToString(digest[:]), UsedBy: profile.UsedBy, Bindings: bindings})
	return resources, nil
}
