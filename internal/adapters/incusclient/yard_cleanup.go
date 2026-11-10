package incusclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

const cleanupDevice = "cleanup-backup"
const cleanupMount = "/mnt/subyard-cleanup-backup"

type cleanupNative struct {
	Instance  api.Instance
	Volume    api.StorageVolume
	Binds     api.DevicesMap
	HostRoots map[string][2]uint64
}

func cleanupHostRoots(binds api.DevicesMap) (map[string][2]uint64, error) {
	roots := map[string][2]uint64{}
	for _, device := range binds {
		path := device["source"]
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errors.New("unsafe host bind source")
		}
		for parent := path; parent != "/"; parent = filepath.Dir(parent) {
			info, err := os.Lstat(parent)
			if err != nil || !info.IsDir() {
				return nil, errors.New("host bind source has a missing or linked ancestor")
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		st := info.Sys().(*syscall.Stat_t)
		roots[path] = [2]uint64{uint64(st.Dev), st.Ino}
	}
	return roots, nil
}

func (client *Client) PrepareYardBackup(ctx context.Context, project, name, pool, volume, token string, hostBinds map[string]string) (ports.YardCleanupBackup, error) {
	p := ports.YardCleanupBackup{Project: project, Instance: name, Pool: pool, Volume: volume, Token: token, BackupVolume: "subyard-cleanup-" + token, RestoreVolume: "cleanup-restore-" + token}
	if project == "default" || !domain.SafeID(token) || len(token) != 32 {
		return p, errors.New("hard cleanup requires an isolated owned yard project and backup token")
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return p, err
	}
	projectInfo, _, err := server.GetProject(project)
	if err != nil {
		return p, err
	}
	if projectInfo.Config["features.storage.volumes"] == "false" {
		return p, errors.New("hard cleanup requires a project-isolated volume namespace")
	}
	scoped := server.UseProject(project)
	instance, _, err := scoped.GetInstance(name)
	if err != nil {
		return p, err
	}
	srv := instance.Devices["srv"]
	if srv["type"] != "disk" || srv["pool"] != pool || srv["source"] != volume || !reflect.DeepEqual(srv, instance.ExpandedDevices["srv"]) || (srv["path"] != "/srv" && !(instance.Type == "virtual-machine" && srv["io.bus"] == "virtio-scsi" && srv["path"] == "")) {
		return p, errors.New("hard cleanup requires the exact independent local srv volume")
	}
	source, _, err := scoped.GetStoragePoolVolume(pool, "custom", volume)
	if err != nil {
		return p, err
	}
	binds := api.DevicesMap{}
	for key, path := range hostBinds {
		device := instance.Devices[key]
		if device["type"] != "disk" || device["pool"] != "" || device["source"] != path || !reflect.DeepEqual(device, instance.ExpandedDevices[key]) {
			return p, errors.New("registered bind differs from its local native device")
		}
		binds[key] = maps.Clone(device)
	}
	roots, err := cleanupHostRoots(binds)
	if err != nil {
		return p, err
	}
	p.OriginalUUID = instance.Config["volatile.uuid"]
	p.DesiredPower = instance.Config["user.subyard.desired_power"]
	if p.DesiredPower != "running" && p.DesiredPower != "stopped" {
		p.DesiredPower = strings.ToLower(instance.Status)
	}
	if p.OriginalUUID == "" || (p.DesiredPower != "running" && p.DesiredPower != "stopped") {
		return p, errors.New("yard native identity or power intent is unavailable")
	}
	p.ContentType = source.ContentType
	p.Native, _ = json.Marshal(cleanupNative{Instance: *instance, Volume: *source, Binds: binds, HostRoots: roots})
	return p, client.CheckYardBackupSource(ctx, p)
}

func cleanupConfig(config map[string]string) map[string]string {
	result := maps.Clone(config)
	for key := range result {
		if strings.HasPrefix(key, "volatile.") {
			delete(result, key)
		}
	}
	return result
}

func (client *Client) CheckYardBackupSource(ctx context.Context, p ports.YardCleanupBackup) error {
	var saved cleanupNative
	if err := json.Unmarshal(p.Native, &saved); err != nil {
		return err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	projectInfo, _, err := server.GetProject(p.Project)
	if err != nil {
		return err
	}
	if projectInfo.Config["features.storage.volumes"] == "false" {
		return domain.ErrPlanStale
	}
	scoped := server.UseProject(p.Project)
	current, _, err := scoped.GetInstance(p.Instance)
	if err != nil {
		return err
	}
	devices := maps.Clone(current.Devices)
	delete(devices, cleanupDevice)
	expanded := maps.Clone(current.ExpandedDevices)
	delete(expanded, cleanupDevice)
	if current.Config["volatile.uuid"] != p.OriginalUUID || !reflect.DeepEqual(cleanupConfig(current.Config), cleanupConfig(saved.Instance.Config)) || !reflect.DeepEqual(devices, saved.Instance.Devices) || !reflect.DeepEqual(expanded, saved.Instance.ExpandedDevices) || !reflect.DeepEqual(current.Profiles, saved.Instance.Profiles) || current.Type != saved.Instance.Type {
		return domain.ErrPlanStale
	}
	roots, err := cleanupHostRoots(saved.Binds)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(roots, saved.HostRoots) {
		return domain.ErrPlanStale
	}
	volume, _, err := scoped.GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(volume.Config, saved.Volume.Config) || volume.ContentType != saved.Volume.ContentType || len(volume.UsedBy) != 1 {
		return domain.ErrPlanStale
	}
	use, err := url.Parse(volume.UsedBy[0])
	if err != nil || use.Path != "/1.0/instances/"+p.Instance || use.Query().Get("project") != p.Project {
		return domain.ErrPlanStale
	}
	return nil
}

func backupOwned(volume *api.StorageVolume, p ports.YardCleanupBackup) bool {
	return volume != nil && volume.Type == "custom" && volume.ContentType == p.ContentType && volume.Config["user.subyard.cleanup"] == p.Token && volume.Config["user.subyard.cleanup.source"] == p.OriginalUUID
}

func cleanupVolumeIdle(server incus.InstanceServer, pool, name string) error {
	operations, err := server.GetOperations()
	if err != nil {
		return err
	}
	target := "/1.0/storage-pools/" + url.PathEscape(pool) + "/volumes/custom/" + url.PathEscape(name)
	for _, operation := range operations {
		for _, resources := range operation.Resources {
			for _, resource := range resources {
				u, _ := url.Parse(resource)
				if u != nil && u.Path == target && !operation.StatusCode.IsFinal() {
					return errors.New("native cleanup copy is still running; retry after it finishes")
				}
			}
		}
	}
	return nil
}

// Completion is persisted only after native Wait succeeds. An interrupted copy
// is safely recreated while the source still exists; no partial copy authorizes deletion.
func copyCleanupVolume(destination, source incus.InstanceServer, pool string, volume api.StorageVolume, name string, p ports.YardCleanupBackup) error {
	current, _, err := destination.GetStoragePoolVolume(pool, "custom", name)
	if err == nil {
		if !backupOwned(current, p) {
			return domain.ErrPlanStale
		}
		if current.Config["user.subyard.cleanup.complete"] == "true" {
			return nil
		}
		if len(current.UsedBy) != 0 {
			return errors.New("incomplete cleanup copy is attached")
		}
		if err := cleanupVolumeIdle(destination, pool, name); err != nil {
			return err
		}
		if err := destination.DeleteStoragePoolVolume(pool, "custom", name); err != nil {
			return err
		}
	} else if !api.StatusErrorCheck(err, http.StatusNotFound) {
		return err
	}
	volume.Config = maps.Clone(volume.Config)
	if volume.Config == nil {
		volume.Config = map[string]string{}
	}
	volume.Config["user.subyard.cleanup"] = p.Token
	volume.Config["user.subyard.cleanup.source"] = p.OriginalUUID
	volume.Config["user.subyard.cleanup.complete"] = "false"
	operation, err := destination.CopyStoragePoolVolume(pool, source, pool, volume, &incus.StoragePoolVolumeCopyArgs{Name: name, VolumeOnly: true})
	if err != nil {
		return err
	}
	if err := operation.Wait(); err != nil {
		return err
	}
	current, etag, err := destination.GetStoragePoolVolume(pool, "custom", name)
	if err != nil {
		return err
	}
	if !backupOwned(current, p) || len(current.UsedBy) != 0 {
		return domain.ErrPlanStale
	}
	current.Config["user.subyard.cleanup.complete"] = "true"
	return destination.UpdateStoragePoolVolume(pool, "custom", name, current.Writable(), etag)
}

func (client *Client) CopyYardBackup(ctx context.Context, p ports.YardCleanupBackup) error {
	if err := client.CheckYardBackupSource(ctx, p); err != nil {
		return err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	scoped := server.UseProject(p.Project)
	instance, _, err := scoped.GetInstance(p.Instance)
	if err != nil {
		return err
	}
	if !strings.EqualFold(instance.Status, "stopped") {
		return errors.New("stop selected yard before backup")
	}
	volume, _, err := scoped.GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if err != nil {
		return err
	}
	if err := copyCleanupVolume(server.UseProject("default"), scoped, p.Pool, *volume, p.BackupVolume, p); err != nil {
		return err
	}
	return client.CheckYardBackup(ctx, p)
}

func (client *Client) CheckYardBackup(ctx context.Context, p ports.YardCleanupBackup) error {
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	volume, _, err := server.UseProject("default").GetStoragePoolVolume(p.Pool, "custom", p.BackupVolume)
	if err != nil {
		return err
	}
	if !backupOwned(volume, p) || volume.Config["user.subyard.cleanup.complete"] != "true" || len(volume.UsedBy) != 0 {
		return errors.New("independent cleanup backup is incomplete or has unexpected consumers")
	}
	return nil
}

func (client *Client) AttachYardBackup(ctx context.Context, p ports.YardCleanupBackup, uuid string) error {
	if err := client.CheckYardBackup(ctx, p); err != nil {
		return err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	source := server.UseProject("default")
	scoped := server.UseProject(p.Project)
	volume, _, err := source.GetStoragePoolVolume(p.Pool, "custom", p.BackupVolume)
	if err != nil {
		return err
	}
	if err := copyCleanupVolume(scoped, source, p.Pool, *volume, p.RestoreVolume, p); err != nil {
		return err
	}
	instance, etag, err := scoped.GetInstance(p.Instance)
	if err != nil {
		return err
	}
	if instance.Config["volatile.uuid"] != uuid || !strings.EqualFold(instance.Status, "running") {
		return domain.ErrPlanStale
	}
	device := map[string]string{"type": "disk", "pool": p.Pool, "source": p.RestoreVolume, "readonly": "true"}
	if p.ContentType == "block" {
		device["io.bus"] = "virtio-scsi"
	} else {
		device["path"] = cleanupMount
	}
	if existing := instance.Devices[cleanupDevice]; existing != nil {
		if reflect.DeepEqual(existing, device) {
			return nil
		}
		return domain.ErrPlanStale
	}
	instance.Devices[cleanupDevice] = device
	op, err := scoped.UpdateInstance(p.Instance, instance.Writable(), etag)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (client *Client) DetachYardBackup(ctx context.Context, p ports.YardCleanupBackup, uuid string) error {
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	scoped := server.UseProject(p.Project)
	instance, etag, err := scoped.GetInstance(p.Instance)
	if err != nil {
		return err
	}
	if instance.Config["volatile.uuid"] != uuid {
		return domain.ErrPlanStale
	}
	if device := instance.Devices[cleanupDevice]; device != nil {
		if device["source"] != p.RestoreVolume || device["pool"] != p.Pool || device["readonly"] != "true" {
			return domain.ErrPlanStale
		}
		delete(instance.Devices, cleanupDevice)
		op, err := scoped.UpdateInstance(p.Instance, instance.Writable(), etag)
		if err != nil {
			return err
		}
		if err := op.Wait(); err != nil {
			return err
		}
	}
	volume, _, err := scoped.GetStoragePoolVolume(p.Pool, "custom", p.RestoreVolume)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !backupOwned(volume, p) || len(volume.UsedBy) != 0 {
		return domain.ErrPlanStale
	}
	if err := cleanupVolumeIdle(scoped, p.Pool, p.RestoreVolume); err != nil {
		return err
	}
	return scoped.DeleteStoragePoolVolume(p.Pool, "custom", p.RestoreVolume)
}

func (client *Client) RestoreYardBinds(ctx context.Context, p ports.YardCleanupBackup, uuid string) error {
	var saved cleanupNative
	if err := json.Unmarshal(p.Native, &saved); err != nil {
		return err
	}
	roots, err := cleanupHostRoots(saved.Binds)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(roots, saved.HostRoots) {
		return domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	scoped := server.UseProject(p.Project)
	instance, etag, err := scoped.GetInstance(p.Instance)
	if err != nil {
		return err
	}
	if instance.Config["volatile.uuid"] != uuid {
		return domain.ErrPlanStale
	}
	for key, device := range saved.Binds {
		if current := instance.Devices[key]; current != nil && !reflect.DeepEqual(current, device) {
			return fmt.Errorf("%w: recreated host bind differs", domain.ErrPlanStale)
		}
		instance.Devices[key] = maps.Clone(device)
	}
	instance.Config["user.subyard.cleanup"] = p.Token
	instance.Config["user.subyard.desired_power"] = p.DesiredPower
	op, err := scoped.UpdateInstance(p.Instance, instance.Writable(), etag)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (client *Client) ReleaseYardBackup(ctx context.Context, p ports.YardCleanupBackup) error {
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	scoped := server.UseProject("default")
	volume, _, err := scoped.GetStoragePoolVolume(p.Pool, "custom", p.BackupVolume)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !backupOwned(volume, p) || len(volume.UsedBy) != 0 {
		return domain.ErrPlanStale
	}
	if err := cleanupVolumeIdle(scoped, p.Pool, p.BackupVolume); err != nil {
		return err
	}
	return scoped.DeleteStoragePoolVolume(p.Pool, "custom", p.BackupVolume)
}
