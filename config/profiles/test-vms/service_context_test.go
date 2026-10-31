package testvms_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/cli"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Exercise the shipped preset's profile-only ALLOWS_CODING_TOOLS setting through
// CLI loading, prepared rereading, and apply. A resolved setting must never be
// resubmitted as a command override during registration or status.
func TestServiceContextRereadsShippedPresetAndPreservesCustomPort(t *testing.T) {
	root := testkit.TempDir(t)
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(body), mode)
	}
	copyPublic := func(source, target string, mode os.FileMode) {
		t.Helper()
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		write(target, string(body), mode)
	}
	copyPublic("../../commands.registry", "config/commands.registry", 0o644)
	for _, name := range []string{"profile.json", "yard.env", "full-cleanup.py", "service-binding.py"} {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".py") {
			mode = 0o755
		}
		copyPublic(name, "config/profiles/test-vms/"+name, mode)
	}
	copyPublic("../../yards/profiles/test-vms.env", "config/yards/profiles/test-vms.env", 0o644)
	for _, name := range []string{"incus.project.env", "host.env", "ports.env"} {
		write("config/"+name, "", 0o644)
	}
	write("config/agents.env", "CODING_TOOL_INTEGRATIONS=\n", 0o644)
	write("config/subyard.env", fmt.Sprintf("STORAGE_PATH='%s'\nHOST_BASE='%s'\nRESTRICTED_DISK_PATHS='%s'\nSHIFT_MODE=shift\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nDEV_UID=1000\nDEV_USER=dev\nSSH_PORT=2222\n", filepath.Join(root, "data/storage"), filepath.Join(root, "host"), filepath.Join(root, "host")), 0o644)
	write("state/yards/test-yard/config.env", "YARD_TEMPLATE=test-vms\nSSH_PORT=2299\nDEV_UID=2007\n", 0o600)
	home := filepath.Join(root, "home")
	environment := map[string]string{"HOME": home, "SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": filepath.Join(root, "state"), "SUBYARD_HOME": filepath.Join(root, "data"), "SUBYARD_NO_AUDIT": "1"}
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "test-yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["ALLOWS_CODING_TOOLS"] != "false" || loaded.Environment["SSH_PORT"] != "2299" || loaded.Environment["DEV_UID"] != "2007" {
		t.Fatal("shipped restricted preset/custom port and UID not loaded")
	}
	if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(loaded.Context.Paths.DataHome, "e2e/routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	instance := ports.InstanceInfo{Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running", Config: map[string]string{"user.subyard.managed": "true", "user.subyard.name": "test-yard", "volatile.uuid": "service-context-uuid"}}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}, Reconcile: ports.ReconcileState{ProjectFound: true, InstanceFound: true, Instance: instance, ProjectConfig: map[string]string{"user.subyard.installation": "1", "user.subyard.operator_uid": strconv.Itoa(os.Getuid()), "user.subyard.data_home": loaded.Context.Paths.DataHome, "user.subyard.config_home": loaded.Context.Paths.ConfigHome, "user.subyard.yard_name": "test-yard", "user.subyard.instance_name": instance.Name}}}
	var env []string
	for key, value := range environment {
		env = append(env, key+"="+value)
	}
	for _, action := range []string{"register", "status"} {
		var output, diagnostics bytes.Buffer
		arguments := []string{"service", "register", "test-vms", "--yard", "test-yard", "--yes"}
		adapter := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Stderr: `{"schema_version":1,"status":"ok","pool":{"schema_version":2,"slots":[]}}`}}}
		if action == "status" {
			arguments = []string{"-Y", "missing-work-yard", "test-vms", "status", "--json"}
		}
		program, err := cli.New(cli.Options{RepositoryRoot: root, Arguments: arguments, Environment: env, Incus: incus, Executor: incus, AdapterRunner: adapter, Stdout: &output, Stderr: &diagnostics})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("%s exit=%d: %s", action, code, diagnostics.String())
		}
		if action == "status" && (!strings.Contains(output.String(), `"runtime_uuid":"service-context-uuid"`) || len(adapter.Requests) != 1 || adapter.Requests[0].Context["YARD_NAME"] != "test-yard" || adapter.Requests[0].Context["SSH_PORT"] != "2299" || adapter.Requests[0].Context["DEV_UID"] != "2007") {
			t.Fatalf("status did not bind registered service: %s", output.String())
		}
	}
	if len(incus.PowerUpdates) != 0 || len(incus.ConfigUpdates) != 0 {
		t.Fatal("registration/status mutated adopted runtime")
	}
	initial, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "test-yard", YardSettingsFile: filepath.Join(root, "config/profiles/test-vms/yard.env"), Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Context.SSHPort != 2223 || !initial.Context.NestedE2EVMs || initial.Environment["ALLOWS_CODING_TOOLS"] != "false" || initial.Environment["DEV_UID"] != "1001" {
		t.Fatal("declared installation preset lost safe initial service settings")
	}
	write("state/yards/test-yard/config.env", "YARD_TEMPLATE=test-vms\nSSH_PORT=2299\n", 0o600)
	existing, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "test-yard", Environment: environment})
	if err != nil || existing.Environment["DEV_UID"] != "1000" {
		t.Fatalf("creation preset changed an existing template-only runtime UID: %v", err)
	}
	environment["ALLOWS_CODING_TOOLS"] = "false"
	if _, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "test-yard", Environment: environment}); err == nil || !strings.Contains(err.Error(), "not allowed in command scope") {
		t.Fatalf("explicit restricted command override was accepted: %v", err)
	}
}
