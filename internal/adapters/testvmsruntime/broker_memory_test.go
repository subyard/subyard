package testvmsruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBrokerMemoryBudgetAndGuestBoundary(t *testing.T) {
	proc, root, write := memoryFixture(t)
	write(filepath.Join(proc, "meminfo"), "MemAvailable: 16777216 kB\nSwapFree: 999999999 kB\n")
	write(filepath.Join(proc, "self/cgroup"), "0::/"+brokerMemorySlice+"\n")
	write(filepath.Join(root, "memory.max"), "34359738368\n")
	write(filepath.Join(root, "memory.current"), "8589934592\n")
	for name, value := range map[string]string{
		"memory.max": "2147483648", "memory.current": "134217728",
		"memory.swap.max": "2147483648", "memory.swap.current": "33554432",
	} {
		write(filepath.Join(root, brokerMemorySlice, name), value)
	}
	own, err := brokerMemoryCapacity(root)
	if err != nil || own.RAMLimit+own.SwapLimit != 4<<30 || own.EffectiveRAMLimit != 2<<30 || own.Current != 128<<20 || own.SwapCurrent != 32<<20 || !own.AllocationScopeVerified {
		t.Fatalf("own memory budget: %+v %v", own, err)
	}
	guest, err := memoryCapacity(proc, root)
	if err != nil || guest.Available != 16<<30 || guest.Limit != 32<<30 || guest.Current != 8<<30 || guest.VisibleAvailable != 16<<30 || guest.LimitingSource != "visible_meminfo" {
		t.Fatalf("broker cap constrained guest admission or swap added RAM: %+v %v", guest, err)
	}
	write(filepath.Join(root, "memory.max"), "1073741824")
	if _, err := brokerMemoryCapacity(root); err == nil {
		t.Fatal("accepted an ancestor below the required 2GiB RAM budget")
	}
	if _, err := memoryCapacity(proc, root); err == nil {
		t.Fatal("excluded an unverified own-process budget")
	}
}

func TestGuestLaunchMemoryPreservesFiniteDaemonAncestors(t *testing.T) {
	proc, root, write := memoryFixture(t)
	write(filepath.Join(proc, "meminfo"), "MemAvailable: 16777216 kB\n")
	write(filepath.Join(proc, "123/cgroup"), "0::/system.slice/incus.service\n")
	write(filepath.Join(root, "memory.max"), "34359738368")
	write(filepath.Join(root, "memory.current"), "8589934592")
	write(filepath.Join(root, "system.slice/memory.max"), "12884901888")
	write(filepath.Join(root, "system.slice/memory.current"), "8589934592")
	write(filepath.Join(root, "system.slice/incus.service/memory.max"), "max")
	write(filepath.Join(root, "system.slice/incus.service/memory.current"), "4294967296")
	launch, err := memoryCapacityForProcess(proc, root, "123")
	if err != nil || launch.Available != 4<<30 || launch.Limit != 12<<30 || launch.Current != 8<<30 || launch.LimitingSource != "visible_cgroup" {
		t.Fatalf("ignored guest launch ancestor: %+v %v", launch, err)
	}
	physical := capPhysicalMemory(launch, 20<<30)
	if physical.Available != 4<<30 || physical.LimitingSource != "visible_cgroup" {
		t.Fatalf("physical RAM bypassed a launch ceiling: %+v", physical)
	}
}

func TestBrokerTransportPreservesCallerPlacementAndHidesInputs(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "acquire-v3 private-capability")
	t.Setenv("SUBYARD_BROKER_BRIDGE_PID", "123")
	t.Setenv("SUBYARD_BROKER_BRIDGE_START", "456")
	arguments := []string{"_test-vms-facade"}
	command := brokerMemoryCommand("/trusted/engine", arguments, "123", "456")
	joined := strings.Join(command, " ")
	for _, required := range []string{"--slice=" + brokerMemorySlice, "--pipe", "--wait", "--collect", "--service-type=exec", "--expand-environment=no", "--property=LoadCredential=subyard-broker-request:", "--property=KillMode=control-group", "--unit=subyard-test-vms-worker-123-456.service"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("native worker transport omitted %s: %v", required, command)
		}
	}
	if strings.Contains(joined, "private-capability") || strings.Contains(joined, "--scope") || strings.Contains(joined, "--setenv") {
		t.Fatalf("transport exposed inputs or relocated a source process: %v", command)
	}
	for _, membership := range []string{"", "0::/../outside\n", "0::/service (deleted)\n", "0::/\n0::/other\n"} {
		if _, err := brokerCgroupPath(membership); err == nil {
			t.Fatalf("unsafe membership %q accepted", membership)
		}
	}
}

