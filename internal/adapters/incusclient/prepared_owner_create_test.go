package incusclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

type ownerCreateObserverFixture struct {
	custodyObserver
	mu                   sync.Mutex
	before, completed    int
	commands, operations int
	records              map[ports.NativeOwnerCreateRef]ports.NativeOwnerCreated
	intent               ports.NativeOwnerCreateIntent
	created              *ports.NativeOwnerCreated
	failResult           bool
}

func (o *ownerCreateObserverFixture) BeforeOwnerCreate(v ports.NativeOwnerCreateIntent) (ports.NativeOwnerCreateRef, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.before++
	o.intent = v
	index := 0
	if v.Domain == "command" {
		o.commands++
		index = o.commands
	} else {
		o.operations++
		index = o.operations
	}
	return ports.NativeOwnerCreateRef{Producer: strings.Repeat("c", 64), Domain: v.Domain, Index: uint64(index)}, nil
}
func (o *ownerCreateObserverFixture) CommandTerminal(index uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if index != uint64(o.commands) {
		return errors.New("wrong original reservation")
	}
	o.completed++
	return nil
}
func (o *ownerCreateObserverFixture) RecordOwnerCreated(v ports.NativeOwnerCreated) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	closed := v.Intent.Domain != "operation"
	for _, op := range o.terminal {
		if op.ID == v.OperationID && op.Kind == "owner-create-instance" && op.Status == 200 {
			closed = true
		}
	}
	if o.failResult || v.Intent != o.intent || v.Intent.Domain == "command" && o.completed != o.commands || !closed {
		return errors.New("fixed result failure")
	}
	o.created = &v
	if o.records == nil {
		o.records = map[ports.NativeOwnerCreateRef]ports.NativeOwnerCreated{}
	}
	o.records[v.Ref] = v
	return nil
}
func (o *ownerCreateObserverFixture) ReadOwnerCreated(ref ports.NativeOwnerCreateRef) (ports.NativeOwnerCreated, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.records[ref]
	if !ok {
		return v, errors.New("original result missing")
	}
	return v, nil
}

func (o *ownerCreateObserverFixture) Before(v ports.NativeOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ownerImageSecretKind(v.Kind) {
		o.operations++
	}
	return nil
}
func (o *ownerCreateObserverFixture) Accepted(v ports.NativeOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.accepted = append(o.accepted, v)
	return nil
}
func (o *ownerCreateObserverFixture) Terminal(v ports.NativeOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.terminal = append(o.terminal, v)
	return nil
}
func (o *ownerCreateObserverFixture) Uncertain() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.uncertain = true
}

