package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProbeConvergedClassifiesExitStatus(t *testing.T) {
	exit := func(code string) error {
		return exec.Command("sh", "-c", "exit "+code).Run()
	}
	tests := []struct {
		name    string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "success", want: true},
		{name: "drift", err: exit("1")},
		{name: "other exit", err: exit("17"), wantErr: true},
		{name: "non-exit error", err: errors.New("probe unavailable"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converged, err := probeConverged(test.err)
			if converged != test.want || (err != nil) != test.wantErr {
				t.Fatalf("converged=%v error=%v, want converged=%v error=%v",
					converged, err, test.want, test.wantErr)
			}
		})
	}
}

func TestInstallIncusUsesConfiguredSRVPool(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.Mkdir(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "storage-pool")
	if err := os.WriteFile(filepath.Join(scripts, "01-install-incus.sh"), []byte(
		"#!/bin/sh\nprintf '%s\\n' \"${STORAGE_POOL:-}\" > \"$CAPTURE\"\n",
	), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{
		RepositoryRoot: root,
		Environment:    []string{"CAPTURE=" + capture},
		Incus:          &testkit.Incus{},
		SRVPool:        "nested-e2e",
	}
	if err := runtime.installIncus(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(pool) != "nested-e2e\n" {
		t.Fatalf("installer storage pool = %q, want nested-e2e", pool)
	}
}

func TestPowerImportIsNativeAcrossRegisteredLocalYards(t *testing.T) {
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Instances: map[string]ports.InstanceInfo{
			"subyard/yard": {Name: "yard", Project: "subyard", Status: "Running"},
			"subyard-demo/yard-demo": {
				Name: "yard-demo", Project: "subyard-demo", Status: "Stopped",
				Config: map[string]string{
					"user.subyard.managed": "true", "user.subyard.initialized": "false",
					"user.subyard.desired_power": "stopped", "user.subyard.name": "old",
					"user.subyard.bridge": "old", "boot.autostart": "true",
				},
			},
		},
	}
	runtime := Runtime{
		Incus: incus, ConfigWriter: incus,
		PowerYards: []domain.Context{
			{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0"},
			{YardName: "demo", IncusProject: "subyard-demo", YardInstanceName: "yard-demo", IncusBridge: "incusbr1"},
			{YardName: "remote", AccessKind: domain.AccessRemote},
		},
	}
	assertStage(t, runtime, "power-import", false, "pending native import")
	if err := runtime.ApplyStage(context.Background(), "power-import"); err != nil {
		t.Fatal(err)
	}
	if len(incus.ConfigUpdates) != 2 ||
		incus.Instances["subyard/yard"].Config["user.subyard.desired_power"] != "running" ||
		incus.Instances["subyard-demo/yard-demo"].Config["user.subyard.name"] != "demo" {
		t.Fatalf("registered power state was not imported atomically: %#v", incus.ConfigUpdates)
	}
	assertStage(t, runtime, "power-import", true, "completed native import")
	broken := incus.Instances["subyard-demo/yard-demo"]
	broken.Config["user.subyard.managed"] = "invalid"
	incus.Instances["subyard-demo/yard-demo"] = broken
	if _, err := runtime.CheckStage(context.Background(), "power-import"); err == nil {
		t.Fatal("invalid managed metadata was treated as convergence")
	}
}

func TestGitIdentityProbeUsesTypedInstanceState(t *testing.T) {
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
			Status: "Running",
		}},
		ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{}}},
	}
	runtime := Runtime{
		Incus: incus, Executor: incus,
		Yard:        domain.Context{IncusProject: "subyard", YardInstanceName: "yard"},
		Environment: []string{"DEV_USER=developer"},
	}
	assertStage(t, runtime, "git-identity", true, "running yard with git config")
	if calls := incus.ExecCalls; len(calls) != 1 ||
		calls[0].Request.Command[2] != "/home/developer/.gitconfig" {
		t.Fatalf("unexpected git identity probe: %#v", calls)
	}
	incus.ExecSteps = []testkit.IncusExecStep{{
		Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.New("missing"),
	}}
	assertStage(t, runtime, "git-identity", false, "running yard without git config")

	incus.Reconcile.Instance = ports.InstanceInfo{Status: "Stopped", Config: map[string]string{
		"user.subyard.managed": "true", "user.subyard.initialized": "true",
		"user.subyard.desired_power": "stopped",
	}}
	assertStage(t, runtime, "git-identity", true, "intentionally stopped yard")
	incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "running"
	assertStage(t, runtime, "git-identity", false, "unexpected stopped yard")
}