func TestBrokerInputSourceMatchesNativeUnitSpecifier(t *testing.T) {
	arguments := []string{"_test-vms-worker", "${USER}", "$UNSET", "$$", "--yes"}
	command := brokerMemoryCommand("/trusted/engine", arguments, "123", "456")
	unit := ""
	for _, argument := range command {
		if strings.HasPrefix(argument, "--unit=") {
			unit = strings.TrimPrefix(argument, "--unit=")
		}
	}
	// systemd's %N is the complete unit name with its type suffix removed.
	if filepath.Base(brokerInputPath("123", "456")) != strings.TrimSuffix(unit, ".service")+".json" {
		t.Fatal("protected source differs from the scoped native credential specifier")
	}
	for index, argument := range arguments {
		if command[len(command)-len(arguments)+index] != argument {
			t.Fatal("raw worker arguments changed before validation")
		}
	}
	if _, _, _, err := brokerInputFilename("request-123-456.json"); err == nil {
		t.Fatal("cleanup accepted a source outside the native unit naming contract")
	}
}

func TestBrokerNamespaceWaiterRequiresExactLauncherBoundary(t *testing.T) {
	own := "/" + brokerMemorySlice
	for _, test := range []struct {
		path, directory string
		pid, parent     int
		want            bool
	}{
		{"/", incusSystemCredentialDirectory, 123, 0, true},
		{"/system.slice/launcher.service", incusSystemCredentialDirectory, 123, 0, true},
		{own, incusSystemCredentialDirectory, 123, 0, false},
		{own + "/subyard-test-vms-worker-123-456.service", incusSystemCredentialDirectory, 123, 0, false},
		{"/", "", 123, 0, false},
		{"/", "/run/credentials/foreign.service", 123, 0, false},
		{"/", incusSystemCredentialDirectory, 1, 0, false},
		{"/", incusSystemCredentialDirectory, 123, 1, false},
		{"/", incusSystemCredentialDirectory, 123, 456, false},
	} {
		if got := brokerNeedsNamespaceWaiter(test.path, test.directory, test.pid, test.parent); got != test.want {
			t.Fatalf("namespace waiter eligibility differs: %+v", test)
		}
	}
}

func TestBrokerNamespaceWaiterPreservesArgumentsInputAndExit(t *testing.T) {
	arguments := []string{"$USER", "${UNSET}", "$$", "", "two words", `a"b`, ";"}
	child := append([]string{"-c", `printf '%s\000' "$@"; cat; exit 7`, "fixture"}, arguments...)
	command := brokerNamespaceWaiterCommand("/bin/sh", child)
	process := exec.Command(command[0], command[1:]...)
	process.Stdin = strings.NewReader("synthetic stdin")
	output, err := process.Output()
	var failure *exec.ExitError
	if !errors.As(err, &failure) || failure.ExitCode() != 7 {
		t.Fatalf("child exit status changed: %v", err)
	}
	want := strings.Join(arguments, "\x00") + "\x00synthetic stdin"
	if string(output) != want {
		t.Fatal("literal arguments or stdin changed across namespace waiter")
	}
}

func TestBrokerNamespaceWaiterSignalsEndRequesterPromptly(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal syscall.Signal
		code   int
	}{{"hangup", syscall.SIGHUP, 129}, {"interrupt", syscall.SIGINT, 130}, {"terminate", syscall.SIGTERM, 143}} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(testkit.TempDir(t), "output")
			testkit.WriteFile(t, output, nil, 0600)
			file, err := os.OpenFile(output, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			command := brokerNamespaceWaiterCommand("/bin/sh", []string{"-c", "printf ready; exec sleep 30"})
			process := exec.Command(command[0], command[1:]...)
			process.Stdout = file
			process.Stderr = file
			process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			// Only this fixture-owned group is removed; the native worker itself
			// observes requester death and systemd collects its separate service.
			defer syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
			ready := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				body, _ := os.ReadFile(output)
				if strings.Contains(string(body), "ready") {
					ready = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
				_ = process.Wait()
				t.Fatal("namespace waiter child did not start")
			}
			if err := process.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- process.Wait() }()
			select {
			case err := <-finished:
				var failure *exec.ExitError
				if !errors.As(err, &failure) || failure.ExitCode() != test.code {
					t.Fatalf("waiter signal status changed: %v", err)
				}
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
				<-finished
				t.Fatal("signal waited for the worker instead of ending its requester")
			}
		})
	}
}

