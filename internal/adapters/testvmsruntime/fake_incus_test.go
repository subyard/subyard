package testvmsruntime

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const fakeIncusUnavailable = "Error: Failed to connect to local daemon"

type fakeIncus struct {
	*fakeRunner
	t        *testing.T
	projects map[string]*fakeIncusProject
	networks map[string]string
	accounts map[string]string
	fault    []string
	hostKey  string
}

type fakeIncusProject struct {
	config    map[string]string
	instances map[string]*fakeIncusVM
	volumes   map[string]bool
}

type fakeIncusVM struct {
	kind           string
	status         string
	config         map[string]string
	lastUsedAt     time.Time
	ip             string
	agentKeyWrites []guestKeyWrite
}

type guestKeyWrite struct {
	account string
	key     string
}

func newFakeIncus(t *testing.T) *fakeIncus {
	fake := &fakeIncus{
		t: t, projects: make(map[string]*fakeIncusProject),
		networks: make(map[string]string), accounts: make(map[string]string), hostKey: fixturePublicKey(t),
	}
	fake.fakeRunner = &fakeRunner{handler: fake.run}
	return fake
}

func (fake *fakeIncus) run(name string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
	switch name {
	case "systemctl", "pkill", "journalctl":
		return nil, nil, nil
	case "userdel":
		if len(args) == 1 && fake.accounts[args[0]] != "" {
			delete(fake.accounts, args[0])
			return nil, nil, nil
		}
	case "incus":
		if slices.Equal(args, fake.fault) {
			fake.fault = nil
			return nil, nil, &CommandError{
				Name: name, Args: slices.Clone(args), ExitCode: 1, Message: fakeIncusUnavailable,
			}
		}
		out, err := fake.incus(args)
		return []byte(out), nil, err
	}
	return nil, nil, fake.unexpected(append([]string{name}, args...))
}

func (fake *fakeIncus) incus(args []string) (string, error) {
	project := fake.projects[incusArg(args, "--project")]
	switch args[0] {
	case "project":
		return fake.project(args)
	case "network":
		name := args[2]
		switch args[1] {
		case "create":
			fake.networks[name] = incusSetting(args, "user.subyard.managed")
			return "", nil
		case "show", "get", "delete":
			marker, exists := fake.networks[name]
			if !exists {
				return "", fmt.Errorf("network %s not found", name)
			}
			if args[1] == "delete" {
				delete(fake.networks, name)
			}
			return marker, nil
		}
	case "query":
		return fake.query(args[1])
	case "list":
		if project == nil {
			break
		}
		switch incusArg(args, "-c") {
		case "n":
			return strings.Join(slices.Sorted(maps.Keys(project.instances)), "\n"), nil
		case "nt":
			if vm := project.instances[args[1]]; vm != nil {
				return args[1] + "," + strings.ToUpper(vm.kind), nil
			}
			return "", nil
		case "ns":
			if vm := project.instances[args[1]]; vm != nil {
				return args[1] + "," + vm.status, nil
			}
			return "", nil
		}
	case "init":
		if project == nil || project.instances[args[2]] != nil {
			break
		}
		name := args[2]
		config := incusSettings(args, "-c")
		config["volatile.base_image"] = args[1]
		config["volatile.uuid"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(fake.calls))
		kind := "container"
		if slices.Contains(args, "--vm") {
			kind = "virtual-machine"
		}
		project.instances[name] = &fakeIncusVM{kind: kind, status: "STOPPED", config: config}
		project.volumes[name] = true
		return "", nil
	case "copy":
		target := fake.projects[incusArg(args, "--target-project")]
		if project == nil || target == nil || project.instances[args[1]] == nil || target.instances[args[2]] != nil {
			break
		}
		source := project.instances[args[1]]
		config := maps.Clone(source.config)
		maps.Copy(config, incusSettings(args, "-c"))
		config["volatile.uuid"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(fake.calls))
		target.instances[args[2]] = &fakeIncusVM{kind: source.kind, status: "STOPPED", config: config}
		target.volumes[args[2]] = true
		return "", nil
	case "info", "config", "stop", "start", "delete", "exec", "console":
		if project != nil {
			return fake.instance(project, args)
		}
	case "operation":
		if args[1] == "list" && project != nil {
			return "[]", nil
		}
	case "storage":
		if args[1] == "volume" && args[2] == "list" && project != nil {
			var rows []string
			for _, name := range slices.Sorted(maps.Keys(project.volumes)) {
				rows = append(rows, "virtual-machine,"+name)
			}
			return strings.Join(rows, "\n"), nil
		}
	case "profile":
		if args[1] == "device" && project != nil {
			switch args[2] {
			case "list":
				return "root\neth0\n", nil
			case "set":
				return "", nil
			}
		}
	}
	return "", fake.unexpected(args)
}

func (fake *fakeIncus) project(args []string) (string, error) {
	if args[1] == "list" && slices.Equal(args[2:], []string{"--format", "csv", "-c", "n"}) {
		return strings.Join(slices.Sorted(maps.Keys(fake.projects)), "\n"), nil
	}
	name := args[2]
	project := fake.projects[name]
	if args[1] == "create" && project == nil {
		fake.projects[name] = &fakeIncusProject{
			config: incusSettings(args, "-c"), instances: make(map[string]*fakeIncusVM),
			volumes: make(map[string]bool),
		}
		return "", nil
	}
	if project == nil {
		return "", fmt.Errorf("project %s not found", name)
	}
	switch args[1] {
	case "get":
		return project.config[args[3]], nil
	case "set":
		project.config[args[3]] = args[4]
		return "", nil
	case "show":
		return fakeJSON(project.config), nil
	case "delete":
		if len(project.instances) != 0 || len(project.volumes) != 0 {
			return "", fmt.Errorf("project %s is not empty", name)
		}
		delete(fake.projects, name)
		return "", nil
	}
	return "", fake.unexpected(args)
}