func TestIntentionallyStoppedShortcutPhaseMatrix(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		managed string
		init    string
		desired string
		want    bool
	}{
		{name: "exact stopped intent", status: "Stopped", managed: "true", init: "true", desired: "stopped", want: true},
		{name: "running physical state", status: "Running", managed: "true", init: "true", desired: "stopped"},
		{name: "managed absent", status: "Stopped", init: "true", desired: "stopped"},
		{name: "managed empty", status: "Stopped", init: "true", desired: "stopped"},
		{name: "initialized false", status: "Stopped", managed: "true", init: "false", desired: "stopped"},
		{name: "desired running", status: "Stopped", managed: "true", init: "true", desired: "running"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := ports.InstanceInfo{Status: test.status, Config: map[string]string{
				"user.subyard.managed":       test.managed,
				"user.subyard.initialized":   test.init,
				"user.subyard.desired_power": test.desired,
				"user.subyard.bridge":        "",
				"boot.autostart":             "true",
			}}
			if test.name == "managed absent" {
				delete(instance.Config, "user.subyard.managed")
			}
			if got := instanceIntentionallyStopped(instance); got != test.want {
				t.Fatalf("shortcut=%v, want %v", got, test.want)
			}
		})
	}
}

func TestPowerProbeSeparatesInstallFromFinalMetadata(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	installed := filepath.Join(root, "installed")
	for _, directory := range []string{
		bin, installed, filepath.Join(root, "config", "systemd"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, contents string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	reconcilerSource := filepath.Join(root, "yard-engine")
	reconciler := filepath.Join(installed, "yard-boot-reconcile")
	unit := filepath.Join(installed, "subyard-power-reconcile.service")
	write(reconcilerSource, "reconciler\n", 0o700)
	write(reconciler, "reconciler\n", 0o700)
	template := "[Service]\nExecStart=@SUBYARD_POWER_RECONCILER@ _power-reconcile\nRestart=on-failure\n"
	write(filepath.Join(root, "config", "systemd", "subyard-power-reconcile.service.in"),
		template, 0o600)
	write(unit, strings.ReplaceAll(template, "@SUBYARD_POWER_RECONCILER@", reconciler), 0o600)
	write(filepath.Join(bin, "systemctl"), "#!/bin/sh\nexit 0\n", 0o700)

	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
			Config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.initialized": "true",
				"user.subyard.desired_power": "running", "user.subyard.bridge": "incusbr0",
				"boot.autostart": "false",
			},
		}},
	}
	runtime := Runtime{RepositoryRoot: root, Incus: incus, Environment: []string{
		"PATH=" + bin, "SUBYARD_POWER_RECONCILER_PATH=" + reconciler,
		"SUBYARD_POWER_ENGINE_SOURCE=" + reconcilerSource, "SUBYARD_POWER_UNIT_PATH=" + unit,
	}}
	assertStage(t, runtime, "power", true, "installed and finalized power state")
	incus.Reconcile.Instance.Config["user.subyard.initialized"] = "false"
	assertStage(t, runtime, "power", false, "unfinished desired-power transaction")
	verified, err := runtime.VerifyStage(context.Background(), "power")
	if err != nil || !verified {
		t.Fatalf("fresh install did not verify before finalization: %v, %v", verified, err)
	}
	incus.Reconcile.Instance.Config["user.subyard.initialized"] = "true"
	incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "paused"
	assertStage(t, runtime, "power", false, "malformed desired power")
	incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "running"
	incus.Reconcile.Instance.Config["user.subyard.name"] = "ignored-by-init"
	assertStage(t, runtime, "power", true, "init power check ignores yard name")
	incus.Reconcile.Instance.Config["user.subyard.bridge"] = "incusbr1"
	assertStage(t, runtime, "power", false, "wrong effective bridge")
	incus.Reconcile.Instance.Config["user.subyard.bridge"] = "incusbr0"
	incus.Reconcile.Instance.Config["boot.autostart"] = "true"
	assertStage(t, runtime, "power", false, "autostart enabled")
	incus.Reconcile.Instance.Config["boot.autostart"] = "false"
	write(reconciler, "drift\n", 0o700)
	verified, err = runtime.VerifyStage(context.Background(), "power")
	if err != nil || verified {
		t.Fatalf("drifted reconciler passed verification: %v, %v", verified, err)
	}
}

