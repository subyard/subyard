// Package observation owns AI Observer's native schema and protected projection.
package observation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ownerroute"
	"io"
	"net"
	"strconv"
	"strings"

	semantic "github.com/Subyard/Subyard/internal/observation"
)

const provisionKey = "user.subyard.ai_observer_provision"
const proxyKey = "user.subyard.ai_observer_proxy"

type Failure struct{ Reason string }

func (failure *Failure) Error() string { return failure.Reason }
func fail(reason string) error         { return &Failure{Reason: reason} }

type Request struct {
	Snapshot    string            `json:"snapshot"`
	Container   string            `json:"container"`
	Service     string            `json:"service"`
	Installed   bool              `json:"installed"`
	Selected    bool              `json:"selected"`
	OwnerHost   string            `json:"owner_host"`
	Environment map[string]string `json:"environment"`
	Provision   []byte            `json:"provision"`
}
type RuntimeObservation struct {
	State   string `json:"state"`
	Actual  string `json:"actual"`
	Desired string `json:"desired"`
}
type Result struct {
	Observation RuntimeObservation `json:"observation"`
	Context     string             `json:"context"`
	Frontend    string             `json:"frontend"`
	Container   *Identity          `json:"container"`
}

// Identity is also returned by the retirement recheck through the same native
// constructor. Its projected snapshot remains private to this package.
type Identity struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	guard   semantic.Value
}

func Container(data []byte) (*Identity, error) {
	raw, err := semantic.Decode(data)
	if err != nil {
		return nil, fail("invalid-schema")
	}
	if raw.IsNull() {
		return nil, nil
	}
	containers, ok := raw.Elements()
	if !ok || len(containers) != 1 {
		return nil, fail("ambiguous-ownership")
	}
	container, ok := containers[0].Fields()
	if !ok {
		return nil, fail("invalid-schema")
	}
	name, ok := container["Name"].Text()
	if !ok {
		return nil, fail("invalid-schema")
	}
	if name != "/subyard-ai-observer" {
		return nil, fail("foreign-ownership")
	}
	id, idOK := container["Id"].Text()
	image, imageOK := container["Image"].Text()
	config, configOK := container["Config"].Fields()
	host, hostOK := container["HostConfig"].Fields()
	state, stateOK := container["State"].Fields()
	running, runningOK := state["Running"].Boolean()
	mounts, mountsOK := container["Mounts"].Elements()
	if !idOK || id == "" || !imageOK || image == "" || !configOK || !hostOK || !stateOK || !runningOK || !mountsOK {
		return nil, fail("invalid-schema")
	}
	labels, labelsOK := config["Labels"].Fields()
	owner, ownerOK := labels["org.subyard.managed"].Text()
	if !labelsOK || !ownerOK || owner != "ai-observer-v1" {
		return nil, fail("foreign-ownership")
	}
	projected := map[string]semantic.Value{}
	for _, key := range []string{"Image", "User", "Cmd", "Env", "Labels", "Entrypoint", "WorkingDir", "Volumes", "ExposedPorts"} {
		value := config[key]
		if !validConfig(key, value) {
			return nil, fail("invalid-schema")
		}
		projected[key] = ordered(value)
	}
	if status, exists := state["Status"]; exists && !status.IsNull() {
		if _, ok := status.Text(); !ok {
			return nil, fail("invalid-schema")
		}
	}
	// Only this constructor chooses Mounts semantics. The complete records,
	// including unknown properties and duplicate multiplicity, are protected.
	mountValues := make([]semantic.Value, len(mounts))
	for i, mount := range mounts {
		fields, ok := mount.Fields()
		if !ok {
			return nil, fail("invalid-schema")
		}
		for key, value := range fields {
			switch key {
			case "Type", "Name", "Source", "Destination", "Driver", "Mode", "Propagation":
				if _, ok := value.Text(); !ok {
					return nil, fail("invalid-schema")
				}
			case "RW":
				if _, ok := value.Boolean(); !ok {
					return nil, fail("invalid-schema")
				}
			}
		}
		mountValues[i] = ordered(mount)
	}
	return &Identity{ID: id, Running: running, guard: semantic.Object(map[string]semantic.Value{
		"id": semantic.String(id), "image": semantic.String(image), "config": semantic.Object(projected),
		"host_config": objectValue(host), "mounts": semantic.Multiset(mountValues),
		"running": semantic.Bool(running), "status": ordered(state["Status"]),
	})}, nil
}

// ordered preserves the existing order of all other native arrays. It cannot be
// used to construct the Mounts field outside Container.
func ordered(raw semantic.RawValue) semantic.Value {
	if scalar, ok := raw.Scalar(); ok {
		return scalar
	}
	if fields, ok := raw.Fields(); ok {
		values := make(map[string]semantic.Value, len(fields))
		for key, value := range fields {
			values[key] = ordered(value)
		}
		return semantic.Object(values)
	}
	items, _ := raw.Elements()
	values := make([]semantic.Value, len(items))
	for i, item := range items {
		values[i] = ordered(item)
	}
	return semantic.Ordered(values)
}