func TestBrokerBridgeIdentityRejectsPIDReuseAndZombie(t *testing.T) {
	body, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	pid, start := strconv.Itoa(os.Getpid()), brokerProcessStart(body)
	if start == "" || !brokerBridgeAlive(pid, start) {
		t.Fatal("live native transport identity was rejected")
	}
	if brokerBridgeAlive(pid, start+"1") {
		t.Fatal("reused PID retained a worker")
	}
	zombie := strings.Replace(string(body), ") ", ") Z ", 1)
	if brokerProcessStart([]byte(zombie)) != "" {
		t.Fatal("zombie transport retained a worker")
	}
}

func TestBrokerProtectedInputsAndRequesterLifetime(t *testing.T) {
	body, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	pid, start := strconv.Itoa(os.Getpid()), brokerProcessStart(body)
	inputs := brokerInputs{BridgePID: pid, BridgeStart: start, RequesterPID: pid, RequesterStart: start,
		Environment: map[string]string{"SSH_ORIGINAL_COMMAND": "renew private-capability", "SUBYARD_TEST_VMS_CONFIG": "/etc/subyard/candidate.env"}}
	encoded, _ := json.Marshal(inputs)
	decoded, err := decodeBrokerInputs(encoded)
	if err != nil || !decoded.alive() || decoded.Environment["SSH_ORIGINAL_COMMAND"] != "renew private-capability" {
		t.Fatalf("protected input round trip failed: %v", err)
	}
	decoded.RequesterStart += "1"
	if decoded.alive() {
		t.Fatal("live detached transport retained a dead/reused requester")
	}
	for _, invalid := range [][]byte{append(encoded, []byte("{}")...), []byte(`{"unknown":"value"}`), bytes.Repeat([]byte("x"), (80<<10)+1)} {
		if _, err := decodeBrokerInputs(invalid); err == nil {
			t.Fatal("accepted unbounded or untrusted handoff")
		}
	}
	inputs.Environment["UNRELATED_SECRET"] = "private"
	encoded, _ = json.Marshal(inputs)
	if _, err := decodeBrokerInputs(encoded); err == nil {
		t.Fatal("forwarded an unrelated ambient input")
	}
}