type ownerCreateServerFixture struct {
	mu                   sync.Mutex
	mode                 string
	posts                int
	project              *api.Project
	instance             *api.Instance
	volume               *api.StorageVolume
	volumePosts, updates int
	instanceETag         string
	before               func() bool
	oci                  bool
	onInstancePost       func(api.InstancesPost)
	afterInstanceGet     func()
	afterVolumeGet       func()
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
	if strings.HasPrefix(f.mode, "instance-") || strings.HasPrefix(f.mode, "volume-") {
		fingerprint := strings.Repeat("a", 64)
		imageKind := "container"
		if strings.Contains(f.mode, "block") {
			imageKind = "virtual-machine"
		}
		if r.URL.Path == "/1.0/images" && r.Method == http.MethodGet {
			reply([]api.Image{{Fingerprint: fingerprint, Type: imageKind, Aliases: []api.ImageAlias{{Name: "debian/13"}}}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/1.0/images/aliases/") {
			reply(api.ImageAliasesEntry{Name: "debian/13", Type: imageKind, ImageAliasesEntryPut: api.ImageAliasesEntryPut{Target: fingerprint}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/1.0/images/") {
			reply(api.Image{Fingerprint: fingerprint, Type: imageKind})
			return
		}
		if r.URL.Path == "/1.0/instances" && r.Method == http.MethodPost {
			f.posts++
			var request api.InstancesPost
			decodeErr := json.NewDecoder(r.Body).Decode(&request)
			validSource := request.Source.Alias == "" && request.Source.Fingerprint == fingerprint
			if f.oci {
				validSource = request.Source.Protocol == "oci" && request.Source.Mode == "pull" && request.Source.Alias == "library/alpine:3.22" && request.Source.Fingerprint == "" && request.Source.Secret == ""
			}
			if decodeErr != nil || !f.before() || len(request.Config[ownerCreateTokenKey]) != 32 || !validSource {
				w.WriteHeader(400)
				return
			}
			if f.onInstancePost != nil {
				f.onInstancePost(request)
			}
			if f.mode == "instance-private-error" {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 400, "error": request.Source.Secret})
				return
			}
			f.instance = &api.Instance{Name: request.Name, Type: string(request.Type), InstancePut: request.InstancePut}
			f.instance.Config["volatile.uuid"] = "original-instance-uuid"
			f.instance.Config["volatile.base_image"] = request.Source.Fingerprint
			if f.oci {
				f.instance.Config["volatile.base_image"] = fingerprint
			}
			f.instance.Profiles = []string{"default"}
			f.instance.ExpandedConfig = maps.Clone(f.instance.Config)
			f.instance.ExpandedDevices = map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}, "eth0": {"type": "nic", "nictype": "bridged", "parent": "incusbr0"}}
			if f.mode == "instance-config-diff" {
				f.instance.Config["boot.autostart"] = "true"
			}
			if f.mode == "instance-invalid-base-image" {
				f.instance.Config["volatile.base_image"] = "invalid"
			}
			if f.mode == "instance-wrong-fingerprint" {
				f.instance.Config["volatile.base_image"] = strings.Repeat("b", 64)
			}
			if f.mode == "instance-missing-uuid" {
				delete(f.instance.Config, "volatile.uuid")
			}
			if f.mode == "instance-lost-response" {
				connection, _, _ := w.(http.Hijacker).Hijack()
				_ = connection.Close()
				return
			}
			id := "owner-create-operation"
			if f.mode == "instance-missing-id" {
				id = ""
			}
			resources := map[string][]string{"instances": {"/1.0/instances/yard?project=original-project"}}
			if f.mode == "instance-wrong-resource" {
				resources["instances"] = []string{"/1.0/instances/foreign?project=original-project"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status_code": 100, "operation": "/1.0/operations/" + id, "metadata": api.Operation{ID: id, StatusCode: api.Success, Resources: resources}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/1.0/storage-pools/") {
			if r.Method == http.MethodPost {
				f.volumePosts++
				var request api.StorageVolumesPost
				if json.NewDecoder(r.Body).Decode(&request) != nil || !f.before() || len(request.Config[ownerCreateTokenKey]) != 32 {
					w.WriteHeader(400)
					return
				}
				f.volume = &api.StorageVolume{Name: request.Name, Type: request.Type, ContentType: request.ContentType, StorageVolumePut: request.StorageVolumePut}
				if f.mode == "volume-lost-response" {
					connection, _, _ := w.(http.Hijacker).Hijack()
					_ = connection.Close()
					return
				}
				if f.mode == "volume-missing-status" {
					_, _ = w.Write([]byte("{}"))
					return
				}
				reply(nil)
				return
			}
			if f.volume == nil {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 404, "error": "absent"})
				return
			}
			w.Header().Set("ETag", `"created-volume"`)
			reply(f.volume)
			if f.afterVolumeGet != nil {
				f.afterVolumeGet()
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/1.0/instances/") && r.Method == http.MethodPut {
			if r.Header.Get("If-Match") != f.instanceETag || f.mode == "volume-etag-conflict" {
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
			if device, exists := put.Devices["srv"]; exists {
				f.instance.ExpandedDevices["srv"] = maps.Clone(device)
			}
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
			status := api.Success
			message := ""
			if f.mode == "instance-private-failure" {
				status = api.Failure
				message = strings.Repeat("b", 64)
			}
			reply(api.Operation{ID: "owner-create-operation", StatusCode: status, Err: message, Resources: map[string][]string{"instances": {"/1.0/instances/yard?project=original-project"}}})
			return
		}
	}
	if r.Method == http.MethodPost && r.URL.Path == "/1.0/projects" {
		f.posts++
		var request api.ProjectsPost
		ordinary := strings.HasPrefix(f.mode, "ordinary")
		if json.NewDecoder(r.Body).Decode(&request) != nil || !ordinary && (f.before == nil || !f.before() || len(request.Config[ownerCreateTokenKey]) != 32) || request.Config["features.images"] != "false" && f.mode != "yard-project" {
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
		if f.mode == "yard-project" {
			if _, supplied := request.Config["features.images"]; supplied {
				testMessage := "yard request unexpectedly overrode image feature"
				w.WriteHeader(400)
				_, _ = w.Write([]byte(testMessage))
				return
			}
			for _, key := range []string{"features.images", "features.profiles", "features.storage.volumes", "features.storage.buckets"} {
				f.project.Config[key] = "true"
			}
		}
		switch f.mode {
		case "lost-response":
			connection, _, _ := w.(http.Hijacker).Hijack()
			_ = connection.Close()
		case "missing-status":
			_, _ = w.Write([]byte("{}"))
		case "async-response":
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status_code": 100, "operation": "/1.0/operations/foreign"})
		default:
			reply(nil)
		}
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

func ownerCreateClientFixture(t *testing.T, observer *ownerCreateObserverFixture, mode string) (*Client, *ownerCreateServerFixture) {
	t.Helper()
	socket := filepath.Join(testkit.TempDir(t), "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ownerCreateServerFixture{mode: mode, before: func() bool { observer.mu.Lock(); defer observer.mu.Unlock(); return observer.before > 0 }}
	server := &http.Server{Handler: fixture}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	return New(socket, "projects"), fixture
}

func TestOwnerImageProjectCreateConsumesOriginalSyncResponseAndInitialToken(t *testing.T) {
	for _, mode := range []string{"success", "lost-response", "missing-status", "async-response", "result-error", "already-present"} {
		t.Run(mode, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{failResult: mode == "result-error"}
			client, fixture := ownerCreateClientFixture(t, observer, mode)
			original := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, SharedImages: true, Server: ownerCreateServerSHA(t, client)}
			if mode == "already-present" {
				fixture.project = &api.Project{Name: original.Project, ProjectPut: api.ProjectPut{Config: map[string]string{ownerCreateTokenKey: "foreign"}}}
			}
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			created, err := client.ApplyPreparedOwnerImageProject(ctx, original, "current")
			fixture.mu.Lock()
			posts := fixture.posts
			fixture.mu.Unlock()
			if mode != "success" {
				if err == nil || created != nil || observer.created != nil {
					t.Fatal("unknown or foreign create gained a result")
				}
				if mode == "already-present" {
					if posts != 0 || observer.before != 0 || !errors.Is(err, domain.ErrPlanStale) {
						t.Fatal("foreign resource submitted")
					}
				} else if posts != 1 || !observer.uncertain {
					t.Fatal("missing result lost UNKNOWN")
				}
				return
			}
			if err != nil || created == nil || posts != 1 || observer.before != 1 || observer.completed != 1 || observer.uncertain {
				t.Fatal("original successful sync create lost", err)
			}
			request, requestErr := ownerImageProjectRequest(original, created.Intent.Token)
			if requestErr != nil || created.Intent.RequestDigest != ownerCreateDigest(request) {
				t.Fatal("initial request token not bound")
			}
			if err := client.CheckOwnerCreatedImageProject(ctx, original, *created); err != nil {
				t.Fatal("retained original ref refused", err)
			}
			foreign := *created
			foreign.Ref.Index++
			if client.CheckOwnerCreatedImageProject(ctx, original, foreign) == nil {
				t.Fatal("name/token substituted for original reference")
			}
			fixture.mu.Lock()
			fixture.project.Config["foreign-drift"] = "changed"
			fixture.mu.Unlock()
			if client.CheckOwnerCreatedImageProject(ctx, original, *created) == nil {
				t.Fatal("changed identity admitted")
			}
		})
	}
}

func TestOwnerImageProjectCreateRequiresNativeOriginalScope(t *testing.T) {
	observer := &ownerCreateObserverFixture{}
	client, fixture := ownerCreateClientFixture(t, observer, "success")
	original := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, SharedImages: true, Server: ownerCreateServerSHA(t, client)}
	for _, scope := range []string{"", "other", "current/legacy"} {
		if _, err := client.ApplyPreparedOwnerImageProject(ports.WithNativeOperationObserver(context.Background(), observer), original, scope); err == nil {
			t.Fatal("unprepared scope admitted")
		}
	}
	if _, err := client.ApplyPreparedOwnerImageProject(context.Background(), original, "current"); err == nil {
		t.Fatal("missing original observer admitted")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.posts != 0 {
		t.Fatal("invalid scope submitted")
	}
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

func TestOwnerImageProjectInitialPOSTUsesActualOriginalRenderer(t *testing.T) {
	for _, reporting := range []string{"0", "1"} {
		t.Run(reporting, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{}
			client, fixture := ownerCreateClientFixture(t, observer, "success")
			original := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, SharedImages: true, Server: ownerCreateServerSHA(t, client)}
			initialConfig, contextValue := renderOwnerProjectConfig(t, domain.Context{YardName: "test-yard", IncusProject: original.Project, YardInstanceName: "yard-test-yard"}, []string{"SUBYARD_HOME=/original/data", "SUBYARD_CONFIG_HOME=/original/config", "YARD_NAME=test-yard", "YARD_INSTANCE_NAME=yard-test-yard", "HOST_BASE=/original/host-\xff", "SRV_POOL=original-pool", "SRV_VOLUME=original-srv", "STORAGE_PATH=/original/storage", "YARD_KIND=vm", "VM_FREE_PAGE_REPORTING=" + reporting, "NESTED_E2E_VMS=1"})
			original.InitialConfig = initialConfig
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			created, err := client.ApplyPreparedOwnerImageProject(ctx, original, "current")
			if err != nil || created == nil {
				t.Fatal("original initial project config refused", err)
			}
			fixture.mu.Lock()
			config := fixture.project.Config
			valid := config["user.subyard.installation"] == "1" && config["user.subyard.data_home"] == "/original/data" && config["user.subyard.context"] == contextValue && config["restricted.containers.interception"] == "allow" && (config["restricted.virtual-machines.lowlevel"] == "allow") == (reporting == "1") && config["features.images"] == "false" && len(config[ownerCreateTokenKey]) == 32
			fixture.mu.Unlock()
			if !valid {
				t.Fatal("initial actual POST omitted original ownership or policy")
			}
			if err := client.CheckOwnerCreatedImageProject(ctx, original, *created); err != nil {
				t.Fatal("original full-config ref lost", err)
			}
		})
	}
}

func TestOwnerYardProjectCreatePreservesOriginal02AndDefaultImages(t *testing.T) {
	observer := &ownerCreateObserverFixture{}
	client, fixture := ownerCreateClientFixture(t, observer, "yard-project")
	original := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, Server: ownerCreateServerSHA(t, client)}
	initialConfig, contextValue := renderOwnerProjectConfig(t, domain.Context{YardName: "test-yard", IncusProject: original.Project, YardInstanceName: "yard-test-yard"}, []string{"SUBYARD_HOME=/original/data", "SUBYARD_CONFIG_HOME=/original/config", "YARD_NAME=test-yard", "YARD_INSTANCE_NAME=yard-test-yard", "HOST_BASE=/original/host", "SRV_POOL=original-pool", "SRV_VOLUME=original-srv", "STORAGE_PATH=/original/storage", "YARD_KIND=container"})
	original.InitialConfig = initialConfig
	ctx := ports.WithNativeOperationObserver(context.Background(), observer)
	if created, err := client.ApplyPreparedOwnerImageProject(ctx, original, "recreate"); err != nil || created != nil || fixture.posts != 0 {
		t.Fatal("image-project false branch changed its original no-op", err)
	}
	for _, change := range []string{"missing-config", "shared", "present"} {
		invalid := original
		switch change {
		case "missing-config":
			invalid.InitialConfig = nil
		case "shared":
			invalid.SharedImages = true
		case "present":
			invalid.Absent = false
		}
		if created, err := client.ApplyPreparedOwnerYardProject(ctx, invalid, "recreate"); !errors.Is(err, domain.ErrPlanStale) || created != nil || fixture.posts != 0 {
			t.Fatal("foreign 02 position submitted", change, err)
		}
	}
	created, err := client.ApplyPreparedOwnerYardProject(ctx, original, "recreate")
	if err != nil || created == nil || fixture.posts != 1 || observer.before != 1 || observer.completed != 1 {
		t.Fatal("original 02 project request lost", err)
	}
	if fixture.project.Config["features.images"] != "true" || fixture.project.Config["user.subyard.context"] != contextValue || fixture.project.Config["restricted.devices.proxy"] != "allow" {
		t.Fatal("original config or daemon default images changed")
	}
	fixture.project.UsedBy = []string{"/1.0/instances/yard?project=" + original.Project}
	if err := client.CheckOwnerCreatedYardProject(ctx, original, *created); err != nil {
		t.Fatal("own later instance invalidated original project", err)
	}
	for _, change := range []string{"ref", "request", "config", "unsafe-default"} {
		foreign := *created
		switch change {
		case "ref":
			foreign.Ref.Producer = strings.Repeat("d", 64)
		case "request":
			foreign.Intent.RequestDigest = strings.Repeat("d", 64)
		case "config":
			fixture.project.Config["restricted"] = "false"
		case "unsafe-default":
			fixture.project.Config["restricted"] = "true"
			fixture.project.Config["features.networks"] = "true"
		}
		if client.CheckOwnerCreatedYardProject(ctx, original, foreign) == nil {
			t.Fatal("foreign project successor admitted", change)
		}
	}
}

func TestOwnerInstanceCreateUsesOneOriginalPOSTAndKnownSDKAndResourceUUIDs(t *testing.T) {
	for _, mode := range []string{"instance-success", "instance-lost-response", "instance-missing-id", "instance-wrong-resource", "instance-missing-uuid", "instance-result-error", "instance-config-diff", "instance-wrong-fingerprint"} {
		t.Run(mode, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{failResult: mode == "instance-result-error"}
			client, fixture := ownerCreateClientFixture(t, observer, mode)
			root := testkit.TempDir(t)
			for _, key := range ownerImageEnvironmentKeys {
				t.Setenv(key, "")
			}
			t.Setenv("HOME", root)
			t.Setenv("INCUS_CONF", filepath.Join(root, "config"))
			t.Setenv("INCUS_GLOBAL_CONF", filepath.Join(root, "global"))
			t.Setenv("INCUS_SOCKET", client.socket)
			environment := append(os.Environ(), "INCUS_PROJECT=original-project")
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "local:debian/13", "local:fallback", domain.YardContainer, environment, root)
			if err != nil {
				t.Fatal(err)
			}
			p := ports.PreparedOwnerInstance{Project: "original-project", Instance: "yard", Scope: "current", Policy: &policy, Server: ownerCreateServerSHA(t, client), Rendered: []byte("local:debian/13\x00container\x00-c\x00security.nesting=true\x00-c\x00boot.autostart=false\x00-c\x00user.subyard.managed=true\x00-c\x00user.subyard.name=test-yard\x00-c\x00user.subyard.bridge=incusbr0\x00-c\x00user.subyard.desired_power=stopped\x00-c\x00user.subyard.initialized=false\x00")}
			ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), "original-accepted-owner")
			created, _, err := client.ApplyPreparedOwnerPolicyInstance(ctx, p, environment, root)
			fixture.mu.Lock()
			posts := fixture.posts
			fixture.mu.Unlock()
			if mode != "instance-success" {
				if err == nil || created != nil || observer.created != nil || !observer.uncertain || posts != 1 {
					t.Fatal("unknown create obtained result or lost custody", err)
				}
				return
			}
			if err != nil || created == nil || posts != 1 || observer.before != 1 || len(observer.accepted) != 1 || len(observer.terminal) != 1 || created.OperationID != "owner-create-operation" || created.InstanceUUID != "original-instance-uuid" {
				t.Fatal("original SDK/resource causal chain lost", err)
			}
			if err := client.CheckOwnerCreatedInstance(ctx, p, *created); err != nil {
				t.Fatal("original returned ref refused", err)
			}
			fixture.mu.Lock()
			birth := ownerCreateDigest(fixture.instance)
			fixture.mu.Unlock()
			if created.IdentityDigest != birth {
				t.Fatal("birth evidence did not bind full returned instance")
			}
			for _, field := range []string{"birth", "request", "ref", "nonce", "fingerprint", "operation"} {
				bad := *created
				switch field {
				case "birth":
					bad.IdentityDigest = strings.Repeat("f", 64)
				case "request":
					bad.Intent.RequestDigest = strings.Repeat("f", 64)
				case "ref":
					bad.Ref.Producer = strings.Repeat("f", 64)
				case "nonce":
					bad.Intent.Token = strings.Repeat("f", 32)
				case "fingerprint":
					bad.Intent.Fingerprint = strings.Repeat("f", 64)
				case "operation":
					bad.OperationID = "foreign"
				}
				if client.CheckOwnerCreatedInstance(ctx, p, bad) == nil {
					t.Fatal("tampered authenticated birth admitted", field)
				}
			}
			fixture.mu.Lock()
			fixture.instance.Config["volatile.uuid"] = "foreign-uuid"
			fixture.mu.Unlock()
			if err := client.CheckOwnerCreatedInstance(ctx, p, *created); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("replaced resource UUID admitted", err)
			}
			fixture.mu.Lock()
			fixture.instance.Config["volatile.uuid"] = "original-instance-uuid"
			fixture.mu.Unlock()
			observer.created = nil
			observer.records = nil
			if err := client.CheckOwnerCreatedInstance(ctx, p, *created); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("name/token inferred missing original result", err)
			}
		})
	}
}

