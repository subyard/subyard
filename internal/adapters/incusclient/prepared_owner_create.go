package incusclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	incus "github.com/lxc/incus/v6/client"
	"maps"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/lxc/incus/v6/shared/api"
)

const ownerCreateTokenKey = "user.subyard.owner_create"

func ownerCreateDigest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ownerPrivateCreateError(err error) error {
	if failure, ok := err.(*ports.NativeOperationFailure); ok {
		return &ports.NativeOperationFailure{Operation: failure.Operation, Cause: errors.New("original private image instance operation failed")}
	}
	return errors.New("original private image instance request unproved")
}

func ownerImageProjectRequest(p ports.PreparedImageProject, token string) (api.ProjectsPost, error) {
	request := api.ProjectsPost{Name: p.Project, ProjectPut: api.ProjectPut{Config: map[string]string{}}}
	if token != "" {
		request.Config[ownerCreateTokenKey] = token
	}
	if p.SharedImages {
		request.Config["features.images"] = "false"
	}
	if p.InitialConfig == nil {
		return request, nil
	}
	if len(p.InitialConfig) == 0 || len(p.InitialConfig) > 32<<20 || p.InitialConfig[len(p.InitialConfig)-1] != 0 {
		return request, domain.ErrPlanStale
	}
	// This is the existing 02 renderer's closed output grammar, not another
	// policy renderer. Values remain exactly the original captured bytes.
	keys := strings.Fields("user.subyard.installation user.subyard.operator_uid user.subyard.context user.subyard.data_home user.subyard.config_home user.subyard.yard_name user.subyard.instance_name user.subyard.host_base user.subyard.bridge user.subyard.srv_pool user.subyard.srv_volume user.subyard.storage_pool user.subyard.storage_path restricted restricted.containers.nesting restricted.containers.privilege restricted.containers.interception restricted.devices.disk restricted.devices.unix-char restricted.devices.proxy")
	seen := map[string]bool{}
	for _, row := range bytes.Split(p.InitialConfig[:len(p.InitialConfig)-1], []byte{0}) {
		key, value, ok := strings.Cut(string(row), "=")
		if !ok || seen[key] || key != "restricted.virtual-machines.lowlevel" && !slices.Contains(keys, key) {
			return request, domain.ErrPlanStale
		}
		seen[key], request.Config[key] = true, value
	}
	for _, key := range keys {
		if !seen[key] {
			return request, domain.ErrPlanStale
		}
	}
	var context domain.Context
	if request.Config["user.subyard.installation"] != "1" || request.Config["user.subyard.operator_uid"] != strconv.Itoa(os.Getuid()) || json.Unmarshal([]byte(request.Config["user.subyard.context"]), &context) != nil || context.IncusProject != p.Project || context.YardName != request.Config["user.subyard.yard_name"] || context.YardInstanceName != request.Config["user.subyard.instance_name"] {
		return request, domain.ErrPlanStale
	}
	return request, nil
}

// ApplyPreparedOwnerImageProject consumes only the original prepared image
// namespace. Its fixed sync POST returns explicit causal identity to its caller.
// The ordinary SDK method remains unchanged for non-native component callers.
func (client *Client) ApplyPreparedOwnerImageProject(ctx context.Context, p ports.PreparedImageProject, scope string) (created *ports.NativeOwnerCreated, result error) {
	return client.applyPreparedOwnerProject(ctx, p, scope, false)
}

// ApplyPreparedOwnerYardProject consumes the original 02 project position when
// images are not shared. Its original initial configuration is mandatory.
func (client *Client) ApplyPreparedOwnerYardProject(ctx context.Context, p ports.PreparedImageProject, scope string) (*ports.NativeOwnerCreated, error) {
	if p.SharedImages || !p.Absent || len(p.InitialConfig) == 0 {
		return nil, domain.ErrPlanStale
	}
	return client.applyPreparedOwnerProject(ctx, p, scope, true)
}

func (client *Client) applyPreparedOwnerProject(ctx context.Context, p ports.PreparedImageProject, scope string, yard bool) (created *ports.NativeOwnerCreated, result error) {
	observer := ports.NativeOperationObserverFrom(ctx)
	creates, ok := observer.(ports.NativeOwnerCreateObserver)
	if !ok || !ownerCreateScope(scope) || !domain.SafeName(p.Project) || p.Absent && p.Config != nil {
		return nil, domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return nil, err
	}
	identity, err := nativeOperation(server, "owner-create-project", p.Project, p.Project)
	if err != nil || p.Server == "" || identity.Server != p.Server {
		return nil, domain.ErrPlanStale
	}
	actual, _, err := server.GetProject(p.Project)
	if !p.Absent || !p.SharedImages && !yard {
		// Reuse the unchanged original guard/no-op semantics. No create result
		// is manufactured for a preexisting or unselected namespace.
		return nil, client.ApplyPreparedImageProject(ctx, p)
	}
	if !api.StatusErrorCheck(err, http.StatusNotFound) || actual != nil {
		return nil, domain.ErrPlanStale
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(nonce[:])
	request, err := ownerImageProjectRequest(p, token)
	if err != nil {
		return nil, err
	}
	intent := ports.NativeOwnerCreateIntent{Scope: scope, Kind: "project", Domain: "command", Token: token, RequestDigest: ownerCreateDigest(request), Server: identity.Server, Project: p.Project, Resource: p.Project}
	ref, err := creates.BeforeOwnerCreate(intent)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			observer.Uncertain()
		}
	}()
	// Only this fixed typed project POST uses the official SDK's response-level
	// API: CreateProject discards the sync response type/status we must prove.
	response, _, err := server.RawQuery(http.MethodPost, "/1.0/projects", request, "")
	if err != nil {
		return nil, normalizeError("create original owner image project", err)
	}
	if response == nil || response.Type != api.SyncResponse || response.StatusCode != int(api.Success) || response.Operation != "" {
		return nil, errors.New("owner project sync response unavailable")
	}
	if err := observer.CommandTerminal(ref.Index); err != nil {
		return nil, err
	}
	actual, etag, err := server.GetProject(p.Project)
	if err != nil || actual == nil || actual.Name != p.Project || p.SharedImages && actual.Config["features.images"] != "false" || actual.Config[ownerCreateTokenKey] != token || etag == "" {
		return nil, errors.New("owner project successful create identity unavailable")
	}
	// Compare the exact JSON values sent through the existing SDK encoding.
	// The original lossless source columns remain in the approved capsule.
	wire, _ := json.Marshal(request)
	var sent api.ProjectsPost
	if json.Unmarshal(wire, &sent) != nil {
		return nil, errors.New("owner project request encoding unavailable")
	}
	for key, value := range sent.Config {
		if actual.Config[key] != value {
			return nil, errors.New("owner project initial configuration differs")
		}
	}
	value := ports.NativeOwnerCreated{Ref: ref, Intent: intent, Status: 200, IdentityDigest: ownerCreateDigest(actual), ETagDigest: ownerCreateDigest(etag)}
	if err := creates.RecordOwnerCreated(value); err != nil {
		return nil, err
	}
	return &value, nil
}