func TestBrokerInputPublicationAndDirectory(t *testing.T) {
	root := testkit.TempDir(t)
	directory := filepath.Join(root, "handoffs")
	if err := privateBrokerInputDirectory(directory, uint32(os.Getuid())); err != nil {
		t.Fatalf("fresh directory: %v", err)
	}
	if err := privateBrokerInputDirectory(directory, uint32(os.Getuid())); err != nil {
		t.Fatalf("repeat directory: %v", err)
	}
	path := filepath.Join(directory, "subyard-test-vms-worker-123-456.json")
	if err := publishBrokerInputs(path, []byte("protected")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private publication mode: %v %v", info, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("staging remains after publication")
	}
	if err := publishBrokerInputs(path, []byte("replacement")); err == nil {
		t.Fatal("existing handoff overwritten")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "protected" {
		t.Fatal("published handoff changed")
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := privateBrokerInputDirectory(directory, uint32(os.Getuid())); err == nil {
		t.Fatal("unsafe existing mode repaired implicitly")
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if err := privateBrokerInputDirectory(link, uint32(os.Getuid())); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestBrokerInputCrashFilenameAndPIDOne(t *testing.T) {
	for _, name := range []string{"subyard-test-vms-worker-123-456.json", "subyard-test-vms-worker-123-456.json.tmp"} {
		pid, start, staging, err := brokerInputFilename(name)
		if err != nil || pid != "123" || start != "456" || staging != strings.HasSuffix(name, ".tmp") {
			t.Fatalf("owned crash file rejected: %s %v", name, err)
		}
	}
	for _, name := range []string{"subyard-test-vms-worker-1-456.json", "subyard-test-vms-worker-123-0.json.tmp", "subyard-test-vms-worker-0123-456.json", "unowned", "subyard-test-vms-worker-123-456.json.tmp.tmp"} {
		if _, _, _, err := brokerInputFilename(name); err == nil {
			t.Fatalf("unowned crash file accepted: %s", name)
		}
	}
	for _, inputs := range []brokerInputs{{BridgePID: "1", BridgeStart: "2", RequesterPID: "3", RequesterStart: "4"}, {BridgePID: "2", BridgeStart: "3", RequesterPID: "1", RequesterStart: "4"}} {
		body, _ := json.Marshal(inputs)
		if _, err := decodeBrokerInputs(body); err == nil {
			t.Fatal("PID 1 can retain an orphan worker")
		}
	}
}

func TestBrokerInputCrashCleanup(t *testing.T) {
	root := testkit.TempDir(t)
	owner := uint32(os.Getuid())
	body, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	pid, start := strconv.Itoa(os.Getpid()), brokerProcessStart(body)
	live := brokerInputs{BridgePID: pid, BridgeStart: start, RequesterPID: pid, RequesterStart: start}
	encode := func(inputs brokerInputs) []byte {
		t.Helper()
		body, err := json.Marshal(inputs)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	path := func(pid, start string) string {
		return filepath.Join(root, "subyard-test-vms-worker-"+pid+"-"+start+".json")
	}
	final := path(pid, start)
	if err := publishBrokerInputs(final, encode(live)); err != nil {
		t.Fatal(err)
	}
	// Partial staging from the current writer is retained while it is alive.
	staging := path(pid, start) + ".tmp"
	testkit.WriteFile(t, staging, []byte("partial"), 0600)
	// An interrupted writer leaves an undecodable staging file; its PID was reused.
	deadStaging := path(pid, start+"1") + ".tmp"
	testkit.WriteFile(t, deadStaging, nil, 0600)
	// A malformed live request must not prevent cleanup of later dead writers.
	deadFinal := path(pid, start+"2")
	testkit.WriteFile(t, deadFinal, []byte("partial"), 0600)
	if err := cleanupBrokerMemoryInputs(root, owner); err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{final, staging} {
		if _, err := os.Stat(retained); err != nil {
			t.Fatal("live handoff removed")
		}
	}
	for _, removed := range []string{deadStaging, deadFinal} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatal("partial dead handoff retained")
		}
	}
	if err := os.Remove(staging); err != nil {
		t.Fatal(err)
	}
	deadRequester := live
	deadRequester.RequesterStart += "1"
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}
	if err := publishBrokerInputs(final, encode(deadRequester)); err != nil {
		t.Fatal(err)
	}
	if err := cleanupBrokerMemoryInputs(root, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatal("live bridge retained dead requester handoff")
	}
	if _, err := readPrivateBrokerInputFile(final, owner); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("consumer deletion race loses ENOENT")
	}
	// An unrelated malformed entry still reports drift, while owned dead files clean up.
	testkit.WriteFile(t, filepath.Join(root, "unowned"), nil, 0600)
	testkit.WriteFile(t, deadStaging, nil, 0600)
	if err := cleanupBrokerMemoryInputs(root, owner); err == nil {
		t.Fatal("unowned handoff silently accepted")
	}
	if _, err := os.Stat(deadStaging); !os.IsNotExist(err) {
		t.Fatal("unowned file prevented dead-writer cleanup")
	}
}

func TestBrokerCredentialScopeSeparatesIncusSystemContext(t *testing.T) {
	unit := "subyard-test-vms-worker-123-456.service"
	own := "/" + brokerMemorySlice
	for _, path := range []string{"/", "/init.scope", "/system.slice/incus.service"} {
		request, err := brokerRequestCredentialScope(path, incusSystemCredentialDirectory)
		if err != nil || request {
			t.Fatalf("Incus launcher treated as request worker: %v %v", request, err)
		}
	}
	request, err := brokerRequestCredentialScope(own+"/"+unit, "/run/credentials/"+unit)
	if err != nil || !request {
		t.Fatalf("native request credential rejected: %v %v", request, err)
	}
	for _, value := range [][2]string{
		{own, incusSystemCredentialDirectory},
		{own + "/" + unit, incusSystemCredentialDirectory},
		{own + "/subyard-test-vms-broker.service", incusSystemCredentialDirectory},
		{own + "/" + unit, "/run/credentials/foreign.service"},
		{own + "/" + unit + "/.control", "/run/credentials/" + unit},
		{"/init.scope", "/run/credentials/" + unit},
		{"/init.scope", "/dev/.incus-systemd-credentials/"},
		{"/init.scope", "/unrecognized"},
	} {
		if _, err := brokerRequestCredentialScope(value[0], value[1]); err == nil {
			t.Fatalf("foreign credential context accepted: %v", value)
		}
	}
}

func TestRestoreBrokerInputsAllowsAmbientIncusLauncherOnly(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", incusSystemCredentialDirectory)
	t.Setenv("SSH_ORIGINAL_COMMAND", "status")
	t.Setenv("SUBYARD_TEST_VMS_CONFIG", "/protected/staged.env")
	if err := RestoreBrokerMemoryInputs(); err != nil {
		t.Fatalf("Incus bootstrap launcher rejected: %v", err)
	}
	if restoredBrokerInputs != nil || os.Getenv("SSH_ORIGINAL_COMMAND") != "status" || os.Getenv("SUBYARD_TEST_VMS_CONFIG") != "/protected/staged.env" {
		t.Fatal("ambient context restored or altered request inputs")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/credentials/foreign.service")
	if err := RestoreBrokerMemoryInputs(); err == nil {
		t.Fatal("foreign native credential context ignored")
	}
}

func TestBrokerCredentialScopeRequiresNativeRequestWithoutEnvHint(t *testing.T) {
	own := "/" + brokerMemorySlice
	for _, path := range []string{own + "/subyard-test-vms-worker-123-456.service"} {
		request, err := brokerRequestCredentialScope(path, "")
		if err != nil || !request {
			t.Fatalf("missing hint bypassed private native request: %v %v", request, err)
		}
	}
	for _, unit := range []string{"subyard-test-vms-broker.service", "subyard-test-vms-lease-reaper.service"} {
		request, err := brokerRequestCredentialScope(own+"/"+unit, "")
		if err != nil || request {
			t.Fatalf("static direct lifecycle changed: %v %v", request, err)
		}
	}
}

func TestBrokerPrivateHandoffLoadsUnpublishedStagedConfig(t *testing.T) {
	root := testkit.TempDir(t)
	staged := filepath.Join(root, ".test-vms.env.candidate")
	published := filepath.Join(root, "test-vms.env")
	testkit.WriteFile(t, staged, []byte("NESTED_E2E_VMS=1\nE2E_VM_SLOT_COUNT=3\nE2E_MEMORY_RESERVE=128MiB\n"), 0644)
	if _, err := os.Stat(published); !os.IsNotExist(err) {
		t.Fatal("fixture accidentally has published config")
	}
	// Capture the path already loaded by the caller even if its ambient value changes.
	t.Setenv("SUBYARD_TEST_VMS_CONFIG", published)
	t.Setenv("SSH_ORIGINAL_COMMAND", "renew slot-001 synthetic-lease 1 synthetic-capability")
	for _, name := range brokerInputNames {
		if name != "SUBYARD_TEST_VMS_CONFIG" && name != "SSH_ORIGINAL_COMMAND" {
			t.Setenv(name, "")
		}
	}
	inputs := captureBrokerInputs("123", "456", "789", "101112", staged)
	body, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(root, "subyard-test-vms-worker-123-456.json")
	if err := publishBrokerInputs(credential, body); err != nil {
		t.Fatal(err)
	}
	body, err = readPrivateBrokerInputFile(credential, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decodeBrokerInputs(body)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Join(brokerMemoryCommand("/trusted/engine", []string{"_test-vms-worker", "reconcile-pool", "--yes"}, "123", "456"), " ")
	if strings.Contains(command, staged) || strings.Contains(command, "synthetic-capability") || strings.Contains(command, "--setenv") {
		t.Fatal("private launch input exposed in native unit command")
	}
	// The fresh native worker has neither ambient staged path nor request command.
	t.Setenv("SUBYARD_TEST_VMS_CONFIG", "")
	t.Setenv("SSH_ORIGINAL_COMMAND", "")
	if err := restoreBrokerInputEnvironment(restored); err != nil {
		t.Fatal(err)
	}
	runtime, err := LoadRuntime(os.Getenv("SUBYARD_TEST_VMS_CONFIG"), nil, nil)
	if err != nil || !runtime.Config.Enabled || runtime.Config.SlotCount != 3 || runtime.Config.MemoryReserve != "128MiB" || runtime.ConfigPath != staged {
		t.Fatalf("private handoff loaded default/incorrect config: %v", err)
	}
	if os.Getenv("SSH_ORIGINAL_COMMAND") != inputs.Environment["SSH_ORIGINAL_COMMAND"] {
		t.Fatal("protected request input not restored")
	}
	delete(restored.Environment, "SUBYARD_TEST_VMS_CONFIG")
	if err := restoreBrokerInputEnvironment(restored); err == nil {
		t.Fatal("missing protected config input silently fell back to published config")
	}
}