func ownerCreateInstanceOriginal(t *testing.T, client *Client, kind domain.YardKind) ports.PreparedOwnerInstance {
	t.Helper()
	root := testkit.TempDir(t)
	for _, key := range ownerImageEnvironmentKeys {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", root)
	t.Setenv("INCUS_CONF", filepath.Join(root, "config"))
	t.Setenv("INCUS_GLOBAL_CONF", filepath.Join(root, "global"))
	t.Setenv("INCUS_SOCKET", client.socket)
	policy, err := client.CaptureOwnerImagePolicy(context.Background(), "local:debian/13", "local:fallback", kind, append(os.Environ(), "INCUS_PROJECT=original-project"), root)
	if err != nil {
		t.Fatal(err)
	}
	typ, vm := "container", ""
	if kind == domain.YardVM {
		typ = "vm"
		vm = "--vm\x00"
	}
	return ports.PreparedOwnerInstance{Project: "original-project", Instance: "yard", Scope: "current", Policy: &policy, Server: ownerCreateServerSHA(t, client), Rendered: []byte("local:debian/13\x00" + typ + "\x00" + vm + "-c\x00boot.autostart=false\x00-c\x00user.subyard.managed=true\x00-c\x00user.subyard.name=test-yard\x00-c\x00user.subyard.bridge=incusbr0\x00-c\x00user.subyard.desired_power=stopped\x00-c\x00user.subyard.initialized=false\x00")}
}
func TestOwnerVolumeCreateAndAttachUseOriginalCausalChainAndCurrentETag(t *testing.T) {
	for _, mode := range []string{"volume-success", "volume-block-success", "volume-block-invalid-format", "volume-lost-response", "volume-missing-status", "volume-result-error", "volume-etag-conflict", "volume-foreign-instance", "volume-current-instance-config", "volume-current-instance-profile", "volume-foreign-device", "volume-security-extra", "volume-size-extra", "volume-second-GET-extra", "volume-foreign-used-by"} {
		t.Run(mode, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{}
			client, fixture := ownerCreateClientFixture(t, observer, mode)
			kind := domain.YardContainer
			if strings.Contains(mode, "block") {
				kind = domain.YardVM
			}
			original := ownerCreateInstanceOriginal(t, client, kind)
			ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), "original-accepted-owner")
			instanceCreated, _, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, append(os.Environ(), "INCUS_PROJECT=original-project"), os.Getenv("HOME"))
			if err != nil {
				t.Fatal(err)
			}
			helper, err := os.ReadFile("../../../scripts/lib-vm-storage.sh")
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "--noprofile", "--norc", "-peu", "-s")
			command.Env = append(os.Environ(), "YARD_KIND="+string(kind), "SRV_POOL=default", "SRV_VOLUME=yard-srv", "SRV_VOLUME_TYPE=filesystem")
			if kind == domain.YardVM {
				command.Env[len(command.Env)-1] = "SRV_VOLUME_TYPE=block"
				command.Env = append(command.Env, "SRV_VOLUME_SIZE=4GiB")
			}
			command.Stdin = io.MultiReader(bytes.NewReader(helper), strings.NewReader("\nsubyard_srv_config_render\n"))
			rendered, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			volume := ports.PreparedOwnerVolume{Project: original.Project, Instance: original.Instance, Scope: original.Scope, Pool: "default", Volume: "yard-srv", Absent: true, Rendered: rendered, Server: original.Server}
			fixture.mu.Lock()
			fixture.instance.Config["user.subyard.initialized"] = "true"
			fixture.instanceETag = `"after-own-command"`
			if mode == "volume-foreign-instance" {
				fixture.instance.Config["volatile.uuid"] = "foreign"
			}
			if mode == "volume-current-instance-config" {
				fixture.instance.Config["security.privileged"] = "true"
			}
			if mode == "volume-current-instance-profile" {
				fixture.instance.Profiles = append(fixture.instance.Profiles, "foreign")
			}
			fixture.mu.Unlock()
			if mode == "volume-result-error" {
				observer.failResult = true
			}
			created, err := client.ApplyPreparedOwnerVolume(ctx, volume, &original, instanceCreated)
			if mode == "volume-lost-response" || mode == "volume-missing-status" || mode == "volume-result-error" || mode == "volume-foreign-instance" {
				if err == nil || created != nil {
					t.Fatal("unknown or foreign volume gained create authority", err)
				}
				fixture.mu.Lock()
				posts, updates := fixture.volumePosts, fixture.updates
				fixture.mu.Unlock()
				if mode == "volume-foreign-instance" {
					if posts != 0 {
						t.Fatal("foreign original UUID submitted volume")
					}
				} else if posts != 1 || !observer.uncertain {
					t.Fatal("missing volume result lost UNKNOWN")
				}
				if updates != 0 {
					t.Fatal("missing result attached volume")
				}
				return
			}
			if err != nil || created == nil {
				t.Fatal("original sync volume create lost", err)
			}
			if mode == "volume-foreign-device" {
				fixture.mu.Lock()
				fixture.instance.ExpandedDevices = map[string]map[string]string{"srv": {"type": "disk", "source": "foreign"}}
				fixture.mu.Unlock()
			}
			if mode == "volume-foreign-used-by" {
				fixture.mu.Lock()
				fixture.volume.UsedBy = []string{"/1.0/instances/foreign?project=original-project"}
				fixture.mu.Unlock()
			}
			if mode == "volume-block-invalid-format" {
				fixture.mu.Lock()
				fixture.volume.Config["user.subyard.format"] = "foreign"
				fixture.mu.Unlock()
			}
			if mode == "volume-security-extra" || mode == "volume-size-extra" {
				fixture.mu.Lock()
				if mode == "volume-security-extra" {
					fixture.volume.Config["security.shifted"] = "true"
				} else {
					fixture.volume.Config["size"] = "20GiB"
				}
				fixture.mu.Unlock()
				if client.CheckOwnerCreatedVolume(ctx, volume, *created) == nil {
					t.Fatal("new static volume config retained original authority")
				}
			}
			if mode == "volume-second-GET-extra" {
				fixture.mu.Lock()
				fixture.afterVolumeGet = func() {
					fixture.volume.Config["security.shifted"] = "true"
					fixture.afterVolumeGet = nil
				}
				fixture.mu.Unlock()
			}
			fixture.mu.Lock()
			want := fixture.instance.Writable()
			want.Devices = maps.Clone(want.Devices)
			if want.Devices == nil {
				want.Devices = map[string]map[string]string{}
			}
			_, srv, _ := ownerVolumeRequest(volume, created.Intent.Token)
			want.Devices["srv"] = srv
			wantDigest := ownerCreateDigest(want)
			fixture.mu.Unlock()
			err = client.AttachPreparedOwnerVolume(ctx, volume, &original, instanceCreated, created)
			fixture.mu.Lock()
			updates, posts := fixture.updates, fixture.volumePosts
			fixture.mu.Unlock()
			if mode == "volume-etag-conflict" || mode == "volume-foreign-device" || mode == "volume-security-extra" || mode == "volume-size-extra" || mode == "volume-second-GET-extra" || mode == "volume-foreign-used-by" || mode == "volume-block-invalid-format" {
				if err == nil || updates != 0 {
					t.Fatal("foreign device or ETag guard bypassed")
				}
				return
			}
			if err != nil || updates != 1 || posts != 1 {
				t.Fatal("exact current UUID/ETag did not attach original role", err, updates, posts)
			}
			fixture.mu.Lock()
			actualDigest := ownerCreateDigest(fixture.instance.Writable())
			fixture.mu.Unlock()
			if actualDigest != wantDigest {
				t.Fatal("srv PUT changed fields beyond original device")
			}
			if err := client.CheckOwnerCreatedInstance(ctx, original, *instanceCreated); err != nil {
				t.Fatal("own srv attachment lost original identity", err)
			}
			if err := client.SetOwnerCreatedPower(ctx, original, *instanceCreated, "running", true); err != nil {
				t.Fatal("own srv attachment prevented power metadata", err)
			}
			formatted := map[string]string{}
			if kind == domain.YardVM {
				formatted = ownerActualBlockFormat(t, "pending", "", "11111111-2222-3333-4444-555555555555")
			}
			fixture.mu.Lock()
			fixture.volume.UsedBy = []string{"/1.0/instances/yard?project=original-project"}
			if kind == domain.YardVM {
				for key, value := range formatted {
					fixture.volume.Config[key] = value
				}
			} else {
				fixture.volume.Config["volatile.idmap.last"] = "[]"
				fixture.volume.Config["volatile.idmap.next"] = "[]"
			}
			fixture.mu.Unlock()
			if err := client.CheckOwnerCreatedVolume(ctx, volume, *created); err != nil {
				t.Fatal("own attachment or ready formatting lost original volume", err)
			}
			fixture.mu.Lock()
			fixture.volume.Config[ownerCreateTokenKey] = "foreign"
			fixture.mu.Unlock()
			if client.CheckOwnerCreatedVolume(ctx, volume, *created) == nil {
				t.Fatal("foreign volume replaced original retained result")
			}
			fixture.mu.Lock()
			fixture.volume.Config[ownerCreateTokenKey] = created.Intent.Token
			fixture.mu.Unlock()
			observer.records = nil
			if err := client.AttachPreparedOwnerVolume(ctx, volume, &original, instanceCreated, created); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("missing causal result inferred from desired device", err)
			}
		})
	}
}