// CheckOwnerCreatedImageProject validates one exact returned reference. A
// matching name or token alone cannot supply its original successful request.
func (client *Client) CheckOwnerCreatedImageProject(ctx context.Context, p ports.PreparedImageProject, expected ports.NativeOwnerCreated) error {
	if !p.SharedImages {
		return domain.ErrPlanStale
	}
	return client.checkOwnerCreatedProject(ctx, p, expected)
}

func (client *Client) CheckOwnerCreatedYardProject(ctx context.Context, p ports.PreparedImageProject, expected ports.NativeOwnerCreated) error {
	if p.SharedImages || len(p.InitialConfig) == 0 {
		return domain.ErrPlanStale
	}
	return client.checkOwnerCreatedProject(ctx, p, expected)
}

func (client *Client) checkOwnerCreatedProject(ctx context.Context, p ports.PreparedImageProject, expected ports.NativeOwnerCreated) error {
	creates, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerCreateObserver)
	request, requestErr := ownerImageProjectRequest(p, expected.Intent.Token)
	if !ok || requestErr != nil || !p.Absent || p.Config != nil || !ownerCreateScope(expected.Intent.Scope) || expected.Status != 200 || expected.Ref.Domain != "command" || !ownerImageHex(expected.Intent.Token, 32) || expected.Intent.Kind != "project" || expected.Intent.Project != p.Project || expected.Intent.Resource != p.Project || expected.Intent.RequestDigest != ownerCreateDigest(request) {
		return domain.ErrPlanStale
	}
	retained, err := creates.ReadOwnerCreated(expected.Ref)
	if err != nil || retained != expected {
		return domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	identity, err := nativeOperation(server, "owner-create-project", p.Project, p.Project)
	if err != nil || identity.Server != expected.Intent.Server || p.Server == "" || identity.Server != p.Server {
		return domain.ErrPlanStale
	}
	actual, etag, err := server.GetProject(p.Project)
	if err != nil || actual == nil || actual.Name != p.Project || actual.Config[ownerCreateTokenKey] != expected.Intent.Token || etag == "" {
		return domain.ErrPlanStale
	}
	wire, _ := json.Marshal(request)
	var sent api.ProjectsPost
	if json.Unmarshal(wire, &sent) != nil {
		return domain.ErrPlanStale
	}
	for key, value := range sent.Config {
		if actual.Config[key] != value {
			return domain.ErrPlanStale
		}
	}
	for key, value := range actual.Config {
		if _, known := sent.Config[key]; !known && (!slices.Contains([]string{"features.images", "features.profiles", "features.storage.volumes", "features.storage.buckets"}, key) || value != "true") {
			return domain.ErrPlanStale
		}
	}
	return nil
}

// ownerInstanceRequest interprets only the retained 03 renderer's fixed flags.
// It never resolves an image alias or selects another source at dispatch.
func ownerInstanceFixedRequest(p ports.PreparedOwnerInstance, token, fingerprint string) (api.InstancesPost, error) {
	request := api.InstancesPost{Name: p.Instance, InstancePut: api.InstancePut{Config: map[string]string{ownerCreateTokenKey: token}}, Source: api.InstanceSource{Type: "image", Fingerprint: fingerprint}}
	if p.Policy == nil || !domain.SafeName(p.Project) || !domain.SafeName(p.Instance) || !ownerCreateScope(p.Scope) || len(p.Rendered) == 0 || len(p.Rendered) > 32<<20 || p.Rendered[len(p.Rendered)-1] != 0 || fingerprint != "" && !ownerImageHex(fingerprint, 64) {
		return request, domain.ErrPlanStale
	}
	fields := bytes.Split(p.Rendered[:len(p.Rendered)-1], []byte{0})
	kind := p.Policy.Kind
	if p.Policy.Version != 1 || len(fields) < 2 || !bytes.Equal(fields[0], p.Policy.Primary) && !bytes.Equal(fields[0], p.Policy.Fallback) {
		return request, domain.ErrPlanStale
	}
	switch string(fields[1]) {
	case "vm":
		request.Type = api.InstanceTypeVM
		if kind != "virtual-machine" {
			return request, domain.ErrPlanStale
		}
	case "container":
		request.Type = api.InstanceTypeContainer
		if kind != "container" {
			return request, domain.ErrPlanStale
		}
	default:
		return request, domain.ErrPlanStale
	}
	flags := fields[2:]
	if request.Type == api.InstanceTypeVM {
		if len(flags) == 0 || string(flags[0]) != "--vm" {
			return request, domain.ErrPlanStale
		}
		flags = flags[1:]
	}
	allowed := strings.Fields("security.nesting security.syscalls.intercept.bpf security.syscalls.intercept.bpf.devices limits.cpu limits.memory boot.autostart user.subyard.managed user.subyard.name user.subyard.bridge user.subyard.desired_power user.subyard.initialized")
	for len(flags) > 0 {
		if len(flags) < 2 || string(flags[0]) != "-c" {
			return request, domain.ErrPlanStale
		}
		key, value, ok := strings.Cut(string(flags[1]), "=")
		if !ok || !slices.Contains(allowed, key) {
			return request, domain.ErrPlanStale
		}
		if _, duplicate := request.Config[key]; duplicate {
			return request, domain.ErrPlanStale
		}
		request.Config[key] = value
		flags = flags[2:]
	}
	if request.Config["boot.autostart"] != "false" || request.Config["user.subyard.managed"] != "true" || request.Config["user.subyard.initialized"] != "false" || request.Config["user.subyard.name"] == "" || request.Config["user.subyard.bridge"] == "" || !slices.Contains([]string{"running", "stopped"}, request.Config["user.subyard.desired_power"]) {
		return request, domain.ErrPlanStale
	}
	return request, nil
}