func TestTestVMHostSinkProbeRequiresSelectedEngineUnitAndTimer(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(root, "yard-engine")
	sink := filepath.Join(root, "test-vms-host-sink")
	service := filepath.Join(root, "subyard-test-vms-host-sink.service")
	timer := filepath.Join(root, "subyard-test-vms-host-sink.timer")
	for _, path := range []string{engine, sink} {
		if err := os.WriteFile(path, []byte("selected runtime\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(service, []byte(
		"[Service]\n"+
			`Environment="SUBYARD_HOME=/data"`+"\n"+
			"ExecStart="+sink+" _test-vms-host-sink sync\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timer, []byte("[Timer]\nOnUnitActiveSec=1min\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bin, "systemctl"),
		[]byte("#!/bin/sh\nexit 0\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{
		Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: "/data"}},
		Environment: []string{
			"PATH=" + bin,
			"SUBYARD_DISPATCHER_PATH=" + engine,
			"SUBYARD_TEST_VMS_SINK_PATH=" + sink,
			"SUBYARD_TEST_VMS_SINK_SERVICE_PATH=" + service,
			"SUBYARD_TEST_VMS_SINK_TIMER_PATH=" + timer,
		},
	}
	if !runtime.testVMHostSinkConverged(context.Background()) {
		t.Fatal("matching test-vms host sink was not converged")
	}
	if err := os.WriteFile(sink, []byte("stale runtime\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.testVMHostSinkConverged(context.Background()) {
		t.Fatal("stale test-vms host sink runtime was accepted")
	}
}

func TestFinalizeMapsDesiredPowerToLifecycleAction(t *testing.T) {
	for _, test := range []struct {
		desired string
		action  string
	}{
		{desired: "running", action: "start --reconcile"},
		{desired: "stopped", action: "stop --reconcile"},
	} {
		t.Run(test.desired, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0o700); err != nil {
				t.Fatal(err)
			}
			arguments := filepath.Join(root, "arguments")
			if err := os.WriteFile(filepath.Join(root, "scripts", "lifecycle-guard.sh"), []byte(
				"#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$ARGUMENTS\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {
					Name: "yard", Project: "subyard", Status: test.desired,
					Config: map[string]string{
						"user.subyard.managed": "true", "user.subyard.initialized": "false",
						"user.subyard.desired_power": test.desired, "user.subyard.name": "default",
						"user.subyard.bridge": "incusbr0", "boot.autostart": "false",
					},
				},
			}}
			runtime := Runtime{
				RepositoryRoot: root, Environment: append(os.Environ(), "ARGUMENTS="+arguments),
				Incus: incus, ConfigWriter: incus,
				Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard"},
			}
			if err := runtime.ApplyStage(context.Background(), "finalize"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(arguments)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.action+"\n" {
				t.Fatalf("lifecycle arguments = %q, want %q", got, test.action)
			}
			if incus.Instances["subyard/yard"].Config["user.subyard.initialized"] != "true" {
				t.Fatal("final power state was not committed")
			}
		})
	}
}