func TestOwnerExistingVolumeGuardAllowsOnlyOriginalInstanceUsage(t *testing.T) {
	for _, project := range []string{"original-project", "default"} {
		t.Run(project, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{}
			client, fixture := ownerCreateClientFixture(t, observer, "volume-success")
			oldUse := "/1.0/instances/original-foreign?project=" + project
			fixture.volume = &api.StorageVolume{Name: "yard-srv", Type: "custom", ContentType: "filesystem", CreatedAt: time.Unix(100, 0), UsedBy: []string{oldUse}, StorageVolumePut: api.StorageVolumePut{Description: "original", Config: api.ConfigMap{"driver.default": "retained"}}}
			original := ports.PreparedOwnerVolume{Project: project, Instance: "yard", Scope: "current", Pool: "default", Volume: "yard-srv", Rendered: []byte("filesystem\x00\x00type=disk\x00pool=default\x00source=yard-srv\x00path=/srv\x00")}
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			original, err := client.PrepareOwnerVolumeGuard(ctx, original)
			if err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			fixture.volume.UsedBy = []string{oldUse, "/1.0/instances/yard?project=" + project}
			fixture.volume.Config["volatile.idmap.last"] = "[]"
			fixture.volume.Config["volatile.idmap.next"] = "[]"
			if project == "default" {
				fixture.volume.UsedBy = append(fixture.volume.UsedBy, "/1.0/instances/yard")
			}
			fixture.mu.Unlock()
			if err := client.CheckOwnerVolumeGuard(ctx, original); err != nil {
				t.Fatal("own exact attachment invalidated original volume", err)
			}
			for _, change := range []string{"new-foreign", "removed-foreign", "static-config", "description"} {
				fixture.mu.Lock()
				fixture.volume.UsedBy = []string{oldUse}
				fixture.volume.Config = api.ConfigMap{"driver.default": "retained"}
				fixture.volume.Description = "original"
				switch change {
				case "new-foreign":
					fixture.volume.UsedBy = append(fixture.volume.UsedBy, "/1.0/instances/new-foreign?project="+project)
				case "removed-foreign":
					fixture.volume.UsedBy = nil
				case "static-config":
					fixture.volume.Config["security.shifted"] = "true"
				case "description":
					fixture.volume.Description = "foreign"
				}
				fixture.mu.Unlock()
				if client.CheckOwnerVolumeGuard(ctx, original) == nil {
					t.Fatal("unapproved original volume change admitted", change)
				}
			}
		})
	}
}