func ownerCreateScope(scope string) bool {
	return scope == "current" || scope == "legacy" || scope == "recreate"
}

func ownerInstanceRequest(p ports.PreparedOwnerInstance, token string) (api.InstancesPost, error) {
	request, err := ownerInstanceFixedRequest(p, token, p.Image.Fingerprint)
	if err != nil || p.Image.Version != 2 {
		return request, domain.ErrPlanStale
	}
	if p.Image.Protocol == "oci" {
		selected, _, _ := bytes.Cut(p.Rendered, []byte{0})
		request.Source = api.InstanceSource{Type: "image", Mode: "pull", Protocol: "oci", Server: string(p.Image.Server), Certificate: string(p.Image.Certificate), Alias: string(p.Image.Alias), Fingerprint: p.Image.Fingerprint}
		if p.Image.Local || p.Policy.Kind != "container" || !bytes.Equal(selected, p.Policy.Fallback) || bytes.Equal(selected, p.Policy.Primary) || !bytes.Equal(p.Image.Reference, selected) || !ownerImageOCISource(request.Source) {
			return request, domain.ErrPlanStale
		}
		return request, nil
	}
	if !ownerImageHex(p.Image.Fingerprint, 64) || len(p.Image.Alias) != 0 {
		return request, domain.ErrPlanStale
	}
	if p.Image.Local {
		if p.Image.Protocol != "incus" || string(p.Image.Project) != p.Project {
			return request, domain.ErrPlanStale
		}
	} else {
		if (p.Image.Protocol != "simplestreams" && (p.Policy == nil || p.Image.Protocol != "incus")) || len(p.Image.Server) == 0 || p.Policy == nil && len(p.Image.Certificate) == 0 {
			return request, domain.ErrPlanStale
		}
		request.Source.Mode = "pull"
		request.Source.Protocol = p.Image.Protocol
		request.Source.Server = string(p.Image.Server)
		request.Source.Certificate = string(p.Image.Certificate)
	}
	return request, nil
}

// ApplyPreparedOwnerPolicyInstance resolves only the original accepted policy
// and submits one CreateInstance call. A lost response never gains a result.
func (client *Client) ApplyPreparedOwnerPolicyInstance(ctx context.Context, p ports.PreparedOwnerInstance, environment []string, home string) (created *ports.NativeOwnerCreated, resolved ports.PreparedOwnerInstance, result error) {
	if p.Policy == nil || !reflect.DeepEqual(p.Image, ports.PreparedOwnerImageSource{}) || len(p.Rendered) == 0 || p.Rendered[len(p.Rendered)-1] != 0 {
		return nil, ports.PreparedOwnerInstance{}, domain.ErrPlanStale
	}
	selected, _, ok := bytes.Cut(p.Rendered, []byte{0})
	if !ok || !bytes.Equal(selected, p.Policy.Primary) && !bytes.Equal(selected, p.Policy.Fallback) {
		return nil, ports.PreparedOwnerInstance{}, domain.ErrPlanStale
	}
	source, secret, err := client.resolveOwnerPolicyImageSource(ctx, *p.Policy, string(selected), p.Scope, environment, home)
	if err != nil {
		return nil, ports.PreparedOwnerInstance{}, err
	}
	if secret != nil {
		defer func() {
			if err := secret.Close(); err != nil {
				created = nil
				result = errors.Join(result, err)
			}
		}()
		ctx = secret.WorkContext()
	}
	if source.Protocol == "oci" && !ownerImageOCISource(source) || source.Protocol != "oci" && (source.Type != "image" || source.Alias != "" || !ownerImageHex(source.Fingerprint, 64)) || source.Secret != "" && secret == nil || source.Mode != "" && source.Mode != "pull" {
		return nil, ports.PreparedOwnerInstance{}, domain.ErrPlanStale
	}
	// Version two exists only in this accepted call's result handle. The
	// original durable policy capsule retains a zero concrete image record.
	p.Image = ports.PreparedOwnerImageSource{Version: 2, Reference: bytes.Clone(selected), Kind: p.Policy.Kind, Protocol: source.Protocol, Server: []byte(source.Server), Certificate: []byte(source.Certificate), Fingerprint: source.Fingerprint, Alias: []byte(source.Alias), Project: []byte(source.Project)}
	if source.Server == "" && source.Protocol == "incus" {
		server, err := client.connect(ctx, true)
		if err != nil {
			return nil, ports.PreparedOwnerInstance{}, err
		}
		info, err := server.GetConnectionInfo()
		if err != nil || info.SocketPath == "" || source.Project != p.Project {
			return nil, ports.PreparedOwnerInstance{}, domain.ErrPlanStale
		}
		p.Image.Local, p.Image.Server = true, []byte(info.SocketPath)
	}
	created, err = client.applyPreparedOwnerInstance(ctx, p, source)
	return created, p, err
}