func objectValue(fields map[string]semantic.RawValue) semantic.Value {
	values := make(map[string]semantic.Value, len(fields))
	for key, value := range fields {
		values[key] = ordered(value)
	}
	return semantic.Object(values)
}

func validConfig(key string, raw semantic.RawValue) bool {
	if raw.IsNull() {
		return true
	}
	switch key {
	case "Image", "User", "WorkingDir":
		_, ok := raw.Text()
		return ok
	case "Cmd", "Env", "Entrypoint":
		items, ok := raw.Elements()
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.Text(); !ok {
				return false
			}
		}
		return true
	case "Labels":
		fields, ok := raw.Fields()
		if !ok {
			return false
		}
		for _, value := range fields {
			if _, ok := value.Text(); !ok {
				return false
			}
		}
		return true
	case "Volumes", "ExposedPorts":
		_, ok := raw.Fields()
		return ok
	}
	return false
}

func Observe(request Request) (Result, error) {
	raw, err := semantic.Decode([]byte(request.Snapshot))
	if err != nil {
		return Result{}, fail("invalid-schema")
	}
	snapshot, ok := raw.Fields()
	if !ok {
		return Result{}, fail("invalid-schema")
	}
	localConfig, err := nativeObject(snapshot, "config", nil)
	if err != nil {
		return Result{}, err
	}
	localDevices, err := nativeObject(snapshot, "devices", nil)
	if err != nil {
		return Result{}, err
	}
	config, err := nativeObject(snapshot, "expanded_config", localConfig)
	if err != nil {
		return Result{}, err
	}
	devices, err := nativeObject(snapshot, "expanded_devices", localDevices)
	if err != nil {
		return Result{}, err
	}
	provision, err := receipt(config, provisionKey)
	if err != nil {
		return Result{}, err
	}
	localProvision, err := receipt(localConfig, provisionKey)
	if err != nil {
		return Result{}, err
	}
	if provision != localProvision {
		return Result{}, fail("ambiguous-ownership")
	}
	marker, err := receipt(config, proxyKey)
	if err != nil {
		return Result{}, err
	}
	localMarker, err := receipt(localConfig, proxyKey)
	if err != nil {
		return Result{}, err
	}
	proxy, proxyExists := devices["ai-observer"]
	localProxy, localProxyExists := localDevices["ai-observer"]
	if marker != localMarker || proxyExists != localProxyExists || !equal(ordered(proxy), ordered(localProxy)) {
		return Result{}, fail("ambiguous-ownership")
	}
	deferred, err := receipt(localConfig, ownerpublication.Prefix+"ai-observer")
	if err != nil {
		return Result{}, err
	}
	if deferred != "" {
		var saved ownerpublication.Receipt
		decoder := json.NewDecoder(bytes.NewBufferString(deferred))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&saved) != nil || decoder.Decode(new(any)) != io.EOF || saved.Version != 1 || saved.Ownership != marker {
			return Result{}, fail("ambiguous-ownership")
		}
		host, _, _ := ownerroute.Owned(marker, saved.Device, 8080)
		if host == "" || (proxyExists && !proxy.IsNull()) {
			return Result{}, fail("ambiguous-ownership")
		}
		payload, _ := json.Marshal(saved.Device)
		proxy, err = semantic.Decode(payload)
		if err != nil {
			return Result{}, fail("invalid-schema")
		}
		proxyExists = true
	}
	host, port, ready := "", "", false
	if proxyExists && !proxy.IsNull() {
		version, value, separated := strings.Cut(marker, ":")
		pending := strings.HasPrefix(value, "pending:")
		value = strings.TrimPrefix(value, "pending:")
		switch version {
		case "v1":
			host, port = "127.0.0.1", value
		case "v2":
			host, port, _ = strings.Cut(value, ":")
			ip := net.ParseIP(host)
			octets := strings.Split(host, ".")
			if ip == nil || ip.To4() == nil || len(octets) != 4 || ip.String() != host || ip.To4()[0] != 100 || ip.To4()[1] < 64 || ip.To4()[1] > 127 {
				return Result{}, fail("ambiguous-ownership")
			}
		default:
			return Result{}, fail("ambiguous-ownership")
		}
		number, parseErr := strconv.Atoi(port)
		if !separated || parseErr != nil || strconv.Itoa(number) != port || number < 1024 || number > 65535 {
			return Result{}, fail("ambiguous-ownership")
		}
		wanted := semantic.Object(map[string]semantic.Value{"type": semantic.String("proxy"), "bind": semantic.String("host"), "listen": semantic.String("tcp:" + host + ":" + port), "connect": semantic.String("tcp:127.0.0.1:8080")})
		if !equal(ordered(proxy), wanted) {
			return Result{}, fail("foreign-ownership")
		}
		ready = !pending
	}
	frontend, context := "", ""
	if request.Selected {
		frontendHost := "127.0.0.1"
		if request.Environment["YARD_KIND"] != "vm" {
			frontendHost = request.OwnerHost
			if ready && host != "" {
				frontendHost = host
			}
		}
		if net.ParseIP(frontendHost) == nil {
			return Result{}, fail("native-unavailable")
		}
		frontend = "http://" + frontendHost + ":" + request.Environment["AI_OBSERVER_HOST_PORT"]
		context = contextDigest(request, frontend)
	}
	container, err := Container([]byte(request.Container))
	if err != nil {
		return Result{}, err
	}
	if container != nil && request.Service == "" {
		return Result{}, fail("ambiguous-ownership")
	}
	desired, err := fingerprint(semantic.Object(map[string]semantic.Value{"selected": semantic.Bool(request.Selected), "context": semantic.String(context), "frontend": semantic.String(frontend), "source": semantic.String(request.Environment["SUBYARD_INTEGRATION_SOURCE_BINDING"])}))
	if err != nil {
		return Result{}, err
	}
	current := false
	if request.Selected {
		routeCurrent := ready && frontend == "http://"+host+":"+port
		if request.Environment["YARD_KIND"] == "vm" {
			routeCurrent = marker == "" && (!proxyExists || proxy.IsNull())
		}
		current = request.Installed && provision == context && routeCurrent
	} else {
		current = marker == "" && (!proxyExists || proxy.IsNull()) && provision == "" && !strings.Contains("\n"+request.Service, "\nactive\n") && !strings.Contains("\n"+request.Service, "\nenabled\n") && (container == nil || !container.Running)
		if current && request.Service == "" {
			return Result{Observation: RuntimeObservation{State: "absent"}}, nil
		}
	}
	result := Result{Observation: RuntimeObservation{State: "current", Actual: desired, Desired: desired}, Context: context, Frontend: frontend, Container: container}
	if !current {
		containerValue := semantic.Null()
		if container != nil {
			containerValue = container.guard
		}
		actual, err := fingerprint(semantic.Object(map[string]semantic.Value{"provision": semantic.String(provision), "proxy": semantic.String(marker), "device": ordered(proxy), "service": semantic.String(request.Service), "installation": semantic.Bool(request.Installed), "container": containerValue}))
		if err != nil {
			return Result{}, err
		}
		result.Observation = RuntimeObservation{State: "stale", Actual: actual, Desired: desired}
	}
	return result, nil
}