func ownerCreateServerSHA(t *testing.T, client *Client) string {
	t.Helper()
	value, err := client.OwnerCreateServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestOwnerPolicyInstanceResolvesOnlyAfterAcceptanceAndRetainsExactRequest(t *testing.T) {
	for _, failure := range []string{"none", "no-acceptance", "wrong-reference", "wrong-target"} {
		t.Run(failure, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{}
			client, fixture := ownerCreateClientFixture(t, observer, "instance-success")
			original := ownerCreateInstanceOriginal(t, client, domain.YardContainer)
			env := append(os.Environ(), "INCUS_PROJECT=original-project")
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "local:debian/13", "local:fallback", domain.YardContainer, env, os.Getenv("HOME"))
			if err != nil {
				t.Fatal(err)
			}
			original.Policy = &policy
			original.Image = ports.PreparedOwnerImageSource{}
			ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), "original-accepted-owner")
			if failure == "no-acceptance" {
				ctx = context.Background()
			}
			if failure == "wrong-reference" {
				original.Rendered = bytes.Replace(original.Rendered, []byte("local:debian/13"), []byte("local:foreign"), 1)
			}
			if failure == "wrong-target" {
				original.Server = strings.Repeat("f", 64)
			}
			created, resolved, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, env, os.Getenv("HOME"))
			fixture.mu.Lock()
			posts := fixture.posts
			fixture.mu.Unlock()
			if failure != "none" {
				if err == nil || created != nil || posts != 0 {
					t.Fatal("unapproved policy submitted create", failure, err)
				}
				return
			}
			if err != nil || created == nil || posts != 1 || created.Intent.Fingerprint != strings.Repeat("a", 64) || resolved.Image.Version != 2 || original.Image.Version != 0 {
				t.Fatal("policy/selected request causal identity lost", err)
			}
			if err = client.CheckOwnerCreatedInstance(ctx, resolved, *created); err != nil {
				t.Fatal("exact ephemeral request refused", err)
			}
			if err = client.CheckOwnerCreatedInstance(ctx, original, *created); err != nil {
				t.Fatal("original zero-image capsule lost typed result", err)
			}
		})
	}
}

