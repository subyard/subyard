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

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/lxc/incus/v6/shared/api"
)

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
	server, err := client.reconcileServer(ctx)
	if err != nil {
		if client.LocalInstallationAbsent() {
			return []ports.TeardownResource{}, nil
		}
		return nil, err
	}
	resources := []ports.TeardownResource{}
	add := func(kind, name string, value any) {
		payload, _ := json.Marshal(value)
		digest := sha256.Sum256(payload)
		resources = append(resources, ports.TeardownResource{Kind: kind, Name: name, Binding: hex.EncodeToString(digest[:])})
	}
	if networkName != "" {
		network, _, err := server.GetNetwork(networkName)
		if err != nil && !api.StatusErrorCheck(err, http.StatusNotFound) {
			return nil, normalizeError("read teardown shared network", err)
		}
		if err == nil {
			add("network", networkName, map[string]any{"name": network.Name, "type": network.Type, "managed": network.Managed, "config": network.Config, "description": network.Description})
		}
	}
	if poolName != "" {
		pool, _, err := server.GetStoragePool(poolName)
		if err != nil && !api.StatusErrorCheck(err, http.StatusNotFound) {
			return nil, normalizeError("read teardown shared pool", err)
		}
		if err == nil {
			add("pool", poolName, map[string]any{"name": pool.Name, "driver": pool.Driver, "config": pool.Config, "description": pool.Description})
		}
	}
	return resources, nil
}