func TestSSHProbeOwnsProxyAndClientConfig(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	subyardHome := filepath.Join(root, "subyard")
	bin := filepath.Join(root, "bin")
	for _, directory := range []string{filepath.Join(home, ".ssh"), filepath.Join(subyardHome, "ssh"), bin} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	identity := filepath.Join(subyardHome, "ssh", "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", identity).CombinedOutput(); err != nil {
		t.Fatalf("generate transport identity: %v: %s", err, output)
	}
	if err := os.Chmod(identity, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(identity+".pub", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "subyard.config"), []byte(
		"Host yard\n    Port 2222\n    IdentityFile \""+identity+"\"\n"+
			"    IdentitiesOnly yes\n    StrictHostKeyChecking yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Include subyard.config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subyardHome, "ssh", "known_hosts"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ssh-keygen"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
			Status: "Stopped", Config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.initialized": "true",
				"user.subyard.desired_power": "stopped",
			}, LocalDevices: map[string]map[string]string{"ssh": {
				"type": "proxy", "listen": "tcp:127.0.0.1:2222", "connect": "tcp:127.0.0.1:22",
			}},
		}},
	}
	runtime := Runtime{
		Incus: incus, Executor: incus,
		Yard: domain.Context{
			YardName: "default", IncusProject: "subyard", YardInstanceName: "yard",
			SSHHost: "yard", SSHPort: 2222, YardKind: domain.YardContainer,
			Paths: domain.RuntimePaths{OperatorHome: home, DataHome: subyardHome},
		},
		Environment: []string{"PATH=" + bin},
	}
	assertStage(t, runtime, "ssh", true, "matching SSH state")
	if err := os.Chmod(identity, 0o640); err != nil {
		t.Fatal(err)
	}
	assertStage(t, runtime, "ssh", false, "unsafe transport identity")
	if err := os.Chmod(identity, 0o600); err != nil {
		t.Fatal(err)
	}
	incus.Reconcile.Instance.LocalDevices["ssh"]["listen"] = "tcp:127.0.0.1:2299"
	assertStage(t, runtime, "ssh", false, "drifted SSH proxy")
	incus.Reconcile.Instance.LocalDevices["ssh"]["listen"] = "tcp:127.0.0.1:2222"
	if err := os.Rename(
		filepath.Join(home, ".ssh", "subyard.config"),
		filepath.Join(home, ".ssh", "subyard-demo.config"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".ssh", "config"), []byte("Include subyard-demo.config\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	runtime.Yard.YardName = "demo"
	assertStage(t, runtime, "ssh", true, "matching named-yard SSH state")

	runtime.Yard.YardKind = domain.YardVM
	incus.Reconcile.Instance.LocalDevices["eth0"] = map[string]string{"ipv4.address": "10.0.0.2"}
	unitDir := filepath.Join(root, "systemd")
	if err := os.Mkdir(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "subyard-ssh-relay-2222.socket"), []byte(
		"[Socket]\nListenStream=127.0.0.1:2222\nAccept=no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(unitDir, "subyard-ssh-relay-2222.service")
	if err := os.WriteFile(service, []byte(
		"[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 10.0.0.2:22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime.Environment = append(runtime.Environment, "SUBYARD_SSH_RELAY_UNIT_DIR="+unitDir,
		fmt.Sprintf("SUBYARD_SSH_RELAY_EXPECTED_UID=%d", os.Geteuid()))
	assertStage(t, runtime, "ssh", true, "matching VM loopback relay")
	if err := os.WriteFile(service, []byte(
		"[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 10.0.0.3:22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertStage(t, runtime, "ssh", false, "VM relay targets another address")
	if err := os.WriteFile(service, []byte(
		"[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 10.0.0.2:22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	incus.Reconcile.Instance.Status = "Running"
	incus.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1}}}
	assertStage(t, runtime, "ssh", false, "guest missing canonical public key")
	incus.ExecSteps = []testkit.IncusExecStep{{}}
	assertStage(t, runtime, "ssh", true, "guest authorizes canonical public key")
	last := incus.ExecCalls[len(incus.ExecCalls)-1].Request.Command
	if len(last) < 4 || last[0] != "grep" || last[1] != "-qxF" {
		t.Fatalf("guest authorization probe does not match the canonical public key: %q", last)
	}
	runtime.Yard.NestedE2EVMs = true
	incus.ExecSteps = []testkit.IncusExecStep{{}}
	assertStage(t, runtime, "ssh", true, "nested guest authorizes restricted canonical key")
	last = incus.ExecCalls[len(incus.ExecCalls)-1].Request.Command
	if len(last) < 4 || !strings.HasPrefix(last[3], `from="127.0.0.1,::1" ssh-ed25519 `) {
		t.Fatalf("nested authorization probe is not exact and restricted: %q", last)
	}
}

func TestProvisionProbeChecksGuestAndStoppedMarker(t *testing.T) {
	steps := func(stat, configHash string) []testkit.IncusExecStep {
		return []testkit.IncusExecStep{
			{}, {}, {}, {Result: ports.InstanceExecResult{Stdout: []byte("dev:x:1000:1000::/home/dev:/bin/bash\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte(stat + "\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte(" 7f 45 4c 46\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte("ccusage 1.2.3\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte(configHash + "  config\n")}},
			{}, {Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.New("not a link")},
		}
	}
	instructions := filepath.Join(t.TempDir(), "AGENTS.md")
	payload := []byte("fixture\n")
	if err := os.WriteFile(instructions, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		ExecSteps:  steps("regular file|755|0:0", digest),
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
			Status: "Running", Config: map[string]string{"user.subyard.ccusage_version": "1.2.3"},
		}},
	}
	runtime := Runtime{
		Incus: incus, Executor: incus,
		Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev"},
		Environment: []string{
			"CODING_TOOL_INTEGRATIONS=opencode", "CCUSAGE_VERSION=1.2.3",
			"HOST_OPENCODE_AGENTS_MD=" + instructions,
		},
	}
	assertStage(t, runtime, "provision", true, "matching running provision state")
	if command := incus.ExecCalls[7].Request.Command; len(command) != 3 ||
		command[0] != "sha256sum" || command[2] != "/home/dev/.config/opencode/AGENTS.md" {
		t.Fatalf("OpenCode instructions were not checked natively: %#v", command)
	}
	if err := os.WriteFile(instructions, []byte("updated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	incus.ExecSteps = steps("regular file|755|0:0", digest)
	assertStage(t, runtime, "provision", false, "stale materialized agent config")
	linkedInstructions := filepath.Join(filepath.Dir(instructions), "linked-AGENTS.md")
	if err := os.Symlink(instructions, linkedInstructions); err != nil {
		t.Fatal(err)
	}
	runtime.Environment = []string{
		"CODING_TOOL_INTEGRATIONS=opencode", "CCUSAGE_VERSION=1.2.3",
		"HOST_OPENCODE_AGENTS_MD=" + linkedInstructions,
	}
	linkedDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("updated\n")))
	incus.ExecSteps = steps("regular file|755|0:0", linkedDigest)
	assertStage(t, runtime, "provision", true, "symlinked host instructions")
	runtime.Environment = []string{
		"CODING_TOOL_INTEGRATIONS=opencode", "CCUSAGE_VERSION=1.2.3",
		"HOST_OPENCODE_AGENTS_MD=" + instructions,
	}
	missing := steps("regular file|755|0:0", digest)
	missing[7] = testkit.IncusExecStep{
		Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.New("missing config"),
	}
	incus.ExecSteps = missing
	assertStage(t, runtime, "provision", false, "missing materialized agent config")
	forwardDrop := steps("regular file|755|0:0", digest)
	forwardDrop[1] = testkit.IncusExecStep{
		Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.New("FORWARD DROP"),
	}
	incus.ExecSteps = forwardDrop
	assertStage(t, runtime, "provision", false, "stale Docker forwarding policy")

	incus.ExecSteps = steps("regular file|777|0:0", digest)
	assertStage(t, runtime, "provision", false, "wrong ccusage mode")

	incus.Reconcile.Instance = ports.InstanceInfo{Status: "Stopped", Config: map[string]string{
		"user.subyard.managed": "true", "user.subyard.initialized": "true",
		"user.subyard.desired_power": "stopped", "user.subyard.ccusage_version": "1.2.3",
	}}
	assertStage(t, runtime, "provision", true, "matching stopped provision marker")
	runtime.Environment = []string{
		"CODING_TOOL_INTEGRATIONS=codex", "CCUSAGE_VERSION=1.2.3", "CODEX_VERSION=0.147.0",
	}
	assertStage(t, runtime, "provision", false, "missing stopped Codex version marker")
	incus.Reconcile.Instance.Config["user.subyard.codex_version"] = "0.147.0"
	assertStage(t, runtime, "provision", true, "matching stopped Codex version marker")
	runtime.Environment = []string{
		"CODING_TOOL_INTEGRATIONS=opencode", "CCUSAGE_VERSION=1.2.4",
		"HOST_OPENCODE_AGENTS_MD=" + instructions,
	}
	assertStage(t, runtime, "provision", false, "stale stopped provision marker")
}

func TestIncusProbeOwnsVersionPoolAndNetwork(t *testing.T) {
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus", Version: "6.0.6-debian13"},
		Reconcile:  ports.ReconcileState{HostPoolFound: true, HostNetworkFound: true},
	}
	runtime := Runtime{Incus: incus, Yard: domain.Context{IncusBridge: "incusbr0"}}
	assertStage(t, runtime, "incus", true, "matching Incus bootstrap")
	incus.ServerInfo.Version = "6.0.5"
	assertStage(t, runtime, "incus", false, "old Incus")
	incus.ServerInfo.Version = "6.1.0"
	incus.Reconcile.HostNetworkFound = false
	assertStage(t, runtime, "incus", false, "missing bridge")
}

func TestProvisionAgentCommandsAreValidated(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "provision.sh")
	if err := os.WriteFile(hook, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Environment: []string{
		"CODING_TOOL_INTEGRATIONS=opencode", "AGENT_opencode_PROVISION=" + hook, "AGENT_opencode_COMMAND=opencode",
	}}
	commands, err := runtime.provisionAgentCommands()
	if err != nil || len(commands) != 1 || commands[0] != "opencode" {
		t.Fatalf("valid agent command rejected: %v, %v", commands, err)
	}
	runtime.Environment[2] = "AGENT_opencode_COMMAND=bad command"
	if _, err := runtime.provisionAgentCommands(); err == nil {
		t.Fatal("unsafe agent command accepted")
	}
}

func TestProvisionAgentChecksAreValidated(t *testing.T) {
	runtime := Runtime{Environment: []string{
		"CODING_TOOL_INTEGRATIONS=paseo", "AGENT_paseo_CHECK=paseo-check",
	}}
	checks, err := runtime.provisionAgentChecks()
	if err != nil || len(checks) != 1 || checks[0] != "paseo-check" {
		t.Fatalf("valid agent check rejected: %v, %v", checks, err)
	}
	runtime.Environment[1] = "AGENT_paseo_CHECK=bad check"
	if _, err := runtime.provisionAgentChecks(); err == nil {
		t.Fatal("unsafe agent check accepted")
	}
}

func TestProjectProbeOwnsRestrictedPolicy(t *testing.T) {
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{
			ProjectFound: true, ProfileFound: true,
			ProjectConfig: map[string]string{
				"restricted": "true", "restricted.containers.nesting": "allow",
				"restricted.containers.privilege":    "unprivileged",
				"restricted.containers.interception": "block",
				"restricted.devices.disk":            "allow", "restricted.devices.disk.paths": "",
				"restricted.devices.unix-char": "allow", "restricted.devices.proxy": "allow",
			},
			ProfileDevices: map[string]map[string]string{
				"root": {"type": "disk"}, "eth0": {"type": "nic"},
			},
		},
	}
	runtime := Runtime{
		Incus: incus,
		Yard:  domain.Context{IncusProject: "subyard", YardKind: domain.YardContainer},
	}
	converged, err := runtime.CheckStage(context.Background(), "project")
	if err != nil || !converged {
		t.Fatalf("matching project rejected: %v, %v", converged, err)
	}
	incus.Reconcile.ProjectConfig["restricted"] = "false"
	converged, err = runtime.CheckStage(context.Background(), "project")
	if err != nil || converged {
		t.Fatalf("project policy drift accepted: %v, %v", converged, err)
	}
	incus.Reconcile.ProjectConfig["restricted"] = "true"
	runtime.Yard.NestedE2EVMs = true
	incus.Reconcile.ProjectConfig["restricted.containers.interception"] = "allow"
	converged, err = runtime.CheckStage(context.Background(), "project")
	if err != nil || !converged {
		t.Fatalf("trusted nested project rejected: %v, %v", converged, err)
	}
	delete(incus.Reconcile.ProfileDevices, "eth0")
	converged, err = runtime.CheckStage(context.Background(), "project")
	if err != nil || converged {
		t.Fatalf("missing project NIC accepted: %v, %v", converged, err)
	}
}

func TestInstanceProbeOwnsVolumeAndNestedBoundary(t *testing.T) {
	deviceRoot := t.TempDir()
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{
			InstanceFound: true, VolumeFound: true,
			Instance: ports.InstanceInfo{
				Status:      "Running",
				Config:      map[string]string{},
				LocalConfig: map[string]string{"security.nesting": "true"},
				LocalDevices: map[string]map[string]string{
					"srv": {"source": "yard-srv", "path": "/srv", "pool": "default"},
					"subyard-e2e-routes": {
						"type": "disk", "source": "/data/e2e/routes",
						"path": "/var/lib/subyard/e2e-routes", "readonly": "true",
					},
				},
			},
		},
	}
	runtime := Runtime{
		Incus: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", YardKind: domain.YardContainer,
			Paths: domain.RuntimePaths{DataHome: "/data"},
		},
		HostDeviceRoot: deviceRoot,
	}
	assertStageConverged(t, runtime, true, "matching instance")
	incus.Reconcile.Instance.LocalDevices["subyard-e2e-routes"]["path"] = "/run/subyard/e2e-routes"
	assertStageConverged(t, runtime, false, "boot-hidden route mount")
	incus.Reconcile.Instance.LocalDevices["subyard-e2e-routes"]["path"] =
		"/var/lib/subyard/e2e-routes"
	incus.Reconcile.Instance.Status = "Stopped"
	incus.Reconcile.Instance.Config["user.subyard.managed"] = "true"
	incus.Reconcile.Instance.Config["user.subyard.initialized"] = "true"
	incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "running"
	assertStageConverged(t, runtime, false, "stopped desired-running power fence")
	incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "stopped"
	assertStageConverged(t, runtime, true, "intentionally stopped instance")
	incus.Reconcile.Instance.Status = "Running"
	incus.Reconcile.Instance.LocalDevices["srv"]["source"] = "wrong"
	assertStageConverged(t, runtime, false, "drifted volume")
	incus.Reconcile.Instance.LocalDevices["srv"]["source"] = "yard-srv"
	if err := os.WriteFile(filepath.Join(deviceRoot, "kvm"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertStageConverged(t, runtime, false, "missing host KVM mapping")
	incus.Reconcile.Instance.LocalDevices["kvm"] = charDevice("/dev/kvm")
	assertStageConverged(t, runtime, true, "host KVM mapping")

	runtime.Yard.NestedE2EVMs = true
	incus.Reconcile.Instance.LocalConfig["security.syscalls.intercept.bpf"] = "true"
	incus.Reconcile.Instance.LocalConfig["security.syscalls.intercept.bpf.devices"] = "true"
	incus.Reconcile.Instance.LocalDevices["e2e-vsock"] = charDevice("/dev/vsock")
	incus.Reconcile.Instance.LocalDevices["e2e-vhost-vsock"] = charDevice("/dev/vhost-vsock")
	incus.Reconcile.Instance.LocalDevices["e2e-tun"] = charDevice("/dev/net/tun")
	assertStageConverged(t, runtime, true, "nested VM boundary")
	delete(incus.Reconcile.Instance.LocalDevices, "e2e-vhost-vsock")
	assertStageConverged(t, runtime, false, "missing vhost-vsock mapping")

	runtime.Yard.YardKind = domain.YardVM
	incus.Reconcile.Instance.LocalConfig = nil
	incus.Reconcile.Instance.LocalDevices = map[string]map[string]string{
		"srv": {"source": "yard-srv", "path": "/srv", "pool": "default"},
		"subyard-e2e-routes": {
			"type": "disk", "source": "/data/e2e/routes",
			"path": "/var/lib/subyard/e2e-routes", "readonly": "true",
		},
	}
	assertStageConverged(t, runtime, true, "VM volume")
}

func TestMountProbeDetectsMissingDriftedAndStaleDevices(t *testing.T) {
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
			LocalDevices: map[string]map[string]string{"host-cache": {
				"source": "/srv/subyard/host-cache", "path": "/mnt/cache",
			}},
		}},
	}
	runtime := Runtime{
		Incus: incus, Yard: domain.Context{Paths: domain.RuntimePaths{HostBase: "/srv/subyard"}},
		Environment: []string{"HOST_MOUNTS=host-cache:/mnt/cache:rw:0755"},
	}
	assertStage(t, runtime, "mounts", true, "matching mount")
	incus.Reconcile.Instance.LocalDevices["host-cache"]["path"] = "/wrong"
	assertStage(t, runtime, "mounts", false, "drifted mount")
	incus.Reconcile.Instance.LocalDevices["host-cache"]["path"] = "/mnt/cache"
	incus.Reconcile.Instance.LocalDevices["host-old"] = map[string]string{}
	assertStage(t, runtime, "mounts", false, "stale mount")
}

