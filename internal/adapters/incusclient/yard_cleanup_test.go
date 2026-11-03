package incusclient

import (
	"errors"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCleanupHostBindIdentity(t *testing.T) {
	root := testkit.TempDir(t)
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	instance := api.Instance{}
	instance.ExpandedDevices = map[string]map[string]string{"core-nondirectory-mask": {"type": "disk", "source": "/dev/null", "path": "/sys/module/apparmor/parameters/enabled", "readonly": "true"}, "ws-id": {"type": "disk", "source": source, "path": "/srv/workspaces/id/src"}}
	before, err := cleanupHostRoots(api.DevicesMap{"ws-id": instance.ExpandedDevices["ws-id"]})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(source, "private"), []byte("host edit"), 0600)
	after, err := cleanupHostRoots(api.DevicesMap{"ws-id": instance.ExpandedDevices["ws-id"]})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("content edit changed root binding: %v", err)
	}
	if err := os.Rename(source, source+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	after, err = cleanupHostRoots(api.DevicesMap{"ws-id": instance.ExpandedDevices["ws-id"]})
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatalf("root replacement was not detected: %v", err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source+"-old", source); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupHostRoots(api.DevicesMap{"ws-id": instance.ExpandedDevices["ws-id"]}); err == nil {
		t.Fatal("linked source was accepted")
	}
}

type cleanupCopyServer struct {
	incus.InstanceServer
	volume *api.StorageVolume
	copied int
	fail   bool
	waited bool
}
type cleanupCopyOperation struct {
	incus.RemoteOperation
	server *cleanupCopyServer
}

func (op cleanupCopyOperation) Wait() error {
	op.server.waited = true
	if op.server.fail {
		return errors.New("native copy failed")
	}
	return nil
}
func (s *cleanupCopyServer) GetStoragePoolVolume(_, _, _ string) (*api.StorageVolume, string, error) {
	if s.volume == nil {
		return nil, "", api.StatusErrorf(404, "absent")
	}
	value := *s.volume
	value.Config = maps.Clone(value.Config)
	return &value, "etag", nil
}
func (s *cleanupCopyServer) CopyStoragePoolVolume(_ string, _ incus.InstanceServer, _ string, v api.StorageVolume, args *incus.StoragePoolVolumeCopyArgs) (incus.RemoteOperation, error) {
	s.copied++
	v.Name = args.Name
	s.volume = &v
	return cleanupCopyOperation{server: s}, nil
}
func (s *cleanupCopyServer) UpdateStoragePoolVolume(_, _, _ string, put api.StorageVolumePut, _ string) error {
	if !s.waited {
		return errors.New("completion published before native wait")
	}
	s.volume.Config = maps.Clone(put.Config)
	return nil
}
func (s *cleanupCopyServer) DeleteStoragePoolVolume(_, _, _ string) error { s.volume = nil; return nil }
func (s *cleanupCopyServer) GetOperations() ([]api.Operation, error)      { return nil, nil }

func TestCleanupNativeCopyFailureCannotAuthorizeDeletionAndRetryCompletes(t *testing.T) {
	p := ports.YardCleanupBackup{Token: "token", OriginalUUID: "original", ContentType: "block"}
	original := api.StorageVolume{Name: "srv", Type: "custom", ContentType: "block", StorageVolumePut: api.StorageVolumePut{Config: map[string]string{"user.subyard.uuid": "filesystem"}}}
	target := &cleanupCopyServer{fail: true}
	if err := copyCleanupVolume(target, nil, "pool", original, "backup", p); err == nil {
		t.Fatal("native failure lost")
	}
	if target.volume.Config["user.subyard.cleanup.complete"] == "true" {
		t.Fatal("failed copy marked complete")
	}
	if original.Config["user.subyard.cleanup"] != "" {
		t.Fatal("source metadata mutated")
	}
	target.fail = false
	target.waited = false
	if err := copyCleanupVolume(target, nil, "pool", original, "backup", p); err != nil {
		t.Fatal(err)
	}
	if target.copied != 2 || !target.waited || target.volume.Config["user.subyard.cleanup.complete"] != "true" {
		t.Fatal("retry did not establish independent complete copy")
	}
	target.volume.Config["user.subyard.cleanup"] = "foreign"
	if err := copyCleanupVolume(target, nil, "pool", original, "backup", p); err == nil {
		t.Fatal("foreign backup adopted")
	}
}