func (client *Client) applyPreparedOwnerInstance(ctx context.Context, p ports.PreparedOwnerInstance, source api.InstanceSource) (created *ports.NativeOwnerCreated, result error) {
	observer := ports.NativeOperationObserverFrom(ctx)
	creates, ok := observer.(ports.NativeOwnerCreateObserver)
	if !ok {
		return nil, domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return nil, err
	}
	if p.Image.Local {
		info, err := server.GetConnectionInfo()
		if err != nil || info.SocketPath != string(p.Image.Server) {
			return nil, domain.ErrPlanStale
		}
	}
	target := server.UseProject(p.Project)
	instance, _, err := target.GetInstance(p.Instance)
	if !api.StatusErrorCheck(err, http.StatusNotFound) || instance != nil {
		return nil, domain.ErrPlanStale
	}
	identity, err := nativeOperation(target, "owner-create-instance", p.Project, p.Instance)
	if err != nil {
		return nil, err
	}
	if p.Server == "" || identity.Server != p.Server {
		return nil, domain.ErrPlanStale
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(nonce[:])
	request, err := ownerInstanceRequest(p, token)
	if err != nil {
		return nil, err
	}
	if (p.Image.Protocol == "oci" || source.Protocol == "oci") && (!ownerImageOCISource(source) || !reflect.DeepEqual(request.Source, source)) {
		return nil, domain.ErrPlanStale
	}
	request.Source = source
	defer func() {
		if result != nil && source.Secret != "" {
			result = ownerPrivateCreateError(result)
		}
	}()
	intent := ports.NativeOwnerCreateIntent{Scope: p.Scope, Kind: "instance", Domain: "operation", Token: token, RequestDigest: ownerCreateDigest(request), Server: identity.Server, Project: p.Project, Resource: p.Instance}
	if p.Policy != nil {
		intent.Fingerprint = p.Image.Fingerprint
	}
	ref, err := creates.BeforeOwnerCreate(intent)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			observer.Uncertain()
		}
	}()
	operation, err := target.CreateInstance(request)
	if err != nil {
		return nil, normalizeError("create original owner instance", err)
	}
	if operation == nil {
		return nil, errors.New("original owner create operation unavailable")
	}
	if err := waitNative(ctx, target, operation, "owner-create-instance", p.Project, p.Instance); err != nil {
		return nil, err
	}
	instance, etag, err := target.GetInstance(p.Instance)
	if err != nil || instance == nil || instance.Name != p.Instance || instance.Type != string(request.Type) || instance.Config[ownerCreateTokenKey] != token || !ownerImageHex(instance.Config["volatile.base_image"], 64) || source.Fingerprint != "" && instance.Config["volatile.base_image"] != source.Fingerprint || instance.Project != "" && instance.Project != p.Project || !domain.SafeID(instance.Config["volatile.uuid"]) || etag == "" {
		return nil, errors.New("original owner created instance identity unavailable")
	}
	wire, _ := json.Marshal(request)
	var sent api.InstancesPost
	if json.Unmarshal(wire, &sent) != nil {
		return nil, domain.ErrPlanStale
	}
	for key, value := range sent.Config {
		if instance.Config[key] != value {
			return nil, errors.New("original owner created instance configuration differs")
		}
	}
	// Preserve the full birth response as evidence, never as a lifetime revision.
	value := ports.NativeOwnerCreated{Ref: ref, Intent: intent, OperationID: operation.Get().ID, Status: 200, InstanceUUID: instance.Config["volatile.uuid"], IdentityDigest: ownerCreateDigest(instance), ETagDigest: ownerCreateDigest(etag)}
	if err := creates.RecordOwnerCreated(value); err != nil {
		return nil, err
	}
	return &value, nil
}

// CheckOwnerCreatedInstance requires the exact returned ref and original UUID.
// Birth evidence authenticates the original response; it is not a permanent
// revision of fields owned by later stages. This check does not start the guest.

func (client *Client) CheckOwnerCreatedInstance(ctx context.Context, p ports.PreparedOwnerInstance, expected ports.NativeOwnerCreated) error {
	request, err := client.ownerCreatedInstanceRequest(ctx, p, expected)
	if err != nil {
		return err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	target := server.UseProject(p.Project)
	identity, err := nativeOperation(target, "owner-create-instance", p.Project, p.Instance)
	if err != nil || identity.Server != expected.Intent.Server || p.Server == "" || identity.Server != p.Server {
		return domain.ErrPlanStale
	}
	instance, etag, err := target.GetInstance(p.Instance)
	if err != nil || instance == nil || instance.Name != p.Instance || instance.Type != string(request.Type) || instance.Config[ownerCreateTokenKey] != expected.Intent.Token || instance.Config["volatile.uuid"] != expected.InstanceUUID || etag == "" {
		return domain.ErrPlanStale
	}
	if !ownerCreatedInstanceMatches(instance, request, expected) {
		return domain.ErrPlanStale
	}
	return nil
}

// The retained typed create intent authenticates its opaque full request hash.
// Reconstruct only fixed public flags; a private token is never reconstructed.
func (client *Client) ownerCreatedInstanceRequest(ctx context.Context, p ports.PreparedOwnerInstance, expected ports.NativeOwnerCreated) (api.InstancesPost, error) {
	creates, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerCreateObserver)
	if !ok || expected.Intent.Kind != "instance" || expected.Intent.Domain != "operation" || expected.Intent.Scope != p.Scope || expected.Intent.Project != p.Project || expected.Intent.Resource != p.Instance || expected.Intent.Server != p.Server || expected.Status != 200 || expected.Ref.Domain != "operation" || !ownerImageHex(expected.Intent.Token, 32) || !ownerImageHex(expected.Intent.RequestDigest, 64) || !ownerImageHex(expected.IdentityDigest, 64) || !domain.SafeID(expected.OperationID) || !domain.SafeID(expected.InstanceUUID) {
		return api.InstancesPost{}, domain.ErrPlanStale
	}
	retained, err := creates.ReadOwnerCreated(expected.Ref)
	if err != nil || retained != expected {
		return api.InstancesPost{}, domain.ErrPlanStale
	}
	if p.Policy == nil {
		return api.InstancesPost{}, domain.ErrPlanStale
	}
	environment := slices.Clone(os.Environ())
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], "INCUS_PROJECT=") {
			environment = slices.Delete(environment, index, index+1)
		}
	}
	environment = append(environment, "INCUS_PROJECT="+string(p.Policy.Project))
	if err := client.CheckOwnerImagePolicy(ctx, *p.Policy, environment, os.Getenv("HOME")); err != nil {
		return api.InstancesPost{}, err
	}
	if p.Image.Version != 0 && (p.Image.Version != 2 || p.Image.Fingerprint != expected.Intent.Fingerprint) {
		return api.InstancesPost{}, domain.ErrPlanStale
	}
	return ownerInstanceFixedRequest(p, expected.Intent.Token, expected.Intent.Fingerprint)
}