func ownerPrivatePolicyFixture(t *testing.T, mode, prefix string) (*Client, *ownerCreateServerFixture, *ownerSecretFixture, ports.PreparedOwnerInstance, *ownerCreateObserverFixture, context.Context) {
	t.Helper()
	observer := &ownerCreateObserverFixture{}
	client, target := ownerCreateClientFixture(t, observer, mode)
	original := ownerCreateInstanceOriginal(t, client, domain.YardContainer)
	source, args, _, _, _ := newOwnerSecretFixture(t)
	source.project = original.Project
	root, config := os.Getenv("HOME"), os.Getenv("INCUS_CONF")
	for _, path := range []string{config, filepath.Join(config, "servercerts"), filepath.Join(config, "clientcerts")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(config, "servercerts/private.crt"), []byte(args.TLSServerCert), 0o600)
	testkit.WriteFile(t, filepath.Join(config, "clientcerts/private.crt"), []byte(args.TLSClientCert), 0o600)
	testkit.WriteFile(t, filepath.Join(config, "clientcerts/private.key"), []byte(args.TLSClientKey), 0o600)
	testkit.WriteFile(t, filepath.Join(config, "config.yml"), []byte(fmt.Sprintf("remotes:\n  private:\n    addr: %q\n    protocol: incus\n    auth_type: tls\n    public: false\n    project: ignored-original-project\n", source.server.URL+prefix)), 0o600)
	env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
	policy, err := client.CaptureOwnerImagePolicy(context.Background(), "private:debian/13", "local:fallback", domain.YardContainer, env, root)
	if err != nil {
		t.Fatal(err)
	}
	original.Policy = &policy
	original.Rendered = bytes.Replace(original.Rendered, []byte("local:debian/13"), policy.Primary, 1)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(stop)
	ctx = ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(ctx, observer), "original-private-policy-invocation")
	return client, target, source, original, observer, ctx
}

func TestOwnerPrivatePolicyImageUsesArmedTokenAndSingleInstancePOST(t *testing.T) {
	for _, scenario := range []string{"success", "recreate-prefix", "lost-UUID", "private-error", "private-operation-failure", "foreign-fingerprint"} {
		t.Run(scenario, func(t *testing.T) {
			mode, prefix := "instance-success", ""
			if scenario == "lost-UUID" {
				mode = "instance-missing-id"
			}
			if scenario == "private-error" {
				mode = "instance-private-error"
			}
			if scenario == "private-operation-failure" {
				mode = "instance-private-failure"
			}
			if scenario == "recreate-prefix" {
				prefix = "/original-source"
			}
			client, target, source, original, observer, ctx := ownerPrivatePolicyFixture(t, mode, prefix)
			if scenario == "foreign-fingerprint" {
				source.corrupt = "image-fingerprint"
			}
			if scenario == "recreate-prefix" {
				original.Scope = "recreate"
			}
			env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
			if _, err := client.ReadOwnerPolicyImageCatalog(context.Background(), *original.Policy, env, os.Getenv("HOME")); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("preacceptance catalog authenticated")
			}
			if _, err := client.ReadOwnerPolicyImageCatalog(ctx, *original.Policy, env, os.Getenv("HOME")); err != nil {
				t.Fatal(err)
			}
			var sentDigest string
			target.onInstancePost = func(request api.InstancesPost) {
				select {
				case <-source.armed:
				default:
					t.Error("target POST preceded source wait barrier")
				}
				if request.Source.Secret != strings.Repeat("b", 64) || request.Source.Fingerprint != strings.Repeat("a", 64) || request.Source.Project != original.Project {
					t.Error("private fixed source differs")
				}
				sentDigest = ownerCreateDigest(request)
				source.consume(true)
			}
			created, resolved, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, env, os.Getenv("HOME"))
			if scenario == "foreign-fingerprint" {
				if err == nil || created != nil || source.posts != 0 || target.posts != 0 || observer.before != 0 {
					t.Fatal("alias target A accepted returned fingerprint B or submitted a create")
				}
				return
			}
			if scenario == "lost-UUID" || scenario == "private-error" || scenario == "private-operation-failure" {
				if err == nil || created != nil || observer.created != nil || !observer.uncertain || strings.Contains(err.Error(), strings.Repeat("b", 64)) {
					t.Fatal("unproved private create gained result or disclosed secret")
				}
				if scenario == "private-operation-failure" {
					var failure *ports.NativeOperationFailure
					if !errors.As(err, &failure) || failure.Operation.Status != 400 || failure.Operation.Kind != "owner-create-instance" {
						t.Fatal("sanitization lost exact closed operation failure", err)
					}
				}
				return
			}
			if err != nil || created == nil || created.Ref.Index != 2 || created.Intent.RequestDigest != sentDigest || resolved.Image.Version != 2 || original.Image.Version != 0 {
				t.Fatal("private token/create causal chain lost", err)
			}
			raw, _ := json.Marshal(created)
			if strings.Contains(string(raw), strings.Repeat("b", 64)) {
				t.Fatal("secret persisted in typed result")
			}
			source.mu.Lock()
			reads, posts, events := source.metadata, source.posts, source.events
			source.mu.Unlock()
			if posts != 1 || events != 0 {
				t.Fatal("token retried or hidden event work")
			}
			target.mu.Lock()
			target.instance.Config["user.subyard.initialized"] = "true"
			target.instanceETag = `"own-metadata"`
			target.mu.Unlock()
			if err := client.CheckOwnerCreatedInstance(ctx, original, *created); err != nil {
				t.Fatal("original zero-image capsule could not authenticate its result", err)
			}
			source.mu.Lock()
			currentReads := source.metadata
			source.mu.Unlock()
			if currentReads != reads {
				t.Fatal("successor re-resolved image source")
			}
			foreign := *created
			foreign.Ref.Producer = strings.Repeat("e", 64)
			if client.CheckOwnerCreatedInstance(ctx, original, foreign) == nil {
				t.Fatal("foreign producer ref admitted")
			}
			target.mu.Lock()
			target.instance.Config["volatile.base_image"] = strings.Repeat("f", 64)
			target.mu.Unlock()
			if client.CheckOwnerCreatedInstance(ctx, original, *created) == nil {
				t.Fatal("foreign image fingerprint admitted")
			}
			observer.mu.Lock()
			terminals := append([]ports.NativeOperation(nil), observer.terminal...)
			observer.mu.Unlock()
			if len(terminals) != 2 {
				t.Fatal("token or target terminal missing")
			}
			target.mu.Lock()
			posts = target.posts
			target.mu.Unlock()
			if posts != 1 {
				t.Fatal("instance POST retried")
			}
		})
	}
}