func nativeObject(fields map[string]semantic.RawValue, key string, fallback map[string]semantic.RawValue) (map[string]semantic.RawValue, error) {
	value, exists := fields[key]
	if !exists {
		if fallback == nil {
			return map[string]semantic.RawValue{}, nil
		}
		return fallback, nil
	}
	object, ok := value.Fields()
	if !ok {
		return nil, fail("invalid-schema")
	}
	return object, nil
}
func receipt(fields map[string]semantic.RawValue, key string) (string, error) {
	value, exists := fields[key]
	if !exists {
		return "", nil
	}
	text, ok := value.Text()
	if !ok {
		return "", fail("invalid-schema")
	}
	return text, nil
}
func equal(left, right semantic.Value) bool {
	a, err := semantic.NewSnapshot(left)
	if err != nil {
		return false
	}
	b, err := semantic.NewSnapshot(right)
	return err == nil && string(a.Bytes()) == string(b.Bytes())
}
func fingerprint(value semantic.Value) (string, error) {
	snapshot, err := semantic.NewSnapshot(value)
	if err != nil {
		return "", fail("invalid-schema")
	}
	digest, err := semantic.Fingerprint(snapshot)
	if err != nil {
		return "", fail("invalid-schema")
	}
	return digest.String(), nil
}
func contextDigest(request Request, frontend string) string {
	get := func(key, fallback string) string {
		value, exists := request.Environment[key]
		if !exists {
			return fallback
		}
		return value
	}
	fields := []string{"v1", semantic.DigestSource(request.Provision).String(), get("DEV_USER", "dev"), get("DEV_UID", "1000"), frontend}
	integrations := strings.Fields(request.Environment["CODING_TOOL_INTEGRATIONS"])
	for _, wanted := range []string{"claude", "codex"} {
		found := false
		for _, integration := range integrations {
			found = found || integration == wanted
		}
		fields = append(fields, strconv.FormatBool(found))
	}
	for _, key := range []string{"HOST_BASE", "HOST_LINKS", "HOST_MOUNTS", "SUBYARD_INTEGRATION_SOURCE_BINDING"} {
		fields = append(fields, request.Environment[key])
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(fields, "\x00")+"\x00")))
}

func Reason(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Reason
	}
	return "invalid-schema"
}
