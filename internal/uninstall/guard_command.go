package uninstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

// RunGuard is the hidden `_teardown-guard` boundary of shell and profile
// adapters. It prints fixed refusals only, never plans, facts or bindings.
func RunGuard(arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	mode := ""
	if len(arguments) > 0 {
		mode, arguments = arguments[0], arguments[1:]
	}
	switch mode {
	case "remove-files":
		return refusalExit(stderr, removeFiles(), "full uninstall could not verify exact file removal; retain the saved recovery plan and retry")
	case "remove-shared":
		return refusalExit(stderr, removeShared(slices.Equal(arguments, []string{"--selected"}), stdout), "full uninstall shared ownership/consumer verification failed; preserve backing data and resume the exact saved plan")
	case "check-profiles":
		return refusalExit(stderr, checkProfiles(arguments, stderr), "full uninstall conditional ownership check failed; preserve dependent resources and data")
	case "files-need-root":
		if len(arguments) == 1 && filesNeedRoot(arguments[0]) {
			return 0
		}
		return 1
	}
	err := guardMode(mode, arguments, stdin, stdout, stderr)
	if err == nil {
		return 0
	}
	if errors.Is(err, os.ErrPermission) {
		if mode == "capture-tree" || mode == "capture-artifact" {
			return 77
		}
		return ports.TeardownPlanStaleExitCode
	}
	fmt.Fprintln(stderr, "plan_stale: teardown inventory, artifact or resource binding is invalid")
	if slices.Contains(staleModes, mode) {
		return ports.TeardownPlanStaleExitCode
	}
	return 1
}

func refusalExit(stderr io.Writer, err error, message string) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, message)
	return ports.TeardownPlanStaleExitCode
}

func argument(arguments []string, index int) (string, error) {
	if index >= len(arguments) {
		return "", errors.New("missing teardown guard argument")
	}
	return arguments[index], nil
}

func guardMode(mode string, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch mode {
	case "capture-producer":
		if len(arguments) != 3 {
			return errors.New("usage: capture-producer OWNER PORT ROOT")
		}
		port, portErr := strconv.Atoi(arguments[1])
		root, rootErr := filepath.Abs(arguments[2])
		home, ok := os.LookupEnv("SUBYARD_HOME")
		operator, userErr := producerOwner()
		if err := errors.Join(portErr, rootErr, userErr); err != nil || !ok {
			return errors.Join(err, errors.New("invalid producer capture context"))
		}
		runtime := os.Getenv("YARD_RUNTIME_ROOT")
		if runtime == "" {
			runtime = home + "/runtime"
		}
		services, err := coreProducers(root, Installation{OperatorUID: operator, DataHome: home, RuntimeRoot: runtime}, processEnvironment(), []string{arguments[0]}, nil, port, ProducerEvidence{})
		if err != nil {
			return err
		}
		return printJSON(stdout, services)
	case "producer-artifacts-json":
		services, err := approvedServices(nil)
		if err != nil {
			return err
		}
		artifacts, err := ownerArtifacts(services)
		if err != nil {
			return err
		}
		return printJSON(stdout, artifacts)
	case "capture-artifact":
		body, err := io.ReadAll(io.LimitReader(stdin, 65537))
		if err != nil {
			return err
		}
		var item struct{ Path string }
		if err := json.Unmarshal(body, &item); err != nil {
			return err
		}
		artifact, err := captureArtifact(item.Path)
		if err != nil {
			return err
		}
		return printJSON(stdout, artifact)
	case "capture-tree":
		body, err := io.ReadAll(io.LimitReader(stdin, 64<<20+1))
		if err != nil {
			return err
		}
		var tree Tree
		if len(body) > 64<<20 {
			return guardRefusal("oversized installation artifact")
		}
		if err := json.Unmarshal(body, &tree); err != nil {
			return err
		}
		captured, err := tree.capture()
		if err != nil {
			return err
		}
		return printJSON(stdout, captured)
	case "service-inventory", "producer-artifacts":
		plan, err := optionalPlan()
		if err != nil {
			return err
		}
		services, err := approvedServices(plan)
		if err != nil {
			return err
		}
		if mode == "service-inventory" {
			for _, service := range services {
				for _, unit := range service.Units {
					fmt.Fprintf(stdout, "%s\t%s\n", service.Scope, unit)
				}
			}
			return nil
		}
		artifacts, err := ownerArtifacts(services)
		for _, artifact := range artifacts {
			fmt.Fprintln(stdout, artifact.Path)
		}
		return err
	case "service-fragment":
		fragment, err := argument(arguments, 0)
		if err != nil {
			return err
		}
		approved, err := approvedArtifacts()
		if err != nil {
			return err
		}
		if fragment == "" || !slices.ContainsFunc(approved, func(item ports.TeardownArtifact) bool { return item.Path == fragment }) {
			return guardRefusal("approved unit name resolves to an unapproved artifact")
		}
		return nil
	case "remove-artifact-as":
		// Root keeps the pinned guard; the operator's credentials decide whether
		// its directory entry may be deleted at all.
		account, err := argument(arguments, 0)
		if err != nil {
			return err
		}
		path, err := argument(arguments, 1)
		if err != nil {
			return err
		}
		approved, err := approvedArtifacts()
		if err != nil {
			return err
		}
		return asAccount(account, func() error { return guardSelected(approved, path, func() error { return nil }) })
	case "guard-artifacts", "guard-artifact", "remove-artifact":
		approved, err := approvedArtifacts()
		if err != nil || mode == "guard-artifacts" {
			for _, item := range approved {
				if err == nil {
					err = guardArtifact(item.Path, item.Binding, item.Entries, nil)
				}
			}
			return err
		}
		path, err := argument(arguments, 0)
		if err != nil {
			return err
		}
		var before func() error
		if mode == "remove-artifact" {
			before = func() error { return nil }
		}
		return guardSelected(approved, path, before)
	case "guard-all":
		project, _ := argument(arguments, 0)
		stage, kind, ordinal := "inventory", "none", 0
		resources, err := inventory()
		if err == nil {
			stage = "project"
			if project == "" || project == "default" {
				err = guardRefusal("teardown requires a dedicated project")
			}
		}
		for index := 0; err == nil && index < len(resources); index++ {
			stage, kind, ordinal = "resource", resources[index].Kind, index+1
			err = guardResource(resources[index], project)
		}
		if err != nil {
			fmt.Fprintln(stderr, "plan_stale: "+guardAllDetails(stage, kind, ordinal, err))
		}
		return err
	}
	resources, err := inventory()
	if err != nil {
		return err
	}
	switch mode {
	case "validate":
		return nil
	case "volumes":
		for _, resource := range resources {
			if resource.Kind != "volume" {
				continue
			}
			if resource.Pool == "" {
				return missingField("pool")
			}
			fmt.Fprintf(stdout, "%s\t%s\n", resource.Pool, resource.Name)
		}
		return nil
	case "list", "guard":
		kind, err := argument(arguments, 0)
		if err != nil {
			return err
		}
		if mode == "list" {
			for _, resource := range resources {
				if resource.Kind == kind {
					fmt.Fprintln(stdout, resource.Name)
				}
			}
			return nil
		}
		if len(arguments) < 3 {
			return errors.New("usage: guard KIND NAME PROJECT [POOL]")
		}
		name, project, pool := arguments[1], arguments[2], ""
		if len(arguments) > 3 {
			pool = arguments[3]
		}
		if project == "" || project == "default" {
			return guardRefusal("teardown requires a dedicated project")
		}
		var matches []ports.TeardownResource
		for _, resource := range resources {
			if resource.Kind == kind && resource.Name == name && resource.Pool == pool {
				matches = append(matches, resource)
			}
		}
		if len(matches) != 1 {
			return guardRefusal("teardown resource was not approved")
		}
		return guardResource(matches[0], project)
	}
	return guardRefusal("invalid teardown guard mode")
}