func TestOwnerPrivatePolicyImageRecoveryDrainsOnlyExactOriginalToken(t *testing.T) {
	for _, changed := range []string{"none", "missing", "server", "resource", "kind", "changed-config"} {
		t.Run(changed, func(t *testing.T) {
			client, _, source, original, _, ctx := ownerPrivatePolicyFixture(t, "instance-success", "/original-source")
			env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
			connection, err := client.ownerPolicyImageServer(ctx, *original.Policy, string(original.Policy.Primary), env, os.Getenv("HOME"))
			if err != nil {
				t.Fatal(err)
			}
			expected, err := nativeOperation(connection.ImageServer.(incus.InstanceServer), "owner-image-secret-current-primary", original.Project, strings.Repeat("a", 64))
			connection.close()
			if err != nil {
				t.Fatal(err)
			}
			expected.ID = source.op.ID
			switch changed {
			case "missing":
				source.lookupGone = true
			case "server":
				expected.Server = strings.Repeat("e", 64)
			case "resource":
				expected.Resource = strings.Repeat("f", 64)
			case "kind":
				expected.Kind = "owner-image-secret-foreign-primary"
			case "changed-config":
				testkit.WriteFile(t, filepath.Join(os.Getenv("INCUS_CONF"), "client.crt"), []byte("changed-original-credential"), 0o600)
			}
			terminal, err := client.DrainOwnerImageSecretOperation(ctx, *original.Policy, string(original.Policy.Primary), env, os.Getenv("HOME"), expected)
			if changed == "none" {
				if err != nil || terminal.ID != expected.ID || terminal.Status != 401 {
					t.Fatal("exact token did not drain", err)
				}
			} else if err == nil {
				t.Fatal("foreign/missing token gained closure")
			}
			source.mu.Lock()
			defer source.mu.Unlock()
			if source.posts != 0 || source.metadata != 0 || (changed != "none" && source.deletes != 0) {
				t.Fatal("recovery replanned, minted, or cancelled foreign token")
			}
		})
	}
}

func TestOwnerCreatedPowerUsesSameKnownUUIDAndETag(t *testing.T) {
	for _, changed := range []string{"none", "runtime-facts", "replaced-before-GET", "replaced-after-GET", "wrong-nonce", "wrong-fingerprint", "invalid-desired", "current-security", "current-profile", "current-host-disk", "current-expanded-policy", "current-volatile"} {
		t.Run(changed, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{}
			client, fixture := ownerCreateClientFixture(t, observer, "instance-success")
			original := ownerCreateInstanceOriginal(t, client, domain.YardContainer)
			ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), "original-power-invocation")
			env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
			created, _, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, env, os.Getenv("HOME"))
			if err != nil {
				t.Fatal(err)
			}
			if err := client.CheckOwnerCreatedInstance(ctx, original, *created); err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			if changed == "replaced-before-GET" {
				fixture.instance.Config["volatile.uuid"] = "foreign"
			}
			switch changed {
			case "wrong-nonce":
				fixture.instance.Config[ownerCreateTokenKey] = strings.Repeat("f", 32)
			case "wrong-fingerprint":
				fixture.instance.Config["volatile.base_image"] = strings.Repeat("f", 64)
			case "runtime-facts":
				fixture.instance.Config["volatile.last_state.power"] = "RUNNING"
				fixture.instance.Config["volatile.last_state.ready"] = "true"
				fixture.instance.Config["volatile.eth0.host_name"] = "veth-owned"
				fixture.instance.ExpandedConfig["volatile.eth0.host_name"] = "veth-owned"
			case "current-security":
				fixture.instance.Config["security.privileged"] = "true"
			case "current-profile":
				fixture.instance.Profiles = append(fixture.instance.Profiles, "foreign")
			case "current-host-disk":
				fixture.instance.Devices = map[string]map[string]string{"host": {"type": "disk", "path": "/host", "source": "/"}}
			case "current-expanded-policy":
				fixture.instance.ExpandedConfig["security.privileged"] = "true"
			case "current-volatile":
				fixture.instance.Config["volatile.unapproved"] = "true"
			}
			if changed == "replaced-after-GET" {
				fixture.afterInstanceGet = func() {
					fixture.instance.Config["volatile.uuid"] = "foreign"
					fixture.instanceETag = `"foreign-generation"`
					fixture.afterInstanceGet = nil
				}
			}
			want := fixture.instance.Writable()
			want.Config = maps.Clone(want.Config)
			want.Config["user.subyard.desired_power"] = "running"
			want.Config["user.subyard.initialized"] = "true"
			wantDigest := ownerCreateDigest(want)
			fixture.mu.Unlock()
			desired := "running"
			if changed == "invalid-desired" {
				desired = "foreign"
			}
			err = client.SetOwnerCreatedPower(ctx, original, *created, desired, true)
			fixture.mu.Lock()
			updates := fixture.updates
			fixture.mu.Unlock()
			if changed == "replaced-before-GET" || changed == "replaced-after-GET" || (changed == "invalid-desired" || changed == "wrong-nonce" || changed == "wrong-fingerprint") {
				if err == nil || updates != 0 {
					t.Fatal("replacement or foreign power gained metadata")
				}
				return
			}
			if err != nil || updates != 1 {
				t.Fatal("known created power failed", err)
			}
			fixture.mu.Lock()
			actualDigest := ownerCreateDigest(fixture.instance.Writable())
			fixture.mu.Unlock()
			if actualDigest != wantDigest {
				t.Fatal("power PUT changed fields beyond two original keys")
			}
			if err := client.CheckOwnerCreatedInstance(ctx, original, *created); err != nil {
				t.Fatal("own power metadata invalidated causal identity", err)
			}
		})
	}
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
incus_wait_instance_agent() { :; }
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
	formatted := ownerActualBlockFormat(t, "pending", "", uuid)
	volume := &api.StorageVolume{Name: "yard-srv", Type: "custom", ContentType: "block", StorageVolumePut: api.StorageVolumePut{Config: map[string]string{"size": "4GiB", "user.subyard.storage": "v1", "user.subyard.format": "pending"}}}
	original := ownerVolumeStaticDigest(volume, "original-project", "yard", true)
	for key, value := range formatted {
		volume.Config[key] = value
	}
	p := ports.PreparedOwnerVolume{Project: "original-project", Instance: "yard"}
	request := api.StorageVolumesPost{ContentType: "block", StorageVolumePut: api.StorageVolumePut{Config: map[string]string{"user.subyard.format": "pending"}}}
	if !ownerVolumeDigestMatches(volume, p, request, original) {
		t.Fatal("actual pending-to-ready format rejected")
	}
	ready := ownerVolumeStaticDigest(volume, p.Project, p.Instance, true)
	if raw, err := ownerBlockFormatCommand(t, "ready", uuid, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err == nil || len(raw) != 0 {
		t.Fatal("original producer accepted changed ready filesystem UUID")
	}
	ownerActualBlockFormat(t, "ready", uuid, uuid)
	for _, changed := range []string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "", "invalid"} {
		volume.Config["user.subyard.uuid"] = changed
		if ownerVolumeDigestMatches(volume, p, request, ready) {
			t.Fatal("original ready UUID changed", changed)
		}
	}
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