func ownerCreatedInstanceMatches(instance *api.Instance, request api.InstancesPost, expected ports.NativeOwnerCreated) bool {
	if instance.Type != string(request.Type) || instance.Config["volatile.uuid"] != expected.InstanceUUID || instance.Config[ownerCreateTokenKey] != expected.Intent.Token || expected.Intent.Fingerprint != "" && instance.Config["volatile.base_image"] != expected.Intent.Fingerprint {
		return false
	}
	for key, value := range request.Config {
		if key == "user.subyard.initialized" && (instance.Config[key] == "false" || instance.Config[key] == "true") {
			continue
		}
		if key == "user.subyard.desired_power" && (instance.Config[key] == "running" || instance.Config[key] == "stopped") {
			continue
		}
		if instance.Config[key] != value {
			return false
		}
	}
	// IdentityDigest authenticates the complete birth observation through the
	// retained typed result. Later original owner stages may change other fields.
	return true
}

// SetOwnerCreatedPower validates the original causal UUID and uses the ETag of
// that same GET. A replacement by name cannot receive owner power metadata.

func (client *Client) SetOwnerCreatedPower(ctx context.Context, p ports.PreparedOwnerInstance, expected ports.NativeOwnerCreated, desired string, initialized bool) error {
	if desired != "running" && desired != "stopped" {
		return domain.ErrPlanStale
	}
	request, err := client.ownerCreatedInstanceRequest(ctx, p, expected)
	if err != nil {
		return err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	target := server.UseProject(p.Project)
	identity, err := nativeOperation(target, "owner-create-instance", p.Project, p.Instance)
	if err != nil || identity.Server != p.Server {
		return domain.ErrPlanStale
	}
	instance, etag, err := target.GetInstance(p.Instance)
	if err != nil || instance == nil || instance.Name != p.Instance || etag == "" || !ownerCreatedInstanceMatches(instance, request, expected) {
		return domain.ErrPlanStale
	}
	ready := strconv.FormatBool(initialized)
	if instance.Config["user.subyard.desired_power"] == desired && instance.Config["user.subyard.initialized"] == ready {
		return nil
	}
	put := instance.Writable()
	put.Config = maps.Clone(instance.Config)
	put.Config["user.subyard.desired_power"], put.Config["user.subyard.initialized"] = desired, ready
	if err := beforeNative(ctx, target, "instance-config", p.Project, p.Instance); err != nil {
		return err
	}
	op, err := target.UpdateInstance(p.Instance, put, etag)
	if err != nil {
		ports.NativeOperationObserverFrom(ctx).Uncertain()
		return normalizeError("set original created owner power metadata", err)
	}
	return waitNative(ctx, target, op, "instance-config", p.Project, p.Instance)
}

func ownerVolumeRequest(p ports.PreparedOwnerVolume, token string) (api.StorageVolumesPost, map[string]string, error) {
	request := api.StorageVolumesPost{Name: p.Volume, Type: "custom", StorageVolumePut: api.StorageVolumePut{Config: api.ConfigMap{ownerCreateTokenKey: token}}}
	if !domain.SafeName(p.Project) || !domain.SafeName(p.Instance) || !domain.SafeName(p.Pool) || !domain.SafeName(p.Volume) || !ownerCreateScope(p.Scope) || len(p.Rendered) == 0 || len(p.Rendered) > 32<<20 || p.Rendered[len(p.Rendered)-1] != 0 {
		return request, nil, domain.ErrPlanStale
	}
	rows := bytes.Split(p.Rendered[:len(p.Rendered)-1], []byte{0})
	if len(rows) < 3 {
		return request, nil, domain.ErrPlanStale
	}
	request.ContentType = string(rows[0])
	separator := 1
	for separator < len(rows) && len(rows[separator]) != 0 {
		key, value, ok := strings.Cut(string(rows[separator]), "=")
		if !ok || !slices.Contains([]string{"size", "user.subyard.storage", "user.subyard.format"}, key) {
			return request, nil, domain.ErrPlanStale
		}
		if _, duplicate := request.Config[key]; duplicate {
			return request, nil, domain.ErrPlanStale
		}
		request.Config[key] = value
		separator++
	}
	if separator >= len(rows) {
		return request, nil, domain.ErrPlanStale
	}
	device := map[string]string{}
	for _, row := range rows[separator+1:] {
		key, value, ok := strings.Cut(string(row), "=")
		if !ok || !slices.Contains([]string{"type", "pool", "source", "path", "io.bus"}, key) {
			return request, nil, domain.ErrPlanStale
		}
		if _, duplicate := device[key]; duplicate {
			return request, nil, domain.ErrPlanStale
		}
		device[key] = value
	}
	if device["type"] != "disk" || device["pool"] != p.Pool || device["source"] != p.Volume {
		return request, nil, domain.ErrPlanStale
	}
	switch request.ContentType {
	case "filesystem":
		if len(request.Config) != 1 || len(device) != 4 || device["path"] != "/srv" {
			return request, nil, domain.ErrPlanStale
		}
	case "block":
		if len(request.Config) != 4 || request.Config["size"] == "" || request.Config["user.subyard.storage"] != "v1" || request.Config["user.subyard.format"] != "pending" || len(device) != 4 || device["io.bus"] != "virtio-scsi" {
			return request, nil, domain.ErrPlanStale
		}
	default:
		return request, nil, domain.ErrPlanStale
	}
	return request, device, nil
}

// ownerVolumeInstance reads only the original instance role. For an originally
// absent instance it requires the explicit successful create ref, never a name
// or token discovered from a fresh listing. The returned ETag is current guard
// data for this one known UUID, not a newly prepared resource scope.
func (client *Client) ownerVolumeInstance(ctx context.Context, server incus.InstanceServer, p ports.PreparedOwnerVolume, original *ports.PreparedOwnerInstance, created *ports.NativeOwnerCreated) (*api.Instance, string, error) {
	identity, identityErr := nativeOperation(server, "owner-create-volume", p.Project, p.Volume)
	if identityErr != nil || p.Server == "" || identity.Server != p.Server {
		return nil, "", domain.ErrPlanStale
	}
	expectedUUID := p.InstanceUUID
	token := ""
	var fixed *api.InstancesPost
	if expectedUUID == "" {
		if original == nil || created == nil || original.Scope != p.Scope || original.Project != p.Project || original.Instance != p.Instance || original.Runtime != p.Runtime || original.Source != p.Source {
			return nil, "", domain.ErrPlanStale
		}
		request, requestErr := client.ownerCreatedInstanceRequest(ctx, *original, *created)
		identity, identityErr := nativeOperation(server, "owner-create-instance", p.Project, p.Instance)
		if requestErr != nil || identityErr != nil || created.Intent.Server != identity.Server || p.Server == "" || p.Server != identity.Server || original.Server != p.Server {
			return nil, "", domain.ErrPlanStale
		}
		expectedUUID, token = created.InstanceUUID, created.Intent.Token
		fixed = &request
	}
	if !domain.SafeID(expectedUUID) {
		return nil, "", domain.ErrPlanStale
	}
	instance, etag, err := server.GetInstance(p.Instance)
	if err != nil || instance == nil || instance.Name != p.Instance || instance.Config["volatile.uuid"] != expectedUUID || etag == "" || token != "" && instance.Config[ownerCreateTokenKey] != token {
		return nil, "", domain.ErrPlanStale
	}
	if fixed != nil && !ownerCreatedInstanceMatches(instance, *fixed, *created) {
		return nil, "", domain.ErrPlanStale
	}
	return instance, etag, nil
}

// CheckOwnerCreatedVolume accepts only the exact retained typed result. Only
// the original target's UsedBy entry may move; foreign references stay bound.
func (client *Client) CheckOwnerCreatedVolume(ctx context.Context, p ports.PreparedOwnerVolume, expected ports.NativeOwnerCreated) error {
	observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerCreateObserver)
	request, _, err := ownerVolumeRequest(p, expected.Intent.Token)
	if !ok || err != nil || !p.Absent || expected.Status != 200 || expected.Ref.Domain != "command" || expected.Intent.Domain != "command" || !ownerImageHex(expected.Intent.Token, 32) || !ownerImageHex(expected.IdentityDigest, 64) || expected.Intent.Kind != "srv-volume" || expected.Intent.Scope != p.Scope || expected.Intent.Project != p.Project || expected.Intent.Pool != p.Pool || expected.Intent.Resource != p.Volume || expected.Intent.Server != p.Server || expected.Intent.RequestDigest != ownerCreateDigest(request) {
		return domain.ErrPlanStale
	}
	retained, err := observer.ReadOwnerCreated(expected.Ref)
	if err != nil || retained != expected {
		return domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	identity, err := nativeOperation(server, "owner-create-volume", p.Project, p.Volume)
	if err != nil || p.Server == "" || identity.Server != p.Server {
		return domain.ErrPlanStale
	}
	actual, etag, err := server.UseProject(p.Project).GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if err != nil || actual == nil || actual.Name != p.Volume || actual.Type != "custom" || actual.ContentType != request.ContentType || actual.Config[ownerCreateTokenKey] != expected.Intent.Token || etag == "" {
		return domain.ErrPlanStale
	}
	if !ownerCreatedVolumeMatches(actual, request, expected, p) {
		return domain.ErrPlanStale
	}
	return nil
}

func ownerCreatedVolumeMatches(actual *api.StorageVolume, request api.StorageVolumesPost, expected ports.NativeOwnerCreated, p ports.PreparedOwnerVolume) bool {
	if actual.Name != request.Name || actual.Type != request.Type || actual.ContentType != request.ContentType {
		return false
	}
	for key, value := range request.Config {
		if key == "user.subyard.format" && (actual.Config[key] == "pending" || actual.Config[key] == "ready") {
			continue
		}
		if actual.Config[key] != value {
			return false
		}
	}
	return ownerVolumeDigestMatches(actual, p, request, expected.IdentityDigest)
}

func ownerVolumeDigestMatches(volume *api.StorageVolume, p ports.PreparedOwnerVolume, request api.StorageVolumesPost, expected string) bool {
	format := request.ContentType == "block" && request.Config["user.subyard.format"] == "pending"
	return expected == ownerVolumeStaticDigest(volume, p.Project, p.Instance, format) || format && expected == ownerVolumeStaticDigest(volume, p.Project, p.Instance, format, true)
}

func ownerVolumeStaticDigest(volume *api.StorageVolume, project, instance string, format bool, futureUUID ...bool) string {
	put := volume.Writable()
	put.Config = maps.Clone(put.Config)
	// The daemon updates exactly these idmap facts when mounting a custom
	// filesystem volume. Other static and volatile configuration stays bound.
	if volume.ContentType == "filesystem" {
		delete(put.Config, "volatile.idmap.last")
		delete(put.Config, "volatile.idmap.next")
	}
	if format {
		marker, uuid := put.Config["user.subyard.format"], put.Config["user.subyard.uuid"]
		if marker == "pending" && uuid != "" || marker == "ready" && !ownerImageSecretUUID(uuid) || marker != "pending" && marker != "ready" {
			return ""
		}
		delete(put.Config, "user.subyard.format")
		// Keep an originally known ready UUID in the initial projection. Only
		// an original pending/no-UUID hash can match this future-output variant.
		if uuid == "" || len(futureUUID) == 1 && futureUUID[0] {
			delete(put.Config, "user.subyard.uuid")
		}
	}
	var foreign []string
	for _, use := range volume.UsedBy {
		if use != "/1.0/instances/"+instance+"?project="+url.QueryEscape(project) && (project != "default" || use != "/1.0/instances/"+instance) {
			foreign = append(foreign, use)
		}
	}
	slices.Sort(foreign)
	return ownerCreateDigest(struct {
		Put                                        api.StorageVolumePut
		Name, Type, ContentType, Project, Location string
		ForeignUses                                []string
		CreatedAt                                  time.Time
	}{put, volume.Name, volume.Type, volume.ContentType, volume.Project, volume.Location, foreign, volume.CreatedAt})
}

func (client *Client) ApplyPreparedOwnerVolume(ctx context.Context, p ports.PreparedOwnerVolume, original *ports.PreparedOwnerInstance, instanceCreated *ports.NativeOwnerCreated) (created *ports.NativeOwnerCreated, result error) {
	observer := ports.NativeOperationObserverFrom(ctx)
	creates, ok := observer.(ports.NativeOwnerCreateObserver)
	if !ok {
		return nil, domain.ErrPlanStale
	}
	if !p.Absent || p.Identity != "" || p.ETag != "" {
		return nil, domain.ErrPlanStale
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	target := server.UseProject(p.Project)
	if err := client.validateServerExtensions(target); err != nil {
		return nil, err
	}
	if !target.HasExtension("storage") {
		return nil, errors.New("original owner storage API unavailable")
	}
	_, instanceETag, err := client.ownerVolumeInstance(ctx, target, p, original, instanceCreated)
	if err != nil {
		return nil, err
	}
	volume, _, err := target.GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if !api.StatusErrorCheck(err, http.StatusNotFound) || volume != nil {
		return nil, domain.ErrPlanStale
	}
	identity, err := nativeOperation(target, "owner-create-volume", p.Project, p.Volume)
	if err != nil {
		return nil, err
	}
	if p.Server == "" || identity.Server != p.Server {
		return nil, domain.ErrPlanStale
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(nonce[:])
	request, _, err := ownerVolumeRequest(p, token)
	if err != nil {
		return nil, err
	}
	_, currentETag, err := client.ownerVolumeInstance(ctx, target, p, original, instanceCreated)
	if err != nil || currentETag != instanceETag {
		return nil, domain.ErrPlanStale
	}
	intent := ports.NativeOwnerCreateIntent{Scope: p.Scope, Kind: "srv-volume", Domain: "command", Token: token, RequestDigest: ownerCreateDigest(request), Server: identity.Server, Project: p.Project, Resource: p.Volume, Pool: p.Pool}
	ref, err := creates.BeforeOwnerCreate(intent)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			observer.Uncertain()
		}
	}()
	response, _, err := target.RawQuery(http.MethodPost, "/1.0/storage-pools/"+url.PathEscape(p.Pool)+"/volumes/custom", request, "")
	if err != nil {
		return nil, normalizeError("create original owner SRV volume", err)
	}
	if response == nil || response.Type != api.SyncResponse || response.StatusCode != int(api.Success) || response.Operation != "" {
		return nil, errors.New("owner SRV sync response unavailable")
	}
	if err := observer.CommandTerminal(ref.Index); err != nil {
		return nil, err
	}
	volume, etag, err := target.GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if err != nil || volume == nil || volume.Name != p.Volume || volume.Type != "custom" || volume.ContentType != request.ContentType || volume.Config[ownerCreateTokenKey] != token || etag == "" {
		return nil, errors.New("owner SRV successful create identity unavailable")
	}
	for key, value := range request.Config {
		if volume.Config[key] != value {
			return nil, errors.New("owner SRV initial configuration differs")
		}
	}
	value := ports.NativeOwnerCreated{Ref: ref, Intent: intent, Status: 200, IdentityDigest: ownerVolumeStaticDigest(volume, p.Project, p.Instance, request.ContentType == "block" && request.Config["user.subyard.format"] == "pending"), ETagDigest: ownerCreateDigest(etag)}
	if err := creates.RecordOwnerCreated(value); err != nil {
		return nil, err
	}
	return &value, nil
}

// AttachPreparedOwnerVolume changes only the original fixed srv device using
// the current ETag of the already authenticated original/created instance UUID.
func (client *Client) AttachPreparedOwnerVolume(ctx context.Context, p ports.PreparedOwnerVolume, original *ports.PreparedOwnerInstance, instanceCreated *ports.NativeOwnerCreated, volumeCreated *ports.NativeOwnerCreated) error {
	_, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerCreateObserver)
	if !ok {
		return domain.ErrPlanStale
	}
	token := strings.Repeat("0", 32)
	if p.Absent {
		if volumeCreated == nil {
			return domain.ErrPlanStale
		}
		token = volumeCreated.Intent.Token
	} else if volumeCreated != nil {
		return domain.ErrPlanStale
	}
	_, device, err := ownerVolumeRequest(p, token)
	if err != nil {
		return err
	}
	if p.Absent {
		if err := client.CheckOwnerCreatedVolume(ctx, p, *volumeCreated); err != nil {
			return domain.ErrPlanStale
		}
	} else if err := client.CheckOwnerVolumeGuard(ctx, p); err != nil {
		return err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	target := server.UseProject(p.Project)
	identity, err := nativeOperation(target, "owner-create-volume", p.Project, p.Volume)
	if err != nil || p.Server == "" || identity.Server != p.Server || p.Absent && identity.Server != volumeCreated.Intent.Server {
		return domain.ErrPlanStale
	}
	volume, etag, err := target.GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if err != nil || volume == nil {
		return domain.ErrPlanStale
	}
	if p.Absent {
		request, _, requestErr := ownerVolumeRequest(p, volumeCreated.Intent.Token)
		if requestErr != nil || etag == "" || !ownerCreatedVolumeMatches(volume, request, *volumeCreated, p) {
			return domain.ErrPlanStale
		}
	} else {
		request, _, requestErr := ownerVolumeRequest(p, strings.Repeat("0", 32))
		if requestErr != nil || etag == "" || !ownerVolumeDigestMatches(volume, p, request, p.Identity) {
			return domain.ErrPlanStale
		}
	}
	instance, instanceETag, err := client.ownerVolumeInstance(ctx, target, p, original, instanceCreated)
	if err != nil {
		return err
	}
	if current, exists := instance.ExpandedDevices["srv"]; exists && !reflect.DeepEqual(current, device) {
		return domain.ErrPlanStale
	}
	if current, exists := instance.Devices["srv"]; exists {
		if reflect.DeepEqual(current, device) {
			return nil
		}
		return domain.ErrPlanStale
	}
	put := instance.Writable()
	put.Devices = maps.Clone(instance.Devices)
	if put.Devices == nil {
		put.Devices = map[string]map[string]string{}
	}
	put.Devices["srv"] = maps.Clone(device)
	if err := beforeNative(ctx, target, "instance-devices", p.Project, p.Instance); err != nil {
		return err
	}
	operation, err := target.UpdateInstance(p.Instance, put, instanceETag)
	if err != nil {
		ports.NativeOperationObserverFrom(ctx).Uncertain()
		return normalizeError("attach original owner SRV device", err)
	}
	return waitNative(ctx, target, operation, "instance-devices", p.Project, p.Instance)
}

func (client *Client) PrepareOwnerVolumeGuard(ctx context.Context, p ports.PreparedOwnerVolume) (ports.PreparedOwnerVolume, error) {
	request, _, err := ownerVolumeRequest(p, strings.Repeat("0", 32))
	if err != nil {
		return p, err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return p, err
	}
	target := server.UseProject(p.Project)
	if err := client.validateServerExtensions(target); err != nil {
		return p, err
	}
	identity, err := nativeOperation(target, "owner-create-volume", p.Project, p.Volume)
	if err != nil || p.Server != "" && identity.Server != p.Server {
		return p, domain.ErrPlanStale
	}
	p.Server = identity.Server
	volume, etag, err := target.GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if p.Absent {
		if !api.StatusErrorCheck(err, http.StatusNotFound) || volume != nil {
			return p, domain.ErrPlanStale
		}
		return p, nil
	}
	if err != nil || volume == nil || volume.Name != p.Volume || volume.Type != "custom" || volume.ContentType != request.ContentType || etag == "" {
		return p, domain.ErrPlanStale
	}
	p.Identity, p.ETag = ownerVolumeStaticDigest(volume, p.Project, p.Instance, request.ContentType == "block" && request.Config["user.subyard.format"] == "pending"), ownerCreateDigest(etag)
	if p.Identity == "" {
		return p, domain.ErrPlanStale
	}
	return p, nil
}
func (client *Client) CheckOwnerVolumeGuard(ctx context.Context, p ports.PreparedOwnerVolume) error {
	request, _, err := ownerVolumeRequest(p, strings.Repeat("0", 32))
	if err != nil {
		return err
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	target := server.UseProject(p.Project)
	if err := client.validateServerExtensions(target); err != nil {
		return err
	}
	identity, err := nativeOperation(target, "owner-create-volume", p.Project, p.Volume)
	if err != nil || p.Server == "" || identity.Server != p.Server {
		return domain.ErrPlanStale
	}
	volume, etag, err := target.GetStoragePoolVolume(p.Pool, "custom", p.Volume)
	if p.Absent {
		if !api.StatusErrorCheck(err, http.StatusNotFound) || volume != nil {
			return domain.ErrPlanStale
		}
		return nil
	}
	if err != nil || volume == nil || volume.Name != p.Volume || volume.Type != "custom" || volume.ContentType != request.ContentType || etag == "" || !ownerImageHex(p.Identity, 64) || !ownerVolumeDigestMatches(volume, p, request, p.Identity) {
		return domain.ErrPlanStale
	}
	return nil
}

func (client *Client) CheckPreparedOwnerVolumeInstance(ctx context.Context, p ports.PreparedOwnerVolume, original *ports.PreparedOwnerInstance, created *ports.NativeOwnerCreated) error {
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	_, _, err = client.ownerVolumeInstance(ctx, server.UseProject(p.Project), p, original, created)
	return err
}

// OwnerCreateServer captures only the existing target connection identity. It
// does not claim a daemon UUID or socket-inode fence and chooses no endpoint.
func (client *Client) OwnerCreateServer(ctx context.Context) (string, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return "", err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return "", err
	}
	identity, err := nativeOperation(server, "owner-create", "", "")
	return identity.Server, err
}