func (fake *fakeIncus) instance(project *fakeIncusProject, args []string) (string, error) {
	name := args[1]
	if args[0] == "config" || name == "--show-log" {
		name = args[2]
	}
	vm := project.instances[name]
	if vm == nil {
		return "", fmt.Errorf("instance %s not found", name)
	}
	switch args[0] {
	case "info", "console":
		return vm.status, nil
	case "config":
		if args[1] == "get" {
			return vm.config[args[3]], nil
		}
	case "start":
		vm.status = "RUNNING"
		vm.lastUsedAt = time.Now().UTC()
		return "", nil
	case "stop":
		if incusArg(args, "--timeout") == "60" {
			vm.status = "STOPPED"
			return "", nil
		}
	case "delete":
		if vm.status != "STOPPED" && !slices.Contains(args, "--force") {
			return "", fmt.Errorf("instance %s is still running", name)
		}
		delete(project.instances, name)
		delete(project.volumes, name)
		return "", nil
	case "exec":
		if vm.status != "RUNNING" {
			return "", fmt.Errorf("instance %s is stopped", name)
		}
		separator := slices.Index(args, "--")
		command := args[separator+1:]
		switch {
		case slices.Equal(command, []string{"ip", "-4", "route", "show", "default"}):
			return "default via 192.0.2.1 dev eth0", nil
		case command[0] == "awk" && slices.Contains(command, "/etc/ssh/ssh_host_ed25519_key.pub"):
			return fake.hostKey, nil
		case command[0] == "sh" && strings.Contains(command[len(command)-1], "authorized_keys"):
			account := managedGuestKeyAccount(command[len(command)-1])
			if account == "" {
				return "", fake.unexpected(args)
			}
			vm.agentKeyWrites = append(vm.agentKeyWrites, guestKeyWrite{
				account: account, key: incusSetting(args[:separator], "AGENT_KEY"),
			})
			return "", nil
		case command[0] == "sh" && strings.Contains(command[len(command)-1], "subyard-e2e-lease.json"):
			return "", nil
		case command[0] == "fstrim" || slices.Contains(command, "/run/subyard-e2e-lease.json"):
			return "", nil
		}
	}
	return "", fake.unexpected(args)
}

// Resolve the managed key script's move target from its path assignments.
func managedGuestKeyAccount(script string) string {
	paths := make(map[string]string, 3)
	expand := func(value string) string {
		return os.Expand(strings.Trim(value, `"`), func(name string) string { return paths[name] })
	}
	for line := range strings.SplitSeq(script, "\n") {
		if target, ok := strings.CutPrefix(line, `mv -f "$temp" `); ok {
			switch expand(target) {
			case "/home/dev/.ssh/authorized_keys":
				return "dev"
			case "/root/.ssh/authorized_keys":
				return "root"
			}
		}
		name, value, _ := strings.Cut(line, "=")
		switch name {
		case "home", "ssh_dir", "authorized":
			paths[name] = expand(value)
		}
	}
	return ""
}

func (fake *fakeIncus) query(path string) (string, error) {
	switch path {
	case "/1.0/storage-pools/default":
		return `{"driver":"dir"}`, nil
	case "/1.0/storage-pools/default/resources":
		return `{"space":{"total":107374182400,"used":0}}`, nil
	}
	request, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	project := fake.projects[request.Query().Get("project")]
	parts := strings.Split(strings.TrimPrefix(request.Path, "/1.0/instances/"), "/")
	if project == nil || project.instances[parts[0]] == nil {
		return "", fmt.Errorf("instance query %s not found", path)
	}
	vm := project.instances[parts[0]]
	if len(parts) == 1 {
		status := map[string]string{"STOPPED": "Stopped", "RUNNING": "Running"}[vm.status]
		return fakeJSON(map[string]any{"type": vm.kind, "config": vm.config, "status": status, "last_used_at": vm.lastUsedAt}), nil
	}
	switch parts[1] {
	case "state":
		return fmt.Sprintf(`{"network":{"eth0":{"addresses":[{"family":"inet","scope":"global","address":%q}]}}}`, vm.ip), nil
	case "logs":
		return "fixture console", nil
	}
	return "", fake.unexpected([]string{"query", path})
}

func (fake *fakeIncus) unexpected(args []string) error {
	fake.t.Helper()
	err := fmt.Errorf("unexpected fake command: %q", args)
	fake.t.Error(err)
	return err
}

// Calls without a selector, such as global project inventory, have no project.
func incusCallProject(args []string) string {
	switch args[0] {
	case "project":
		if args[1] != "list" {
			return args[2]
		}
	case "query":
		if path, err := url.Parse(args[1]); err == nil {
			return path.Query().Get("project")
		}
	default:
		return incusArg(args, "--project")
	}
	return ""
}

func incusArg(args []string, flag string) string {
	if index := slices.Index(args, flag); index >= 0 && index+1 < len(args) {
		return args[index+1]
	}
	return ""
}

func incusSettings(args []string, flag string) map[string]string {
	result := make(map[string]string)
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			key, value, _ := strings.Cut(args[index+1], "=")
			result[key] = value
		}
	}
	return result
}

func incusSetting(args []string, key string) string {
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, key+"="); ok {
			return value
		}
	}
	return ""
}

func fakeJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