func TestExtrasDesiredStateIsParsedAndValidatedInGo(t *testing.T) {
	root := t.TempDir()
	writeProfile := func(name, contents string) {
		t.Helper()
		directory := filepath.Join(root, "config", "profiles", name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "profile.conf"),
			[]byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile("a", "YARD_MOUNTS='cache:/srv/cache:rw:0755'\nYARD_CAPS='fuse'\n")
	writeProfile("b", "YARD_CAPS='rootless-docker fuse'\nYARD_DEVICES='gpu'\n")
	runtime := Runtime{RepositoryRoot: root, Environment: []string{"ENVIRONMENT_PROFILES=a"}}
	values, err := runtime.extrasContext()
	if err != nil {
		t.Fatal(err)
	}
	if values["SUBYARD_EXTRAS_MOUNTS"] != "cache:/srv/cache:rw:0755" ||
		values["SUBYARD_EXTRAS_CAPABILITIES"] != "fuse" ||
		values["SUBYARD_EXTRAS_DEVICES"] != "" {
		t.Fatalf("unselected profile leaked extras: %#v", values)
	}
	runtime.Environment = []string{"ENVIRONMENT_PROFILES=a b"}
	values, err = runtime.extrasContext()
	if err != nil {
		t.Fatal(err)
	}
	if values["SUBYARD_EXTRAS_MOUNTS"] != "cache:/srv/cache:rw:0755" ||
		values["SUBYARD_EXTRAS_CAPABILITIES"] != "fuse rootless-docker" ||
		values["SUBYARD_EXTRAS_DEVICES"] != "gpu" {
		t.Fatalf("unexpected extras context: %#v", values)
	}
	runtime.Yard.YardKind = domain.YardVM
	values, err = runtime.extrasContext()
	if err != nil || values["SUBYARD_EXTRAS_DEVICES"] != "" {
		t.Fatalf("VM inherited container-only device extras: %#v, %v", values, err)
	}
	writeProfile("bad", "YARD_MOUNTS='../escape:/srv/cache:rw:0755'\n")
	runtime.Environment = []string{"ENVIRONMENT_PROFILES=bad"}
	if _, err := runtime.extrasContext(); err == nil {
		t.Fatal("unsafe profile extras were accepted")
	}
}

func assertStageConverged(t *testing.T, runtime Runtime, want bool, label string) {
	assertStage(t, runtime, "instance", want, label)
}

func assertStage(t *testing.T, runtime Runtime, stage ports.ReconcileStageID, want bool, label string) {
	t.Helper()
	got, err := runtime.CheckStage(context.Background(), stage)
	if err != nil || got != want {
		t.Fatalf("%s: converged=%v, want %v, error=%v", label, got, want, err)
	}
}

func charDevice(path string) map[string]string {
	return map[string]string{"type": "unix-char", "source": path, "path": path}
}