func TestOwnerPolicyOCIFallbackDelegatesOnlyOriginalAliasToOneDaemonPOST(t *testing.T) {
	for _, mode := range []string{"instance-success", "instance-lost-response", "instance-missing-id", "instance-wrong-resource", "instance-missing-uuid", "instance-invalid-base-image", "instance-result-error", "wrong-reference", "source-alias", "source-server", "source-protocol", "record-reference", "record-protocol", "record-fingerprint"} {
		t.Run(mode, func(t *testing.T) {
			observer := &ownerCreateObserverFixture{failResult: mode == "instance-result-error"}
			client, target := ownerCreateClientFixture(t, observer, mode)
			// Keep the fixture's real SDK endpoints for request guard failures too.
			if !strings.HasPrefix(mode, "instance-") {
				target.mode = "instance-success"
			}
			target.oci = true
			original := ownerCreateInstanceOriginal(t, client, domain.YardContainer)
			root := os.Getenv("HOME")
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "unexpected-image-helper")
			t.Setenv("OCI_EFFECT_MARKER", marker)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			testkit.WriteFile(t, filepath.Join(bin, "skopeo"), []byte("#!/bin/sh\nprintf invoked >\"$OCI_EFFECT_MARKER\"\nexit 1\n"), 0o700)
			var registryRequests atomic.Int32
			registry := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { registryRequests.Add(1) }))
			t.Cleanup(registry.Close)
			configuration := []byte(fmt.Sprintf("remotes:\n  oci:\n    addr: %q\n    protocol: oci\n    public: true\n", registry.URL))
			if err := os.MkdirAll(os.Getenv("INCUS_CONF"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(os.Getenv("INCUS_CONF"), "config.yml"), configuration, 0o600)
			env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "local:missing", "oci:library/alpine:3.22", domain.YardContainer, env, root)
			if err != nil {
				t.Fatal(err)
			}
			original.Policy = &policy
			original.Rendered = bytes.Replace(original.Rendered, []byte("local:debian/13"), policy.Primary, 1)
			ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), "original-oci-fallback")
			catalog, err := client.ReadOwnerPolicyImageCatalog(ctx, policy, env, root)
			if err != nil {
				t.Fatal(err)
			}
			selected := ownerOriginalOCIFallback(t, catalog, string(policy.Primary), string(policy.Fallback))
			prepared := original
			prepared.Rendered = bytes.Replace(prepared.Rendered, policy.Primary, []byte(selected), 1)
			expectedSource := api.InstanceSource{Type: "image", Mode: "pull", Protocol: "oci", Server: registry.URL, Alias: "library/alpine:3.22"}
			target.mu.Lock()
			target.onInstancePost = func(request api.InstancesPost) {
				if ownerCreateDigest(request.Source) != ownerCreateDigest(expectedSource) {
					t.Error("OCI POST changed original alias/server or introduced credentials/fingerprint")
				}
			}
			target.mu.Unlock()
			var created *ports.NativeOwnerCreated
			var resolved ports.PreparedOwnerInstance
			if strings.HasPrefix(mode, "source-") || strings.HasPrefix(mode, "record-") {
				source, secret, resolveErr := client.resolveOwnerPolicyImageSource(ctx, policy, selected, original.Scope, env, root)
				if resolveErr != nil || secret != nil {
					t.Fatal("anonymous OCI resolver required effects", resolveErr)
				}
				prepared.Image = ports.PreparedOwnerImageSource{Version: 2, Reference: []byte(selected), Kind: policy.Kind, Protocol: source.Protocol, Server: []byte(source.Server), Alias: []byte(source.Alias)}
				switch mode {
				case "source-alias":
					source.Alias = "foreign"
				case "source-server":
					source.Server = "https://foreign.invalid"
				case "source-protocol":
					source.Protocol = "incus"
				case "record-reference":
					prepared.Image.Reference = policy.Primary
				case "record-protocol":
					prepared.Image.Protocol = "incus"
				case "record-fingerprint":
					prepared.Image.Fingerprint = strings.Repeat("f", 64)
				}
				created, err = client.applyPreparedOwnerInstance(ctx, prepared, source)
			} else {
				if mode == "wrong-reference" {
					prepared.Rendered = bytes.Replace(prepared.Rendered, policy.Fallback, []byte("oci:foreign"), 1)
				}
				created, resolved, err = client.ApplyPreparedOwnerPolicyInstance(ctx, prepared, env, root)
			}
			target.mu.Lock()
			posts := target.posts
			target.mu.Unlock()
			if mode != "instance-success" {
				wantPosts := 0
				if strings.HasPrefix(mode, "instance-") {
					wantPosts = 1
				}
				if err == nil || created != nil || posts != wantPosts || wantPosts == 1 && !observer.uncertain {
					t.Fatal("unproved OCI create gained result/retried", err, posts)
				}
			} else {
				if err != nil || created == nil || posts != 1 || created.Intent.Fingerprint != "" || resolved.Image.Fingerprint != "" || string(resolved.Image.Alias) != expectedSource.Alias || original.Image.Version != 0 {
					t.Fatal("original OCI request or optional fingerprint changed", err)
				}
				request, requestErr := ownerInstanceRequest(resolved, created.Intent.Token)
				if requestErr != nil || created.Intent.RequestDigest != ownerCreateDigest(request) {
					t.Fatal("OCI request digest did not bind the actual alias request", requestErr)
				}
				// A fresh adapter trusts only the exact retained typed result, without
				// resolving the registry alias or submitting another target request.
				fresh := New(client.socket, "projects")
				if err := fresh.CheckOwnerCreatedInstance(ctx, original, *created); err != nil {
					t.Fatal("original zero-image capsule lost OCI result", err)
				}
				foreign := *created
				foreign.Ref.Producer = strings.Repeat("f", 64)
				if fresh.CheckOwnerCreatedInstance(ctx, original, foreign) == nil {
					t.Fatal("foreign OCI create ref admitted")
				}
				target.mu.Lock()
				target.instance.Config["volatile.uuid"] = "replacement"
				target.mu.Unlock()
				if fresh.CheckOwnerCreatedInstance(ctx, original, *created) == nil {
					t.Fatal("replaced OCI instance UUID admitted")
				}
			}
			if registryRequests.Load() != 0 {
				t.Fatal("client inspected OCI registry")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("OCI client invoked skopeo/helper")
			}
			for _, operation := range observer.accepted {
				if operation.Kind != "owner-create-instance" {
					t.Fatal("OCI created a token operation")
				}
			}
		})
	}
}

func ownerOriginalOCIFallback(t *testing.T, catalog []byte, primary, fallback string) string {
	t.Helper()
	path := filepath.Join(testkit.TempDir(t), "catalog")
	testkit.WriteFile(t, path, catalog, 0o600)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	helper, err := os.ReadFile("../../../scripts/lib/instance-create.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "--noprofile", "--norc", "-peu", "-s")
	command.Env = append(os.Environ(), "YARD_IMAGE="+primary, "YARD_IMAGE_FALLBACK="+fallback)
	command.ExtraFiles = []*os.File{file}
	command.Stdin = io.MultiReader(bytes.NewReader(helper), strings.NewReader("\nwarn() { :; }\navailability=$(subyard_instance_catalog_available missing container <&3)\nsubyard_instance_selected_reference \"$availability\"\nprintf '%s' \"$selected_image\"\n"))
	selected, err := command.Output()
	if err != nil || string(selected) != fallback {
		t.Fatal("original jq catalog rule did not select frozen OCI fallback", err)
	}
	return string(selected)
}