func printJSON(stdout io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err == nil {
		_, err = fmt.Fprintf(stdout, "%s\n", body)
	}
	return err
}

func processEnvironment() map[string]string {
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			environment[name] = value
		}
	}
	return environment
}

// readPlan binds the protected full plan to its approved digest and owner.
func readPlan() (*RecoveryPlan, error) {
	path := os.Getenv("SUBYARD_TEARDOWN_PLAN")
	parent, name, err := pinParent(path)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, guardRefusal("changed or unsafe full uninstall plan")
	}
	body, err := io.ReadAll(io.LimitReader(file, 64<<20+1))
	if err != nil {
		return nil, err
	}
	plan := &RecoveryPlan{}
	if err := json.Unmarshal(body, plan); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if len(body) > 64<<20 || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 || info.Uid != plan.Installation.OperatorUID || hex.EncodeToString(sum[:]) != os.Getenv("SUBYARD_TEARDOWN_PLAN_SHA256") {
		return nil, guardRefusal("changed or unsafe full uninstall plan")
	}
	return plan, nil
}

func optionalPlan() (*RecoveryPlan, error) {
	if os.Getenv("SUBYARD_TEARDOWN_PLAN") == "" {
		return nil, nil
	}
	return readPlan()
}

// decodeExact decodes a JSON list whose members carry exactly one allowed key set.
func decodeExact(payload []byte, target any, refusal guardRefusal, allowed ...[]string) error {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return err
	}
	for _, item := range raw {
		// The released helper refused null artifact identities.
		if string(item["Path"]) == "null" || string(item["Binding"]) == "null" {
			return refusal
		}
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if !slices.ContainsFunc(allowed, func(set []string) bool { return slices.Equal(set, keys) }) {
			return refusal
		}
	}
	return json.Unmarshal(payload, target)
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:+-]*\.(?:service|timer|socket)$`)

func approvedServices(plan *RecoveryPlan) ([]RecoveryService, error) {
	var services []RecoveryService
	if plan != nil {
		services = plan.Services
	} else if value, ok := os.LookupEnv("SUBYARD_TEARDOWN_SERVICES"); !ok {
		return nil, errors.New("approved service inventory is missing")
	} else if err := decodeExact([]byte(value), &services, "invalid approved service scope", []string{"Artifacts", "scope", "units"}); err != nil {
		return nil, err
	}
	if services == nil || len(services) > 128 {
		return nil, guardRefusal("invalid approved service inventory")
	}
	for _, service := range services {
		if service.Scope != "system" && service.Scope != "user" {
			return nil, guardRefusal("invalid approved service scope")
		}
		if service.Units == nil || len(service.Units) > 4 {
			return nil, guardRefusal("invalid approved service units")
		}
		for _, unit := range service.Units {
			if !unitName.MatchString(unit) {
				return nil, guardRefusal("invalid approved service identity")
			}
		}
	}
	return services, nil
}

func ownerArtifacts(services []RecoveryService, snapshots ...[]ports.TeardownArtifact) ([]ports.TeardownArtifact, error) {
	groups := [][]ports.TeardownArtifact{}
	for _, service := range services {
		groups = append(groups, service.Artifacts)
	}
	result := []ports.TeardownArtifact{}
	for _, group := range append(groups, snapshots...) {
		for _, artifact := range group {
			if artifact.Path == "" {
				return nil, guardRefusal("invalid approved artifact path")
			}
			index := slices.IndexFunc(result, func(item ports.TeardownArtifact) bool { return item.Path == artifact.Path })
			if index < 0 {
				result = append(result, artifact)
			} else if !reflect.DeepEqual(result[index], artifact) {
				return nil, guardRefusal("conflicting approved artifact facts")
			}
		}
	}
	return result, nil
}

// producerOwner keeps the historical SUBYARD_USER, SUDO_USER, caller order.
func producerOwner() (uint32, error) {
	name := os.Getenv("SUBYARD_USER")
	if name == "" {
		name = os.Getenv("SUDO_USER")
	}
	if name == "" {
		current, err := user.LookupId(strconv.Itoa(os.Getuid()))
		if err != nil {
			return 0, err
		}
		name = current.Username
	}
	account, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	return uint32(uid), err
}

// approvedArtifacts resolves bindings from the bounded transport or the
// protected plan; a full plan never accepts facts outside its originals.
func approvedArtifacts() ([]ports.TeardownArtifact, error) {
	plan, err := optionalPlan()
	if err != nil {
		return nil, err
	}
	value, present := os.LookupEnv("SUBYARD_TEARDOWN_ARTIFACTS")
	payload := []byte(value)
	if !present && plan != nil {
		services, err := approvedServices(plan)
		if err != nil {
			return nil, err
		}
		artifacts, err := ownerArtifacts(services)
		if err != nil {
			return nil, err
		}
		// The round trip is the field validation: tree-only facts refuse.
		if payload, err = json.Marshal(artifacts); err != nil {
			return nil, err
		}
	} else if !present || len(payload) > 65536 {
		return nil, guardRefusal("missing or oversized teardown artifacts")
	}
	var approved []ports.TeardownArtifact
	if err := decodeExact(payload, &approved, "invalid teardown artifact fields", []string{"Binding", "Path"}, []string{"Binding", "Entries", "Path"}); err != nil {
		return nil, err
	}
	if approved == nil {
		return nil, guardRefusal("invalid teardown artifacts")
	}
	if err := validateArtifacts(approved); err != nil {
		return nil, err
	}
	if plan == nil {
		return approved, nil
	}
	snapshots := [][]ports.TeardownArtifact{}
	for _, yard := range plan.Yards {
		if yard.Snapshot == nil {
			return nil, errors.New("approved yard snapshot is missing")
		}
		snapshots = append(snapshots, yard.Snapshot.Artifacts)
	}
	original, err := ownerArtifacts(plan.Services, snapshots...)
	if err != nil {
		return nil, err
	}
	for index, item := range approved {
		saved := slices.IndexFunc(original, func(artifact ports.TeardownArtifact) bool { return artifact.Path == item.Path })
		if saved < 0 || original[saved].Binding != item.Binding {
			return nil, guardRefusal("artifact is outside original full plan")
		}
		approved[index] = original[saved]
	}
	return approved, nil
}

var (
	resourceIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	resourceKinds    = []string{"instance", "profile", "image", "volume", "network", "pool", "project", "host-profile"}
	resourceFields   = []string{"binding", "bindings", "kind", "name", "owner", "path", "pool", "usedBy"}
)

// inventoryText reads a text field; an absent field is empty, and another
// JSON type (null too, unless nullable) keeps the historical TypeError category.
func inventoryText(fields map[string]json.RawMessage, key string, nullable bool) (string, error) {
	raw, present := fields[key]
	if !present || nullable && string(raw) == "null" {
		return "", nil
	}
	var value string
	if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return "", typeMismatch("teardown resource " + key + " is not text")
	}
	return value, nil
}

func inventory() ([]ports.TeardownResource, error) {
	value := os.Getenv("SUBYARD_TEARDOWN_INVENTORY")
	if len(value) > 65536 {
		return nil, guardRefusal("oversized teardown inventory")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, err
	}
	if raw == nil || len(raw) > 256 {
		return nil, guardRefusal("invalid teardown inventory")
	}
	resources := make([]ports.TeardownResource, len(raw))
	seen := map[[3]string]bool{}
	for index, body := range raw {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			return nil, typeMismatch("teardown resource is not an object")
		}
		for key := range fields {
			if !slices.Contains(resourceFields, key) {
				return nil, guardRefusal("invalid teardown resource fields")
			}
		}
		if kind, err := inventoryText(fields, "kind", true); err != nil || !slices.Contains(resourceKinds, kind) {
			return nil, guardRefusal("invalid teardown resource kind")
		}
		name, err := inventoryText(fields, "name", false)
		if err != nil {
			return nil, err
		}
		pool, err := inventoryText(fields, "pool", true)
		if err != nil {
			return nil, err
		}
		if !resourceIdentity.MatchString(name) || pool != "" && !resourceIdentity.MatchString(pool) {
			return nil, guardRefusal("invalid teardown resource identity")
		}
		binding, err := inventoryText(fields, "binding", false)
		if err != nil {
			return nil, err
		}
		if !hexDigest(binding) {
			return nil, guardRefusal("invalid teardown binding")
		}
		resource := &resources[index]
		if err := json.Unmarshal(body, resource); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				return nil, typeMismatch("invalid teardown resource field type")
			}
			return nil, err
		}
		key := [3]string{resource.Kind, resource.Pool, resource.Name}
		if seen[key] {
			return nil, guardRefusal("duplicate teardown resource")
		}
		seen[key] = true
	}
	return resources, nil
}

// missingField and typeMismatch keep the historical diagnostic categories.
type missingField = ports.TeardownMissingField

type typeMismatch string

func (mismatch typeMismatch) Error() string { return string(mismatch) }

// incusQuery and incusCall are the only native Incus boundaries of the guard.
var incusQuery, incusCall = queryIncus, callIncus

func queryIncus(path, project string, target any) error {
	command := exec.Command("incus", "query", path+"?project="+strings.ReplaceAll(url.QueryEscape(project), "+", "%20"))
	output, err := command.Output()
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return typeMismatch("unexpected Incus response shape")
		}
		return err
	}
	return nil
}

// queryList reads an Incus collection. Only an actual JSON array, even an
// empty one, is evidence; null or another shape never proves absence.
func queryList(path, project string) ([]string, error) {
	var values []string
	if err := incusQuery(path, project, &values); err != nil {
		return nil, err
	}
	if values == nil {
		return nil, typeMismatch("Incus collection is not a list")
	}
	return values, nil
}

// presentIn reports whether an Incus collection names the resource.
func presentIn(path, name string) (bool, error) {
	values, err := queryList(path, "default")
	return slices.ContainsFunc(values, func(value string) bool {
		return strings.HasSuffix(strings.SplitN(value, "?", 2)[0], "/"+name)
	}), err
}

var fractionPattern = regexp.MustCompile(`\.(\d+)`)

func guardResource(resource ports.TeardownResource, project string) error {
	kind, name := resource.Kind, resource.Name
	path := ""
	switch kind {
	case "instance":
		path = "/1.0/instances/" + name
	case "profile", "host-profile":
		path = "/1.0/profiles/" + name
		if kind == "host-profile" {
			project = "default"
		}
	case "volume":
		if resource.Pool == "" {
			return missingField("pool")
		}
		path = "/1.0/storage-pools/" + resource.Pool + "/volumes/custom/" + name
	case "project":
		path, project = "/1.0/projects/"+name, "default"
	case "network":
		path, project = "/1.0/networks/"+name, "default"
	case "pool":
		path, project = "/1.0/storage-pools/"+name, "default"
	case "image":
		path = "/1.0/images/" + name
	}
	var observed map[string]any
	if err := incusQuery(path, project, &observed); err != nil {
		return err
	}
	// The live recheck reads the same shared binding fields as assessment.
	for _, field := range ports.TeardownBindingFields(kind) {
		if _, present := observed[field]; !present {
			return missingField(field)
		}
	}
	if kind == "instance" || kind == "volume" {
		snapshots, err := queryList(path+"/snapshots", project)
		if err != nil {
			return err
		}
		names := []string{}
		for _, item := range snapshots {
			if index := strings.IndexAny(item, "?#"); index >= 0 {
				item = item[:index]
			}
			item = item[strings.LastIndex(item, "/")+1:]
			if unescaped, err := url.PathUnescape(item); err == nil {
				item = unescaped
			}
			names = append(names, item)
		}
		sort.Strings(names)
		observed["snapshots"] = names
	}
	if kind == "instance" {
		timestamp, ok := observed["created_at"].(string)
		if !ok {
			return typeMismatch("instance creation time is not text")
		}
		suffix := ""
		if match := fractionPattern.FindStringSubmatch(timestamp); match != nil {
			suffix = strings.TrimRight(match[1], "0")
		}
		stamp, err := time.Parse(time.RFC3339, fractionPattern.ReplaceAllString(timestamp, ""))
		if err != nil {
			return guardRefusal("invalid instance creation time")
		}
		if suffix != "" {
			suffix = "." + suffix
		}
		observed["created_at"] = stamp.UTC().Format("2006-01-02T15:04:05") + suffix + "Z"
	}
	current, err := ports.TeardownBinding(kind, observed)
	if err != nil {
		return err
	}
	variants := []string{}
	if kind == "host-profile" {
		variants = resource.Bindings
	}
	if len(variants) > 3 || slices.ContainsFunc(variants, func(value string) bool { return !hexDigest(value) }) {
		return guardRefusal("invalid full default-profile retry bindings")
	}
	if current != resource.Binding && !slices.Contains(variants, current) {
		return guardRefusal("teardown resource changed after confirmation")
	}
	return nil
}

var guardAllReasons = []string{"oversized teardown inventory", "invalid teardown inventory", "invalid teardown resource fields",
	"invalid teardown resource kind", "invalid teardown resource identity", "invalid teardown binding",
	"duplicate teardown resource", "teardown requires a dedicated project",
	"invalid full default-profile retry bindings", "teardown resource changed after confirmation"}

// guardAllDetails crosses the boundary with fixed structural labels only,
// never native query output, commands or resource values.
func guardAllDetails(stage, kind string, ordinal int, err error) string {
	details := struct {
		Action    string `json:"action"`
		Stage     string `json:"stage"`
		Kind      string `json:"kind"`
		Ordinal   int    `json:"ordinal"`
		Category  string `json:"category"`
		Field     string `json:"field,omitempty"`
		Reason    string `json:"reason,omitempty"`
		QueryExit *int   `json:"query_exit,omitempty"`
		Errno     *int   `json:"errno,omitempty"`
	}{Action: "guard-all", Stage: stage, Kind: kind, Ordinal: ordinal, Category: "unknown"}
	var refusal guardRefusal
	var field missingField
	var mismatch typeMismatch
	var exit *exec.ExitError
	var errno syscall.Errno
	switch {
	case errors.As(err, &refusal):
		details.Category = "ValueError"
		if slices.Contains(guardAllReasons, string(refusal)) {
			details.Reason = string(refusal)
		}
	case errors.As(err, &field):
		details.Category, details.Field = "KeyError", string(field)
	case errors.As(err, &mismatch):
		details.Category = "TypeError"
	case errors.As(err, &exit):
		code := exit.ExitCode()
		details.Category, details.QueryExit = "CalledProcessError", &code
	case errors.Is(err, exec.ErrNotFound):
		code := int(unix.ENOENT)
		details.Category, details.Errno = "FileNotFoundError", &code
	case errors.As(err, &errno):
		code := int(errno)
		details.Errno = &code
		switch errno {
		case unix.EACCES, unix.EPERM:
			details.Category = "PermissionError"
		case unix.ENOENT:
			details.Category = "FileNotFoundError"
		case unix.ENOTDIR, unix.EISDIR, unix.EEXIST, unix.EAGAIN, unix.EINTR, unix.ECHILD, unix.ESRCH, unix.ETIMEDOUT, unix.EPIPE, unix.ECONNREFUSED, unix.ECONNRESET, unix.ECONNABORTED:
		default:
			details.Category = "OSError"
		}
	}
	body, _ := json.Marshal(details)
	return string(body)
}

// removeShared retires exact unused host resources after all approved yard
// projects vanished; it prints each network that is verified gone.
func removeShared(selected bool, stdout io.Writer) error {
	all, err := inventory()
	if err != nil {
		return err
	}
	var resources, profiles []ports.TeardownResource
	for _, item := range all {
		if item.Kind == "host-profile" || item.Kind == "pool" || item.Kind == "network" {
			resources = append(resources, item)
			if item.Kind == "host-profile" {
				profiles = append(profiles, item)
			}
		}
	}
	if len(resources) == len(profiles) {
		return nil
	}
	if len(profiles) != 1 {
		return guardRefusal("missing exact host default profile")
	}
	var profile struct {
		UsedBy  []string                     `json:"used_by"`
		Devices map[string]map[string]string `json:"devices"`
	}
	if err := incusQuery("/1.0/profiles/default", "default", &profile); err != nil {
		return err
	}
	endpoint := func(kind string) string {
		if kind == "pool" {
			return "/1.0/storage-pools"
		}
		return "/1.0/networks"
	}
	present := func(kind, name string) (bool, error) { return presentIn(endpoint(kind), name) }
	var candidates []ports.TeardownResource
	for _, resource := range resources {
		if resource.Kind == "host-profile" {
			continue
		}
		exists, err := present(resource.Kind, resource.Name)
		if err != nil {
			return err
		}
		if !exists {
			if resource.Kind == "network" {
				fmt.Fprintln(stdout, resource.Name)
			}
			continue
		}
		if err := guardResource(resource, "default"); err != nil {
			return err
		}
		var actual struct {
			UsedBy json.RawMessage `json:"used_by"`
		}
		var consumers []string
		if err := incusQuery(endpoint(resource.Kind)+"/"+resource.Name, "default", &actual); err != nil {
			return err
		}
		// An absent consumer list is empty; null or another shape is no evidence.
		if actual.UsedBy != nil && (json.Unmarshal(actual.UsedBy, &consumers) != nil || consumers == nil) {
			return typeMismatch("shared resource consumers are not a list")
		}
		foreign := slices.ContainsFunc(consumers, func(consumer string) bool {
			return strings.SplitN(consumer, "?", 2)[0] != "/1.0/profiles/default" || len(profile.UsedBy) > 0
		})
		if foreign && !selected {
			return guardRefusal("approved shared resource acquired a foreign consumer")
		}
		if !foreign {
			candidates = append(candidates, resource)
		}
	}
	if len(candidates) > 0 {
		if err := guardResource(profiles[0], "default"); err != nil {
			return err
		}
	}
	// Only unused exact matching default devices are eligible. Never remove a
	// device from a profile which a surviving foreign workload still consumes.
	for _, resource := range candidates {
		device, property, family := "eth0", "network", "network"
		if resource.Kind == "pool" {
			device, property, family = "root", "pool", "storage"
		}
		if profile.Devices[device][property] == resource.Name {
			if len(profile.UsedBy) > 0 {
				return guardRefusal("foreign workload consumes the host default profile")
			}
			if err := incusCall("profile", "device", "remove", "default", device, "--project", "default"); err != nil {
				return err
			}
			delete(profile.Devices, device)
		}
		if err := guardResource(resource, "default"); err != nil {
			return err
		}
		if err := incusCall(family, "delete", resource.Name, "--project", "default"); err != nil {
			return err
		}
		if remains, err := present(resource.Kind, resource.Name); err != nil || remains {
			return errors.Join(err, guardRefusal("approved shared resource remains after apply"))
		}
		if resource.Kind == "network" {
			fmt.Fprintln(stdout, resource.Name)
		}
	}
	return nil
}

func callIncus(arguments ...string) error {
	command := exec.Command("incus", arguments...)
	command.Stderr = os.Stderr
	return command.Run()
}

// callerBarrier keeps an orphaned physical leaf under the same approved owner
// barrier once its live caller is gone.
type callerBarrier struct {
	plan *RecoveryPlan
	held *os.File
}

func (barrier *callerBarrier) check() error {
	identity := os.Getenv("SUBYARD_TEARDOWN_PARENT_ID")
	if identity == "" || barrier.held != nil {
		return nil
	}
	pid, birth, ok := strings.Cut(identity, ":")
	if !ok {
		return errors.New("invalid teardown caller identity")
	}
	status, err := os.ReadFile("/proc/" + pid + "/stat")
	if err == nil {
		// Comm may contain spaces/parentheses; fields after its final ')' start at state.
		fields := strings.Fields(string(status)[strings.LastIndex(string(status), ")")+1:])
		if len(fields) < 20 {
			return errors.New("teardown caller identity is unavailable")
		}
		if fields[0] != "Z" && fields[19] == birth {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lock := barrier.plan.OwnerLock
	if lock == nil || len(lock.Entries) != 1 {
		return guardRefusal("approved owner barrier is missing after caller interruption")
	}
	expected := lock.Entries[0]
	parent, name, err := pinParent(lock.Path)
	if err != nil {
		return err
	}
	fd, err := unix.Openat(parent, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	unix.Close(parent)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err == nil && (info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 || info.Uid != barrier.plan.Installation.OperatorUID || info.Dev != expected.Device || info.Ino != expected.Inode || info.Gid != expected.GID) {
		err = guardRefusal("owner barrier changed after caller interruption")
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		file.Close()
		return err
	}
	barrier.held = file
	return nil
}

func filesNeedRoot(path string) bool {
	body, err := os.ReadFile(path)
	var plan RecoveryPlan
	if err != nil || json.Unmarshal(body, &plan) != nil {
		return false
	}
	return slices.ContainsFunc(plan.Trees, func(tree Tree) bool {
		return tree.Opaque || slices.ContainsFunc(tree.Entries, func(entry Entry) bool { return entry.UID != uint32(os.Getuid()) })
	})
}

// filesInterruption is a fixture seam between refinement publication and the first unlink.
var filesInterruption func() error

// removeFiles durably refines approved private roots, then removes runtime last.
func removeFiles() error {
	plan, err := readPlan()
	if err != nil {
		return err
	}
	barrier := &callerBarrier{plan: plan}
	// The leaf exits after this phase; release explicitly for in-process callers.
	defer func() {
		if barrier.held != nil {
			barrier.held.Close()
		}
	}()
	if err := barrier.check(); err != nil {
		return err
	}
	changed := false
	ownedGit := plan.OwnedGitDirectories
	checkGit := func(tree Tree, current []Entry) error {
		if err := checkEntries(tree.Entries, current); err != nil {
			return err
		}
		for _, entry := range current {
			if filepath.Base(entry.Path) == ".git" && !slices.Contains(ownedGit, filepath.Join(tree.Path, entry.Path)) {
				return guardRefusal("full uninstall refuses user or source Git repositories")
			}
		}
		return nil
	}
	for index := range plan.Trees {
		tree := &plan.Trees[index]
		if lexists(tree.Path+"/.git") && !slices.Contains(ownedGit, tree.Path+"/.git") {
			return guardRefusal("full uninstall refuses a user Git repository root")
		}
		if resource := tree.Resource; resource != nil {
			if resource.Kind != "pool" {
				return guardRefusal("invalid backing storage owner")
			}
			if remains, err := presentIn("/1.0/storage-pools", resource.Name); err != nil {
				return err
			} else if remains {
				return guardRefusal("backing storage pool remains; preserve its data")
			}
		}
		if tree.Opaque && tree.Scoped {
			var all []Entry
			err := withPinned(tree.Path, func(parent int, name string) (err error) {
				all, err = walkEntries(parent, name, maxEntries, tree.Exclusions, nil, false, nil)
				return err
			})
			if err != nil {
				return err
			}
			retained := []string{}
			for _, entry := range all {
				if !withinOwned(entry.Path, tree.OwnedPaths) && !excluded(entry.Path, retained) {
					retained = append(retained, entry.Path)
				}
			}
			tree.Preserved, tree.Exclusions = appendUnique(tree.Preserved, retained), appendUnique(tree.Exclusions, retained)
			for _, path := range retained {
				plan.Preserved = append(plan.Preserved, "foreign content "+tree.Path+"/"+path)
			}
		}
		captured, err := tree.capture()
		if err != nil {
			return err
		}
		current := captured.Entries
		facts := map[string]Entry{}
		for _, entry := range current {
			facts[entry.Path] = entry
		}
		for _, namespace := range tree.Namespaces {
			if fact, ok := facts[namespace.Path]; ok && !sameFact(&namespace, fact, true) {
				return guardRefusal("installation-created host namespace changed")
			}
		}
		if tree.Opaque {
			if len(tree.Entries) != 1 || tree.Entries[0].Path != "." {
				return guardRefusal("invalid conditional private root")
			}
			if len(current) > 0 {
				before, after := tree.Entries[0], current[0]
				if tree.Mutable {
					before.Size, before.Modified, before.Changed = 0, 0, 0
					after.Size, after.Modified, after.Changed = 0, 0, 0
				}
				if !sameFact(&before, after, true) {
					return guardRefusal("private full uninstall root changed")
				}
			}
			if tree.Category == "agent" {
				allowed := []string{".", "manager.lock", "worker.lock", "worker.json", "control.sock", "private.sock"}
				if slices.ContainsFunc(current, func(entry Entry) bool {
					return !slices.Contains(allowed, entry.Path) || entry.UID != plan.Installation.OperatorUID
				}) {
					return guardRefusal("SSH signing runtime acquired foreign entries")
				}
			}
			tree.Entries, tree.Opaque, changed = current, false, true
		}
		if err := checkGit(*tree, current); err != nil {
			return err
		}
	}
	// Publish metadata before the first unlink; interruption never adopts new
	// descendants of a private root when retried by a fresh process.
	if changed {
		if err := barrier.check(); err != nil {
			return err
		}
		body, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		err = withPinned(os.Getenv("SUBYARD_TEARDOWN_PLAN"), func(parent int, name string) error {
			if _, err := writePending(parent, ".refined-plan-next", body, 0600, plan.Installation.OperatorUID); err != nil {
				return err
			}
			return publishAt(parent, ".refined-plan-next", name)
		})
		if err != nil {
			return err
		}
	}
	if filesInterruption != nil {
		if err := filesInterruption(); err != nil {
			return err
		}
	}
	ordered := slices.Clone(plan.Trees)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Category != "runtime" && ordered[j].Category == "runtime" })
	for _, tree := range ordered {
		captured, err := tree.capture()
		if err == nil {
			err = checkGit(tree, captured.Entries)
		}
		if err == nil {
			err = withParent(tree.Path, "full uninstall root changed", func(parent int, name string) error {
				return removeEntries(parent, name, tree.Entries, true, tree.Exclusions, len(tree.Exclusions) > 0, barrier.check)
			})
		}
		if err != nil {
			return err
		}
	}
	// Only approved empty containers may remain after separately captured
	// descendants are removed. Identity-pinned rmdir never deletes new content.
	containers := slices.Clone(ordered)
	sort.SliceStable(containers, func(i, j int) bool { return len(containers[i].Path) > len(containers[j].Path) })
	for _, tree := range containers {
		for index := len(tree.Entries) - 1; index >= 0; index-- {
			original := tree.Entries[index]
			if original.Mode&unix.S_IFMT != unix.S_IFDIR {
				continue
			}
			candidate := filepath.Join(tree.Path, original.Path)
			err := withParent(candidate, "approved container changed", func(parent int, name string) error {
				current, err := statAt(parent, name, original.Path)
				if err != nil {
					return err
				}
				if !sameFact(&original, current, true) {
					return guardRefusal("approved container identity changed")
				}
				if err := barrier.check(); err != nil {
					return err
				}
				return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
			})
			if err != nil && !errors.Is(err, unix.ENOTEMPTY) {
				return err
			}
		}
	}
	for _, tree := range ordered {
		if err := tree.verifyRemoved("approved full uninstall artifact remains"); err != nil {
			return err
		}
	}
	return nil
}

func appendUnique(values, additions []string) []string {
	result := []string{}
	for _, value := range append(append([]string{}, values...), additions...) {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

var profileName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// checkProfiles runs only approved shipped owner hooks; never prints records.
func checkProfiles(arguments []string, stderr io.Writer) error {
	if len(arguments) != 1 {
		return errors.New("usage: check-profiles ROOT")
	}
	root, err := filepath.EvalSymlinks(arguments[0])
	if err != nil {
		return err
	}
	if root, err = filepath.Abs(root); err != nil {
		return err
	}
	plan, err := readPlan()
	if err != nil {
		return err
	}
	if len(plan.ProfileChecks) > 128 {
		return guardRefusal("invalid conditional owner inventory")
	}
	for _, check := range plan.ProfileChecks {
		if !profileName.MatchString(check.Profile) || check.Environment == nil {
			return guardRefusal("invalid conditional owner identity")
		}
		directory := filepath.Join(root, "config/profiles", check.Profile)
		body, err := os.ReadFile(filepath.Join(directory, "profile.json"))
		if err != nil {
			return err
		}
		var declaration map[string]any
		if err := json.Unmarshal(body, &declaration); err != nil {
			return err
		}
		hook := directory + "/" + check.Hook
		info, statErr := os.Lstat(hook)
		resolved, resolveErr := filepath.EvalSymlinks(hook)
		if declaration["full_cleanup"] != check.Hook || statErr != nil || !info.Mode().IsRegular() || resolveErr != nil || resolved != hook || !strings.HasPrefix(hook, directory+"/") {
			return guardRefusal("conditional owner hook differs from its declaration")
		}
		if len(check.Environment) > 2048 {
			return guardRefusal("invalid conditional owner context")
		}
		environment := []string{}
		for name, value := range check.Environment {
			if name != "PYTHONDONTWRITEBYTECODE" && name != "SUBYARD_FULL_CLEANUP_READ_ONLY" {
				environment = append(environment, name+"="+value)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, hook, "--verify-private")
		command.Env = append(environment, "PYTHONDONTWRITEBYTECODE=1", "SUBYARD_FULL_CLEANUP_READ_ONLY=1")
		command.Dir = root
		command.WaitDelay = 5 * time.Second
		output, runErr := command.Output()
		expired := ctx.Err() != nil
		cancel()
		if expired {
			return errors.New("conditional owner proof timed out")
		}
		var exit *exec.ExitError
		if runErr != nil && !errors.As(runErr, &exit) {
			return runErr
		}
		if runErr != nil || !bytes.Equal(bytes.Trim(output, " \t\n\r\v\f"), []byte(`{"verified": true}`)) {
			fmt.Fprintln(stderr, "profile "+check.Profile+" private producer ownership could not be proven; preserve its producers and evidence and restore the approved source inventory")
			return guardRefusal("conditional owner proof failed for " + check.Profile)
		}
	}
	return nil
}

// asAccount runs fn on a dedicated OS thread with the account's filesystem
// credentials. Switching fsuid away from root also drops that thread's
// file-permission capabilities. The thread is never unlocked, so the runtime
// discards it instead of reusing it.
func asAccount(name string, fn func() error) error {
	account, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		if uid != os.Geteuid() {
			return os.ErrPermission
		}
		return fn()
	}
	names, err := account.GroupIds()
	if err != nil {
		return err
	}
	groups := []int{gid}
	for _, value := range names {
		group, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		groups = append(groups, group)
	}
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Setgroups(groups); err != nil {
			result <- err
			return
		}
		// setfsuid/setfsgid return the previous value; -1 only reads it back.
		_, gidErr := unix.SetfsgidRetGid(gid)
		_, uidErr := unix.SetfsuidRetUid(uid)
		currentUID, _ := unix.SetfsuidRetUid(-1)
		currentGID, _ := unix.SetfsgidRetGid(-1)
		if gidErr != nil || uidErr != nil || currentUID != uid || currentGID != gid {
			result <- os.ErrPermission
			return
		}
		result <- fn()
	}()
	return <-result
}
