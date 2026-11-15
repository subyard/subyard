package incusclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/lxc/incus/v7/shared/api"
)

// ownerCreateServerFixture serves the ordinary project create boundary used by
// the release transition image namespace and an instance ETag boundary.
type ownerCreateServerFixture struct {
	mu               sync.Mutex
	mode             string
	posts, updates   int
	project          *api.Project
	instance         *api.Instance
	instanceETag     string
	afterInstanceGet func()
}

func (f *ownerCreateServerFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(metadata any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status": "Success", "status_code": 200, "metadata": metadata})
	}
	if r.URL.Path == "/1.0" {
		reply(map[string]any{"api_extensions": []string{"projects", "storage"}})
		return
	}
	if strings.HasPrefix(f.mode, "instance-") {
		if strings.HasPrefix(r.URL.Path, "/1.0/instances/") && r.Method == http.MethodPut {
			if r.Header.Get("If-Match") != f.instanceETag {
				w.WriteHeader(412)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 412, "error": "guard differs"})
				return
			}
			var put api.InstancePut
			if json.NewDecoder(r.Body).Decode(&put) != nil {
				w.WriteHeader(400)
				return
			}
			f.instance.InstancePut = put
			f.updates++
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status_code": 100, "operation": "/1.0/operations/owner-create-operation", "metadata": api.Operation{ID: "owner-create-operation", StatusCode: api.Success, Resources: map[string][]string{"instances": {"/1.0/instances/yard?project=original-project"}}}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/1.0/instances/") {
			if f.instance == nil {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 404, "error": "absent"})
				return
			}
			if f.instanceETag == "" {
				f.instanceETag = `"created-instance"`
			}
			w.Header().Set("ETag", f.instanceETag)
			reply(f.instance)
			if f.afterInstanceGet != nil {
				f.afterInstanceGet()
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/1.0/operations/") {
			reply(api.Operation{ID: "owner-create-operation", StatusCode: api.Success, Resources: map[string][]string{"instances": {"/1.0/instances/yard?project=original-project"}}})
			return
		}
	}
	if r.Method == http.MethodPost && r.URL.Path == "/1.0/projects" {
		f.posts++
		var request api.ProjectsPost
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Config["features.images"] != "false" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.mode == "ordinary-conflict" {
			f.project = &api.Project{Name: request.Name, ProjectPut: api.ProjectPut{Config: map[string]string{"foreign": "keep"}}}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 409, "error": "project already exists"})
			return
		}
		f.project = &api.Project{Name: request.Name, ProjectPut: request.ProjectPut}
		reply(nil)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/1.0/projects/") {
		if f.project == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 404, "error": "fixture absent"})
			return
		}
		w.Header().Set("ETag", `"original-project"`)
		reply(f.project)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func ownerCreateClientFixture(t *testing.T, mode string) (*Client, *ownerCreateServerFixture) {
	t.Helper()
	socket := filepath.Join(testkit.TempDir(t), "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ownerCreateServerFixture{mode: mode}
	server := &http.Server{Handler: fixture}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	return New(socket, "projects"), fixture
}

func renderOwnerProjectConfig(t *testing.T, value domain.Context, environment []string) ([]byte, string) {
	t.Helper()
	contextValue, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile("../../../scripts/lib/project-config.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-peu", "-s")
	command.Stdin = strings.NewReader(string(helper) + "\nsubyard_project_config_render\n")
	command.Env = append([]string{"PATH=/usr/bin:/bin", "SUBYARD_INSTALLATION_CONTEXT=" + string(contextValue)}, environment...)
	output, err := command.Output()
	if err != nil {
		t.Fatal("actual project renderer failed", err)
	}
	return output, string(contextValue)
}

// Exercise the original format producer at its synthetic CLI/guest boundary.
// The guest mock returns an observed filesystem UUID; the original function
// must publish both marker fields and reject a changed already-ready identity.
func ownerBlockFormatCommand(t *testing.T, marker, recorded, observed string) ([]byte, error) {
	t.Helper()
	helper, err := os.ReadFile("../../../scripts/lib-vm-storage.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "--noprofile", "--norc", "-peu", "-s")
	command.Env = append(os.Environ(), "SRV_POOL=default", "SRV_VOLUME=yard-srv", "INCUS_PROJECT=original-project", "YARD_INSTANCE_NAME=yard", "SRV_VOLUME_SIZE=4GiB", "FORMAT_MARKER="+marker, "FORMAT_RECORDED="+recorded, "FORMAT_OBSERVED="+observed)
	command.Stdin = io.MultiReader(bytes.NewReader(helper), strings.NewReader(`
incus_wait_yard_agent() { :; }
die() { printf '%s\n' "$1" >&2; exit 1; }
incus() {
 case "$1 $2 $3" in
  'storage volume get') case "$6" in user.subyard.format) printf '%s\n' "$FORMAT_MARKER";; user.subyard.uuid) printf '%s\n' "$FORMAT_RECORDED";; *) return 2;; esac;;
  'storage volume set') printf '%s\n' "$6" "$7" >&3;;
  *) [ "$1" = exec ] || return 2; cat >/dev/null; printf '%s\n' "$FORMAT_OBSERVED";;
 esac
}
exec 3>&1
vm_storage_mount
`))
	return command.Output()
}
func ownerActualBlockFormat(t *testing.T, marker, recorded, observed string) map[string]string {
	t.Helper()
	raw, err := ownerBlockFormatCommand(t, marker, recorded, observed)
	if err != nil {
		t.Fatal("original format producer failed", err)
	}
	fields := map[string]string{}
	for _, row := range strings.Fields(string(raw)) {
		key, value, ok := strings.Cut(row, "=")
		if !ok {
			t.Fatal("unexpected format output")
		}
		fields[key] = value
	}
	if len(fields) != 2 || fields["user.subyard.format"] != "ready" || fields["user.subyard.uuid"] != observed {
		t.Fatal("original format did not publish both fields")
	}
	return fields
}
func TestOwnerBlockFormatPreservesOriginallyKnownFilesystemUUID(t *testing.T) {
	const uuid = "11111111-2222-3333-4444-555555555555"
	ownerActualBlockFormat(t, "pending", "", uuid)
	if raw, err := ownerBlockFormatCommand(t, "ready", uuid, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err == nil || len(raw) != 0 {
		t.Fatal("original producer accepted changed ready filesystem UUID")
	}
	ownerActualBlockFormat(t, "ready", uuid, uuid)
}

func TestOwnerBlockReadinessComparesMarkerWithMountedFilesystem(t *testing.T) {
	const uuid = "11111111-2222-3333-4444-555555555555"
	helper, err := os.ReadFile("../../../scripts/lib-vm-storage.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, actual := range []string{uuid, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"} {
		command := exec.Command("bash", "--noprofile", "--norc", "-peu", "-s")
		command.Env = append(os.Environ(), "SRV_VOLUME_TYPE=block", "SRV_VOLUME_SIZE=4GiB", "SRV_POOL=default", "SRV_VOLUME=yard-srv", "INCUS_PROJECT=original-project", "YARD_INSTANCE_NAME=yard", "ROOT_DISK_SIZE=", "FILESYSTEM_UUID="+actual)
		// Isolate the filesystem identity predicate; the device boundary has its
		// own original checks, while this fixture supplies their converged result.
		command.Stdin = io.MultiReader(bytes.NewReader(helper), strings.NewReader(`
vm_storage_check_device_boundary() { :; }
power_state() { printf 'RUNNING\n'; }
device_get() { case "$2" in type) printf disk;; pool) printf default;; source) printf yard-srv;; path) :;; io.bus) printf virtio-scsi;; *) return 2;; esac; }
incus() { case "$1" in query) printf '%s\n' '{"content_type":"block","config":{"size":"4GiB","user.subyard.storage":"v1","user.subyard.format":"ready","user.subyard.uuid":"11111111-2222-3333-4444-555555555555"}}';; exec) printf '%s\n' "$FILESYSTEM_UUID";; *) return 2;; esac; }
vm_storage_check
`))
		_, err := command.Output()
		if (err == nil) != (actual == uuid) {
			t.Fatal("original readiness lost mounted UUID predicate", err)
		}
	}
}
