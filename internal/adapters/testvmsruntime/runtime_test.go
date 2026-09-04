package testvmsruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeRunner struct {
	calls    [][]string
	contexts []context.Context
	handler  func(string, []string, []string, io.Reader) ([]byte, []byte, error)
	missing  map[string]bool
}

type requestDeadlineRunner struct {
	project         string
	requestDeadline time.Time
	cancelRequest   context.CancelFunc
	trimDeadline    time.Time
	trimTimedOut    bool
	stopAttempted   bool
	stopContextErr  error
	stopped         bool
}

type pairRequestBudgetRunner struct {
	project           string
	cancelRequest     context.CancelFunc
	facadeLimit       time.Duration
	firstTrimDeadline time.Time
	elapsed           time.Duration
	trimAttempts      []string
	stopAttempts      []string
	stopContextErr    error
	stopped           map[string]bool
	verifiedStops     []string
}

func (runner *pairRequestBudgetRunner) consume(ctx context.Context, duration time.Duration) error {
	if duration > 0 {
		runner.elapsed += duration
	}
	if runner.elapsed >= runner.facadeLimit {
		runner.cancelRequest()
	}
	return context.Cause(ctx)
}

func (runner *pairRequestBudgetRunner) Run(
	ctx context.Context,
	_ string,
	arguments []string,
	_ []string,
	_ io.Reader,
) ([]byte, []byte, error) {
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "fstrim -av") {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, nil, errors.New("trim command has no outer deadline")
		}
		vm := arguments[1]
		runner.trimAttempts = append(runner.trimAttempts, vm)
		trimBudget := time.Until(deadline)
		if runner.firstTrimDeadline.IsZero() {
			runner.firstTrimDeadline = deadline
		} else if deadline.Equal(runner.firstTrimDeadline) {
			// The first trim already consumed this shared virtual deadline.
			trimBudget = 0
		}
		if err := runner.consume(ctx, trimBudget); err != nil {
			return []byte("sensitive guest trim output"), nil, err
		}
		return []byte("sensitive guest trim output"), nil, context.DeadlineExceeded
	}
	for _, vm := range []string{"e2e-vm-1", "e2e-vm-2"} {
		if joined == "stop "+vm+" --project "+runner.project+" --timeout 60" {
			runner.stopAttempts = append(runner.stopAttempts, vm)
			if err := context.Cause(ctx); err != nil {
				runner.stopContextErr = err
				return nil, nil, err
			}
			if err := runner.consume(ctx, 5*time.Second); err != nil {
				runner.stopContextErr = err
				return nil, nil, err
			}
			runner.stopped[vm] = true
			return nil, nil, nil
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, nil, err
	}
	switch joined {
	case "project list --format csv -c n":
		return []byte(runner.project + "\n"), nil, nil
	case "project get " + runner.project + " user.subyard.managed":
		return []byte(managedMarker + "\n"), nil, nil
	case "info e2e-vm-1 --project " + runner.project,
		"info e2e-vm-2 --project " + runner.project:
		return nil, nil, nil
	case "list e2e-vm-1 --project " + runner.project + " -f csv -c s":
		return runner.vmState(ctx, "e2e-vm-1")
	case "list e2e-vm-2 --project " + runner.project + " -f csv -c s":
		return runner.vmState(ctx, "e2e-vm-2")
	}
	if len(arguments) > 0 && arguments[0] == "exec" {
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected call: %s", joined)
}

func (runner *pairRequestBudgetRunner) vmState(
	ctx context.Context,
	vm string,
) ([]byte, []byte, error) {
	if !runner.stopped[vm] {
		return []byte("RUNNING\n"), nil, nil
	}
	if err := runner.consume(ctx, 5*time.Second); err != nil {
		return nil, nil, err
	}
	runner.verifiedStops = append(runner.verifiedStops, vm)
	return []byte("STOPPED\n"), nil, nil
}

func (*pairRequestBudgetRunner) LookPath(name string) (string, error) {
	return "/fixture/" + name, nil
}

func (runner *requestDeadlineRunner) Run(
	ctx context.Context,
	_ string,
	arguments []string,
	_ []string,
	_ io.Reader,
) ([]byte, []byte, error) {
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "fstrim -av") {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, nil, errors.New("trim command has no outer deadline")
		}
		runner.trimDeadline = deadline
		runner.trimTimedOut = true
		if deadline.After(runner.requestDeadline.Add(-20 * time.Second)) {
			runner.cancelRequest()
		}
		return nil, nil, context.DeadlineExceeded
	}
	if joined == "stop e2e-vm-1 --project "+runner.project+" --timeout 60" {
		runner.stopAttempted = true
		runner.stopContextErr = context.Cause(ctx)
		if runner.stopContextErr != nil {
			return nil, nil, runner.stopContextErr
		}
		runner.stopped = true
		return nil, nil, nil
	}
	if err := context.Cause(ctx); err != nil {
		return nil, nil, err
	}
	switch joined {
	case "project list --format csv -c n":
		return []byte(runner.project + "\n"), nil, nil
	case "project get " + runner.project + " user.subyard.managed":
		return []byte(managedMarker + "\n"), nil, nil
	case "info e2e-vm-1 --project " + runner.project:
		return nil, nil, nil
	case "info e2e-vm-2 --project " + runner.project:
		return nil, nil, errors.New("not found")
	case "list e2e-vm-1 --project " + runner.project + " -f csv -c s":
		if runner.stopped {
			return []byte("STOPPED\n"), nil, nil
		}
		return []byte("RUNNING\n"), nil, nil
	}
	if len(arguments) > 0 && arguments[0] == "exec" {
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected call: %s", joined)
}

func (*requestDeadlineRunner) LookPath(name string) (string, error) {
	return "/fixture/" + name, nil
}

func (runner *fakeRunner) Run(
	ctx context.Context,
	name string,
	arguments []string,
	environment []string,
	stdin io.Reader,
) ([]byte, []byte, error) {
	call := append([]string{name}, arguments...)
	runner.calls = append(runner.calls, call)
	runner.contexts = append(runner.contexts, ctx)
	if runner.handler != nil {
		return runner.handler(name, arguments, environment, stdin)
	}
	return nil, nil, nil
}

func (runner *fakeRunner) LookPath(name string) (string, error) {
	if runner.missing[name] {
		return "", errors.New("missing")
	}
	return "/fixture/" + name, nil
}

func fixtureConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		Enabled: true, Project: "subyard-e2e-vms", Prefix: "e2e-vm",
		Image: "images:debian/13/cloud", CPU: 2, Memory: "1GiB", Disk: "10GiB",
		SlotCount: 2, BootTimeout: 30 * time.Second, DevUser: "dev",
		StateDir:  filepath.Join(root, "state"),
		AgentUser: "root", AgentPublicKey: fixturePublicKey(t),
		AgentHome:     filepath.Join(root, "agent"),
		StatusCommand: "sudo -n " + DefaultInstalledPath + " _test-vms-facade", Incus: "incus",
	}
	cfg.AgentAuthorizedKeys = filepath.Join(cfg.AgentHome, ".ssh", "authorized_keys")
	return cfg
}

func fixturePublicKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func TestCloudConfigLeavesToolchainToExplicitReconciliation(t *testing.T) {
	cfg := fixtureConfig(t)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	publicKey := fixturePublicKey(t)
	if err := os.WriteFile(cfg.keyPath()+".pub", []byte(publicKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := (&Runtime{Config: cfg}).cloudConfig()
	if !strings.Contains(payload, "name: dev") ||
		!strings.Contains(payload, publicKey) {
		t.Fatalf("cloud config omitted the dev bootstrap: %s", payload)
	}
	for _, duplicate := range []string{"package_update:", "packages:", "runcmd:"} {
		if strings.Contains(payload, duplicate) {
			t.Fatalf("cloud config retained duplicate provisioning %q: %s", duplicate, payload)
		}
	}
}

func TestEnsureGuestToolsReconcilesZshOnRetainedVM(t *testing.T) {
	cfg := fixtureConfig(t)
	var checkedZsh, installedZsh, usedCurrentRevision bool
	var installArguments []string
	installCommands := 0
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		switch {
		case strings.Contains(joined, "e2e-dependencies.revision") &&
			strings.Contains(joined, "command -v git"):
			checkedZsh = strings.Contains(joined, "command -v zsh")
			usedCurrentRevision = strings.Contains(joined,
				"DEPENDENCY_REVISION=subyard-test-vms-dependencies-v2")
			return nil, nil, errors.New("retained VM is missing zsh")
		case strings.Contains(joined, "apt-get") && strings.Contains(joined, "build-essential"):
			installCommands++
			installArguments = append([]string(nil), arguments...)
			installedZsh = strings.Contains(joined, " zsh")
			if !installedZsh {
				return nil, nil, errors.New("toolchain install omitted zsh")
			}
			return nil, nil, nil
		case strings.Contains(joined, "passwd --status dev"):
			return []byte("dev P 2026-08-06 0 99999 7 -1\n"), nil, nil
		case strings.Contains(joined, "sshd -T"):
			return []byte("passwordauthentication no\n"), nil, nil
		default:
			return nil, nil, nil
		}
	}}
	runtime := &Runtime{
		Config: cfg, Runner: runner, Stdout: io.Discard, Now: time.Now,
	}
	if err := runtime.ensureGuestTools(context.Background(), "e2e-vm-2"); err != nil {
		t.Fatal(err)
	}
	if !checkedZsh || !installedZsh || !usedCurrentRevision {
		t.Fatalf("toolchain reconciliation is incomplete: checked-zsh=%t installed-zsh=%t current-revision=%t",
			checkedZsh, installedZsh, usedCurrentRevision)
	}
	if installCommands != 1 {
		t.Fatalf("toolchain install commands=%d, want 1", installCommands)
	}
	timeoutIndex := slices.Index(installArguments, "timeout")
	if timeoutIndex < 0 || timeoutIndex+5 > len(installArguments) || !slices.Equal(
		installArguments[timeoutIndex:timeoutIndex+5],
		[]string{"timeout", "--signal=TERM", "--kill-after=10", "1800", "sh"},
	) {
		t.Fatalf("toolchain command has no hard 1800-second wrapper: %q", installArguments)
	}
	script := installArguments[len(installArguments)-1]
	if strings.Count(script, "apt-get") != 2 ||
		!strings.Contains(script, " update -qq") ||
		!strings.Contains(script, " install -y -qq") ||
		strings.Contains(script, "timeout ") {
		t.Fatalf("bounded toolchain script does not contain one update+install phase: %s", script)
	}
}

func TestGuestToolchainTimeoutKillsTermIgnoringDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	const script = `trap '' TERM
sleep 30 &
child=$!
printf '%s\n' "$child" > "$PID_FILE"
wait "$child"`
	command := guestToolchainCommand(script, time.Second, time.Second)
	started := time.Now()
	_, _, err := (ProcessRunner{}).Run(
		context.Background(), command[0], command[1:], []string{"PID_FILE=" + pidFile}, nil,
	)
	if err == nil {
		t.Fatal("toolchain timeout unexpectedly accepted a TERM-ignoring descendant")
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("toolchain timeout elapsed=%s, want at most 4s", elapsed)
	}
	payload, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(payload)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	for attempt := 0; attempt < 20; attempt++ {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return
		}
		stat, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if closingParen := bytes.LastIndex(stat, []byte(") ")); statErr == nil &&
			closingParen >= 0 && len(stat) > closingParen+2 && stat[closingParen+2] == 'Z' {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("TERM-ignoring toolchain descendant %d survived the KILL deadline", pid)
}

func TestEnsureVMRetriesTransientRemoteImageLookup(t *testing.T) {
	cfg := fixtureConfig(t)
	initAttempts := 0
	sleeps := 0
	runner := &fakeRunner{handler: func(
		name string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		switch {
		case joined == "info e2e-vm-1 --project "+cfg.Project:
			return nil, nil, errors.New("not found")
		case strings.HasPrefix(joined, "init "+cfg.Image+" e2e-vm-1 --vm"):
			initAttempts++
			if initAttempts < 3 {
				return nil, nil, &CommandError{
					Name: name, Args: arguments, ExitCode: 1,
					Message: "Failed getting remote image info: " +
						"Failed getting image: The requested image couldn't be found",
				}
			}
			return nil, nil, nil
		default:
			return nil, nil, nil
		}
	}}
	runtime := &Runtime{
		Config: cfg, Runner: runner, Stdout: io.Discard, Now: time.Now,
		Sleep: func(context.Context, time.Duration) error {
			sleeps++
			return nil
		},
	}
	if err := runtime.ensureVM(context.Background(), "e2e-vm-1"); err != nil {
		t.Fatal(err)
	}
	if initAttempts != 3 || sleeps != 2 {
		t.Fatalf("remote image retries = attempts %d, sleeps %d; want 3, 2",
			initAttempts, sleeps)
	}
}

func TestAcquireSlotRejectsInsufficientCapacityBeforeMutation(t *testing.T) {
	runtime := &Runtime{Config: fixtureConfig(t)}
	runtime.AvailableBytes = func(string) (uint64, error) {
		return HostReserveBytes + 2*InitialVMHeadroomBytes - 1, nil
	}
	var mutations int
	runtime.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		if strings.HasPrefix(joined, "info ") {
			return nil, nil, errors.New("not found")
		}
		mutations++
		return nil, nil, fmt.Errorf("unexpected mutation: %s", joined)
	}}
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
	grant, err := store.AcquireSlot("client", "SHA256:key", "", "", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.AcquireSlot(context.Background(), store, grant, fixturePublicKey(t))
	if err == nil || !strings.Contains(err.Error(), "insufficient test-vms pool capacity") {
		t.Fatalf("capacity error = %v", err)
	}
	if mutations != 0 {
		t.Fatalf("capacity preflight performed %d mutation(s)", mutations)
	}
}

func TestInstallLeaseContextUsesBoundedAtomicGuestFile(t *testing.T) {
	cfg := fixtureConfig(t)
	var guestArguments []string
	runner := &fakeRunner{handler: func(
		name string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		if name != "incus" || len(arguments) < 6 || arguments[0] != "exec" {
			return nil, nil, fmt.Errorf("unexpected call: %s %s", name, strings.Join(arguments, " "))
		}
		guestArguments = append([]string(nil), arguments...)
		return nil, nil, nil
	}}
	runtime := &Runtime{Config: cfg, Runner: runner}
	leaseContext := LeaseContext{
		SchemaVersion: LeaseAttributionSchemaVersion,
		Yard:          "default", Project: "Subyard-2", Run: "run-a", Purpose: "unit-tests",
	}
	grant := LeaseGrant{SlotID: "slot-001", Context: &leaseContext}
	if err := runtime.installLeaseContext(
		context.Background(), "e2e-vm-1", grant,
	); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(guestArguments, "\n")
	for _, expected := range []string{
		`"yard":"default"`,
		`"project":"Subyard-2"`,
		`"run":"run-a"`,
		`"purpose":"unit-tests"`,
		`"slot":"slot-001"`,
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("guest context omitted %s: %s", expected, joined)
		}
	}
	call := strings.Join(runner.calls[0], " ")
	for _, expected := range []string{
		"mktemp /var/lib/subyard/.e2e-lease-context.XXXXXX",
		`mv -f "$temp" "$state"`,
		"subyard-e2e-lease-context.service",
		"systemctl enable",
		`cmp -s "$state" "$target"`,
	} {
		if !strings.Contains(call, expected) {
			t.Fatalf("guest context omitted reboot-safe atomic contract %q: %s", expected, call)
		}
	}
}

func TestRemoveLeaseContextClearsRuntimeAndPersistentState(t *testing.T) {
	runner := &fakeRunner{}
	runtime := &Runtime{Config: fixtureConfig(t), Runner: runner}
	if err := runtime.removeLeaseContext(context.Background(), "e2e-vm-1"); err != nil {
		t.Fatal(err)
	}
	call := strings.Join(runner.calls[0], " ")
	if !strings.Contains(call, "/run/subyard-e2e-lease.json") ||
		!strings.Contains(call, "/var/lib/subyard/e2e-lease-context.json") {
		t.Fatalf("lease cleanup omitted state: %s", call)
	}
}

func TestStopRunningVMAcceptsConcurrentSuccessfulStop(t *testing.T) {
	cfg := fixtureConfig(t)
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "stop e2e-vm-1 --project subyard-e2e-vms --timeout 60":
			return nil, nil, errors.New("matching non-reusable operation has now succeeded")
		case "list e2e-vm-1 --project subyard-e2e-vms -f csv -c s":
			return []byte("STOPPED\n"), nil, nil
		default:
			return nil, nil, fmt.Errorf("unexpected call: %s", strings.Join(arguments, " "))
		}
	}}
	runtime := &Runtime{Config: cfg, Runner: runner}
	if err := runtime.stopRunningVM(context.Background(), "e2e-vm-1"); err != nil {
		t.Fatalf("concurrent successful stop was rejected: %v", err)
	}
}

func TestReleaseStopsRebootingGuestWhenKeyCleanupIsTemporarilyUnavailable(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	cfg.AgentPublicKey = ""
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.keyPath()+".pub",
		[]byte(fixturePublicKey(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := false
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		switch joined {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "info e2e-vm-1 --project " + cfg.Project:
			return nil, nil, nil
		case "info e2e-vm-2 --project " + cfg.Project:
			return nil, nil, errors.New("not found")
		case "list e2e-vm-1 --project " + cfg.Project + " -f csv -c s":
			if stopped {
				return []byte("STOPPED\n"), nil, nil
			}
			return []byte("RUNNING\n"), nil, nil
		case "stop e2e-vm-1 --project " + cfg.Project + " --timeout 60":
			stopped = true
			return nil, nil, nil
		}
		if len(arguments) > 0 && arguments[0] == "exec" {
			return nil, nil, errors.New("VM agent isn't currently running")
		}
		return nil, nil, fmt.Errorf("unexpected call: %s", joined)
	}}
	var warnings bytes.Buffer
	runtime := &Runtime{Config: cfg, Runner: runner, Stderr: &warnings}
	if err := runtime.stopRetained(context.Background()); err != nil {
		t.Fatalf("rebooting guest was not stopped: %v", err)
	}
	if !strings.Contains(callsText(runner.calls),
		"incus stop e2e-vm-1 --project "+cfg.Project+" --timeout 60") {
		t.Fatal("release did not stop the rebooting guest after key cleanup failed")
	}
	if !strings.Contains(warnings.String(), "guest lease cleanup deferred") {
		t.Fatalf("deferred cleanup warning = %q", warnings.String())
	}
}

func TestReleaseTrimsRunningRetainedVMAfterCleanupBeforeVerifiedStop(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.keyPath()+".pub",
		[]byte(fixturePublicKey(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.AgentAuthorizedKeys), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.AgentAuthorizedKeys, []byte("still-reachable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := false
	trace := []string{}
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "info e2e-vm-1 --project " + cfg.Project:
			return nil, nil, nil
		case "info e2e-vm-2 --project " + cfg.Project:
			return nil, nil, errors.New("not found")
		case "list e2e-vm-1 --project " + cfg.Project + " -f csv -c s":
			if stopped {
				trace = append(trace, "stopped-verified")
				return []byte("STOPPED\n"), nil, nil
			}
			return []byte("RUNNING\n"), nil, nil
		case "stop e2e-vm-1 --project " + cfg.Project + " --timeout 60":
			stopped = true
			trace = append(trace, "stop")
			return nil, nil, nil
		}
		if len(arguments) > 0 && arguments[0] == "exec" {
			payload, err := os.ReadFile(cfg.AgentAuthorizedKeys)
			if err != nil || strings.Contains(string(payload), "port-forwarding") ||
				strings.Contains(string(payload), "still-reachable") {
				return nil, nil, fmt.Errorf("guest command ran before host fence: %q, %v", payload, err)
			}
			if len(trace) == 0 {
				trace = append(trace, "host-fenced")
			}
			joined := strings.Join(arguments, " ")
			switch {
			case strings.Contains(joined, "WORKER_KEY="):
				trace = append(trace, "managed-key-dev")
			case strings.Contains(joined, "ssh_dir=/root/.ssh"):
				trace = append(trace, "managed-key-root")
			case strings.Contains(joined, "/run/subyard-e2e-lease.json"):
				trace = append(trace, "lease-context")
			case strings.Contains(joined, "fstrim -av"):
				trace = append(trace, "trim")
			}
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected call: %s", strings.Join(arguments, " "))
	}}
	runtime := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard}
	if err := runtime.stopRetained(context.Background()); err != nil {
		t.Fatal(err)
	}
	if payload, err := os.ReadFile(cfg.AgentAuthorizedKeys); err != nil ||
		strings.Contains(string(payload), "port-forwarding") ||
		strings.Contains(string(payload), "still-reachable") {
		t.Fatalf("host access fence = %q, %v", payload, err)
	}
	if !strings.Contains(callsText(runner.calls), "incus exec e2e-vm-1 --project "+cfg.Project+
		" -- timeout --signal=TERM --kill-after=10 15 sh -eu -c sync\nfstrim -av") {
		t.Fatalf("trim command was not bounded sync plus fstrim:\n%s", callsText(runner.calls))
	}
	if want := []string{
		"host-fenced", "managed-key-dev", "managed-key-root", "lease-context",
		"trim", "stop", "stopped-verified",
	}; !slices.Equal(trace, want) {
		t.Fatalf("release trace = %v, want %v", trace, want)
	}
}

func TestReleaseContinuesAfterRetainedGuestTrimDeadlineWithoutGuestOutput(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.keyPath()+".pub",
		[]byte(fixturePublicKey(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := false
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "info e2e-vm-1 --project " + cfg.Project:
			return nil, nil, nil
		case "info e2e-vm-2 --project " + cfg.Project:
			return nil, nil, errors.New("not found")
		case "list e2e-vm-1 --project " + cfg.Project + " -f csv -c s":
			if stopped {
				return []byte("STOPPED\n"), nil, nil
			}
			return []byte("RUNNING\n"), nil, nil
		case "stop e2e-vm-1 --project " + cfg.Project + " --timeout 60":
			stopped = true
			return nil, nil, nil
		}
		if len(arguments) > 0 && arguments[0] == "exec" {
			if strings.Contains(strings.Join(arguments, " "), "fstrim -av") {
				return []byte("secret guest output"), nil, context.DeadlineExceeded
			}
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected call: %s", strings.Join(arguments, " "))
	}}
	var warnings bytes.Buffer
	runtime := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: &warnings}
	if err := runtime.stopRetained(context.Background()); err != nil {
		t.Fatalf("trim deadline prevented release: %v", err)
	}
	trimIndex := -1
	for index, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "fstrim -av") {
			trimIndex = index
			break
		}
	}
	if trimIndex < 0 {
		t.Fatal("release did not attempt retained guest trim")
	}
	deadline, ok := runner.contexts[trimIndex].Deadline()
	if !ok {
		t.Fatal("retained guest trim Incus execution has no outer deadline")
	}
	if remaining := time.Until(deadline); remaining < retainedGuestTrimTotalTimeout-time.Second ||
		remaining > retainedGuestTrimTotalTimeout+time.Second {
		t.Fatalf("retained guest trim outer deadline remaining=%s, want about %s",
			remaining, retainedGuestTrimTotalTimeout)
	}
	if got, want := warnings.String(), "test-vms: retained guest disk trim failed; continuing to stop\n"; got != want {
		t.Fatalf("trim warning = %q, want %q", got, want)
	}
	if strings.Contains(warnings.String(), "secret guest output") {
		t.Fatalf("trim warning leaked guest output: %q", warnings.String())
	}
	if !strings.Contains(callsText(runner.calls),
		"incus stop e2e-vm-1 --project "+cfg.Project+" --timeout 60") {
		t.Fatal("trim deadline did not continue to verified stop")
	}
}

func TestRetainedGuestTrimTimeoutLeavesRequestContextForVerifiedStop(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.keyPath()+".pub",
		[]byte(fixturePublicKey(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &requestDeadlineRunner{project: cfg.Project}
	runtime := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard}
	requestDeadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), requestDeadline)
	defer cancel()
	runner.requestDeadline = requestDeadline
	runner.cancelRequest = cancel
	if err := runtime.stopRetained(ctx); err != nil {
		t.Fatalf("trim timeout consumed the stop context: %v", err)
	}
	if !runner.trimTimedOut {
		t.Fatal("fixture trim did not reach its bounded deadline")
	}
	if reserve := runner.requestDeadline.Sub(runner.trimDeadline); reserve < 20*time.Second {
		t.Fatalf("trim left %s of the request lifetime for stop, want at least 20s", reserve)
	}
	if !runner.stopAttempted {
		t.Fatal("trim timeout prevented stop")
	}
	if runner.stopContextErr != nil {
		t.Fatalf("stop inherited an exhausted request context: %v", runner.stopContextErr)
	}
	if !runner.stopped {
		t.Fatal("stop was not verified")
	}
}

func TestRetainedGuestPairTrimBudgetsLeaveTimeForBothVerifiedStops(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.keyPath()+".pub",
		[]byte(fixturePublicKey(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &pairRequestBudgetRunner{
		project:     cfg.Project,
		facadeLimit: time.Minute,
		stopped:     map[string]bool{},
	}
	runtime := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard}
	var warnings bytes.Buffer
	runtime.Stderr = &warnings
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(runner.facadeLimit))
	defer cancel()
	runner.cancelRequest = cancel
	if err := runtime.stopRetained(ctx); err != nil {
		t.Fatalf("cumulative trim budgets exhausted the request before both verified stops: %v", err)
	}
	if got, want := runner.trimAttempts, []string{"e2e-vm-1", "e2e-vm-2"}; !slices.Equal(got, want) {
		t.Fatalf("trim attempts = %v, want %v", got, want)
	}
	if got, want := runner.stopAttempts, []string{"e2e-vm-1", "e2e-vm-2"}; !slices.Equal(got, want) {
		t.Fatalf("stop attempts = %v, want %v", got, want)
	}
	if got, want := runner.verifiedStops, []string{"e2e-vm-1", "e2e-vm-2"}; !slices.Equal(got, want) {
		t.Fatalf("verified stops = %v, want %v", got, want)
	}
	if runner.stopContextErr != nil {
		t.Fatalf("stop inherited an exhausted request context: %v", runner.stopContextErr)
	}
	if runner.elapsed >= runner.facadeLimit {
		t.Fatalf("pair release consumed %s, facade limit %s", runner.elapsed, runner.facadeLimit)
	}
	if got, want := warnings.String(), strings.Repeat(
		"test-vms: retained guest disk trim failed; continuing to stop\n", 2,
	); got != want {
		t.Fatalf("trim warnings = %q, want %q", got, want)
	}
	if strings.Contains(warnings.String(), "sensitive guest trim output") {
		t.Fatalf("trim warning leaked guest output: %q", warnings.String())
	}
}

func TestReleaseSlotContinuesAfterOrdinaryRetainedGuestTrimCommandError(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.keyPath()+".pub",
		[]byte(fixturePublicKey(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	grant, err := store.AcquireSlot("client", "SHA256:key", "", "trim-command-error", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkHeld(grant); err != nil {
		t.Fatal(err)
	}
	stopped := false
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "info e2e-vm-1 --project " + cfg.Project:
			return nil, nil, nil
		case "info e2e-vm-2 --project " + cfg.Project:
			return nil, nil, errors.New("not found")
		case "list e2e-vm-1 --project " + cfg.Project + " -f csv -c s":
			if stopped {
				return []byte("STOPPED\n"), nil, nil
			}
			return []byte("RUNNING\n"), nil, nil
		case "stop e2e-vm-1 --project " + cfg.Project + " --timeout 60":
			stopped = true
			return nil, nil, nil
		}
		if len(arguments) > 0 && arguments[0] == "exec" {
			if strings.Contains(strings.Join(arguments, " "), "fstrim -av") {
				return []byte("sensitive guest output"), nil, &CommandError{
					Name: "incus", Args: append([]string(nil), arguments...), ExitCode: 1,
					Message: "ordinary guest trim command failure",
				}
			}
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected call: %s", strings.Join(arguments, " "))
	}}
	var warnings bytes.Buffer
	runtime := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: &warnings}
	runtime.finishDrain = func(ctx context.Context, store LeaseStore, slot LeaseSlot) error {
		evidence, stopErr := runtime.stopRetainedWithEvidence(ctx)
		runtime.recordStopOutcome(slot, evidence, stopErr)
		return store.FinishDrain(slot.SlotID, stopErr)
	}
	if err := runtime.ReleaseSlot(context.Background(), store, grant); err != nil {
		t.Fatalf("ordinary trim command error prevented release: %v", err)
	}
	if got, want := warnings.String(), "test-vms: retained guest disk trim failed; continuing to stop\n"; got != want {
		t.Fatalf("trim warning = %q, want %q", got, want)
	}
	for _, secret := range []string{"sensitive guest output", "ordinary guest trim command failure"} {
		if strings.Contains(warnings.String(), secret) {
			t.Fatalf("trim warning leaked guest detail %q: %q", secret, warnings.String())
		}
	}
	if !strings.Contains(callsText(runner.calls),
		"incus stop e2e-vm-1 --project "+cfg.Project+" --timeout 60") {
		t.Fatal("ordinary trim command error did not continue to stop")
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got := pool.Slots[0].State; got != SlotAvailable {
		t.Fatalf("slot state after ordinary trim command error = %s, want %s", got, SlotAvailable)
	}
}

func TestReleaseDoesNotTrimStoppedRetainedVM(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "info e2e-vm-1 --project " + cfg.Project:
			return nil, nil, nil
		case "info e2e-vm-2 --project " + cfg.Project:
			return nil, nil, errors.New("not found")
		case "list e2e-vm-1 --project " + cfg.Project + " -f csv -c s":
			return []byte("STOPPED\n"), nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected call: %s", strings.Join(arguments, " "))
	}}
	runtime := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard}
	if err := runtime.stopRetained(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(callsText(runner.calls), "fstrim -av") ||
		strings.Contains(callsText(runner.calls), "incus stop e2e-vm-1") {
		t.Fatalf("stopped retained VM received trim or stop:\n%s", callsText(runner.calls))
	}
}

func TestUnavailableInnerIncusIsNotTreatedAsAnAbsentProject(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Project += "-slot-1"
	daemonFailure := &CommandError{
		Name:     "incus",
		Args:     []string{"project", "list", "--format", "csv", "-c", "n"},
		ExitCode: 1,
		Message:  "Error: Failed to connect to local daemon",
	}
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		if strings.Join(arguments, " ") == "project list --format csv -c n" {
			return nil, nil, daemonFailure
		}
		return nil, nil, fmt.Errorf("unexpected call: %s", strings.Join(arguments, " "))
	}}
	runtime := &Runtime{Config: cfg, Runner: runner}

	err := runtime.stopRetained(context.Background())
	if err == nil ||
		!strings.Contains(err.Error(), "inventory retained slot project") ||
		!strings.Contains(err.Error(), "Failed to connect to local daemon") {
		t.Fatalf("release inventory error = %v", err)
	}
	err = runtime.deleteManagedPairForRebuild(context.Background())
	if err == nil ||
		!strings.Contains(err.Error(), "inventory quarantined slot project before delete") {
		t.Fatalf("rebuild inventory error = %v", err)
	}
	if strings.Contains(callsText(runner.calls), "incus delete ") ||
		strings.Contains(callsText(runner.calls), "incus init ") {
		t.Fatalf("failed inventory allowed mutation:\n%s", callsText(runner.calls))
	}
}

func TestQuarantineLocalSpoolFailureBlocksDestructiveRecovery(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "spool"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig(t)
	cfg.StateDir = stateRoot
	store := LeaseStore{
		Path:      filepath.Join(root, "leases.json"),
		SlotCount: 1,
	}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "fsync-gate", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(grant, errors.New("stop timeout")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{handler: func(
		_ string,
		arguments []string,
		_ []string,
		_ io.Reader,
	) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		if strings.Contains(joined, " delete ") || strings.HasPrefix(joined, "delete ") {
			return nil, nil, errors.New("destructive command must not run")
		}
		return nil, nil, errors.New("diagnostic unavailable")
	}}
	runtime := &Runtime{Config: cfg, Runner: runner}
	err = runtime.HandleQuarantine(
		context.Background(),
		store,
		grant.SlotID,
		errors.New("stop timeout"),
	)
	if err == nil || !strings.Contains(err.Error(), "persist emergency incident") {
		t.Fatalf("local spool failure = %v", err)
	}
	pool, statusErr := store.Status()
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if pool.Slots[0].State != SlotQuarantined ||
		pool.Slots[0].IncidentID != "" ||
		pool.Slots[0].RecoveryAttempt != 0 {
		t.Fatalf("failed local fsync changed recovery state: %#v", pool.Slots[0])
	}
	if strings.Contains(callsText(runner.calls), "incus delete ") {
		t.Fatalf("local fsync failure deleted VM: %s", callsText(runner.calls))
	}
}

func TestFailedRecoveryKeepsTheOriginalIncidentTimeline(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.SlotCount = 1
	cfg.BrokerSource = "test-yard"
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: 1}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "recovery", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(grant, errors.New("stop timeout")); err != nil {
		t.Fatal(err)
	}
	slot, err := storeSlot(store, grant.SlotID)
	if err != nil {
		t.Fatal(err)
	}
	recorder := EventRecorder{StateDir: cfg.StateDir, Source: cfg.BrokerSource}
	incident, err := recorder.SaveIncident(slot, errors.New("stop timeout"), nil)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := recorder.Record(BrokerEvent{
		Kind:               "slot.quarantined",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		FromState:          slot.State,
		ToState:            SlotQuarantined,
		IncidentID:         incident.IncidentID,
		Context:            leaseContextFromSlot(slot),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetQuarantineIncident(
		slot.SlotID,
		quarantined.EventID,
		incident.IncidentID,
	); err != nil {
		t.Fatal(err)
	}

	daemonFailure := &CommandError{
		Name:     "incus",
		Args:     []string{"project", "list", "--format", "csv", "-c", "n"},
		ExitCode: 1,
		Message:  "Error: Failed to connect to local daemon",
	}
	runner := &fakeRunner{handler: func(
		_ string, arguments, _ []string, _ io.Reader,
	) ([]byte, []byte, error) {
		if strings.Join(arguments, " ") == "project list --format csv -c n" {
			return nil, nil, daemonFailure
		}
		return nil, nil, nil
	}}
	runtime := &Runtime{
		Config: cfg,
		Runner: runner,
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
	if err := runtime.RecoverScheduled(
		context.Background(),
		store,
		slot.SlotID,
		true,
	); err == nil {
		t.Fatal("synthetic recovery failure succeeded")
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].IncidentID != incident.IncidentID {
		t.Fatalf(
			"recovery replaced incident %s with %s",
			incident.IncidentID,
			pool.Slots[0].IncidentID,
		)
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Incidents) != 1 ||
		batch.Incidents[0].IncidentID != incident.IncidentID {
		t.Fatalf("recovery incidents = %#v", batch.Incidents)
	}
	for _, kind := range []string{"recovery.start", "recovery.failed", "recovery.scheduled"} {
		found := false
		for _, event := range batch.Events {
			if event.Kind != kind {
				continue
			}
			found = true
			if event.IncidentID != incident.IncidentID {
				t.Fatalf("%s incident = %s", kind, event.IncidentID)
			}
		}
		if !found {
			t.Fatalf("%s event is missing", kind)
		}
	}
}

func TestReconcilePoolRetriesPhysicalShrinkAndRejectsForeignNetwork(t *testing.T) {
	for _, test := range []struct {
		name          string
		networkMarker string
		failFirst     bool
		wantSuccess   bool
	}{
		{name: "partial cleanup retry", networkMarker: managedMarker, failFirst: true, wantSuccess: true},
		{name: "foreign marker", networkMarker: "foreign", wantSuccess: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.SlotCount = 1
			storePath := cfg.leaseState()
			large := LeaseStore{Path: storePath, SlotCount: 2}
			if _, err := large.PrepareResize(); err != nil {
				t.Fatal(err)
			}
			deleteAttempts := 0
			runner := &fakeRunner{handler: func(
				_ string, arguments, _ []string, _ io.Reader,
			) ([]byte, []byte, error) {
				joined := strings.Join(arguments, " ")
				switch joined {
				case "project list --format csv -c n":
					return nil, nil, nil
				case "network show e2e-vm-net-2 --project default":
					return nil, nil, nil
				case "network get e2e-vm-net-2 user.subyard.managed --project default":
					return []byte(test.networkMarker + "\n"), nil, nil
				case "network delete e2e-vm-net-2 --project default":
					deleteAttempts++
					if test.failFirst && deleteAttempts == 1 {
						return nil, nil, errors.New("synthetic delete failure")
					}
					return nil, nil, nil
				default:
					return nil, nil, fmt.Errorf("unexpected call: %s", joined)
				}
			}}
			var output bytes.Buffer
			runtime := &Runtime{
				Config: cfg, Runner: runner, Stdout: &output,
				AvailableBytes: func(string) (uint64, error) {
					return 20 * 1024 * 1024 * 1024, nil
				},
			}
			small := LeaseStore{Path: storePath, SlotCount: 1}
			err := runtime.ReconcilePool(context.Background(), small)
			if test.failFirst && err != nil {
				err = runtime.ReconcilePool(context.Background(), small)
			}
			if test.wantSuccess {
				if err != nil {
					t.Fatal(err)
				}
				pool, statusErr := small.Status()
				if statusErr != nil || len(pool.Slots) != 1 {
					t.Fatalf("shrunk pool = %#v, %v", pool.Slots, statusErr)
				}
				if !strings.Contains(output.String(), "slots 2 -> 1, maximum VMs 2") {
					t.Fatalf("resize plan missing from output: %q", output.String())
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "not Subyard-managed") {
					t.Fatalf("foreign marker error = %v", err)
				}
				payload, readErr := os.ReadFile(storePath)
				if readErr != nil || !bytes.Contains(payload, []byte(`"slot_id": "slot-002"`)) {
					t.Fatalf("retiring state was truncated: %q, %v", payload, readErr)
				}
			}
		})
	}
}

func TestConfigRejectsUnsafeRuntimeValues(t *testing.T) {
	base := map[string]string{"NESTED_E2E_VMS": "1"}
	if _, err := ConfigFromValues(base); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"E2E_VM_PROJECT": "../foreign", "E2E_VM_CPU": "0",
		"E2E_VM_DISK": "9GiB", "E2E_VM_SLOT_COUNT": "0",
		"E2E_VM_BOOT_TIMEOUT": "1801", "E2E_AGENT_HOME": "/home/dev",
		"E2E_AGENT_STATUS_COMMAND": "/bin/sh",
	} {
		values := map[string]string{"NESTED_E2E_VMS": "1", name: value}
		if _, err := ConfigFromValues(values); err == nil {
			t.Errorf("%s=%q was accepted", name, value)
		}
	}
}

func TestVMIPFollowsTheOnlyDefaultRouteInterface(t *testing.T) {
	cfg := fixtureConfig(t)
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch {
		case reflect.DeepEqual(arguments[:2], []string{"exec", cfg.vm(1)}):
			return []byte("default via 10.42.0.1 dev enp5s0 proto dhcp\n"), nil, nil
		case arguments[0] == "list":
			return []byte(`[{"state":{"network":{
				"enp5s0":{"addresses":[{"family":"inet","scope":"global","address":"10.42.0.7"}]},
				"incusbr0":{"addresses":[{"family":"inet","scope":"global","address":"10.99.0.1"}]}
			}}}]`), nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected call: %v", arguments)
	}}
	runtime := Runtime{Config: cfg, Runner: runner}
	address, err := runtime.vmIP(context.Background(), cfg.vm(1))
	if err != nil {
		t.Fatal(err)
	}
	if address != "10.42.0.7" {
		t.Fatalf("address = %q", address)
	}
}

func TestVMIPRejectsAmbiguousDefaultRoutes(t *testing.T) {
	cfg := fixtureConfig(t)
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if arguments[0] == "exec" {
			return []byte("default via 10.42.0.1 dev enp5s0\n" +
				"default via 10.43.0.1 dev enp6s0\n"), nil, nil
		}
		return nil, nil, errors.New("should not inspect addresses")
	}}
	runtime := Runtime{Config: cfg, Runner: runner}
	if _, err := runtime.vmIP(context.Background(), cfg.vm(1)); err == nil {
		t.Fatal("ambiguous routes were accepted")
	}
}

func TestExistingProjectRejectsUnexpectedInstances(t *testing.T) {
	cfg := fixtureConfig(t)
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "list --project " + cfg.Project + " -f csv -c n":
			return []byte("foreign-vm\n"), nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected call: %v", arguments)
	}}
	runtime := Runtime{Config: cfg, Runner: runner}
	if err := runtime.ensureProject(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "unexpected instance") {
		t.Fatalf("error = %v", err)
	}
}

func TestRecoveryDeletesTheEntireOwnedPairAndRejectsForeignInventory(t *testing.T) {
	for _, test := range []struct {
		name        string
		inventory   string
		wantDeletes int
		wantError   string
	}{
		{
			name:        "owned full pair",
			inventory:   "e2e-vm-1\ne2e-vm-2\n",
			wantDeletes: 2,
		},
		{
			name:        "interrupted delete with one owned VM left",
			inventory:   "e2e-vm-2\n",
			wantDeletes: 1,
		},
		{
			name:        "interrupted delete with no VM left",
			inventory:   "",
			wantDeletes: 0,
		},
		{
			name:      "foreign instance",
			inventory: "e2e-vm-1\nforeign-vm\n",
			wantError: "unexpected instance",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			deletes := 0
			runner := &fakeRunner{handler: func(
				_ string,
				arguments []string,
				_ []string,
				_ io.Reader,
			) ([]byte, []byte, error) {
				joined := strings.Join(arguments, " ")
				switch joined {
				case "project list --format csv -c n":
					return []byte(cfg.Project + "\n"), nil, nil
				case "project get " + cfg.Project + " user.subyard.managed":
					return []byte(managedMarker + "\n"), nil, nil
				case "list --project " + cfg.Project + " -f csv -c n":
					return []byte(test.inventory), nil, nil
				case "config get e2e-vm-1 user.subyard.managed --project " + cfg.Project,
					"config get e2e-vm-2 user.subyard.managed --project " + cfg.Project:
					return []byte(managedMarker + "\n"), nil, nil
				case "delete --force e2e-vm-1 --project " + cfg.Project,
					"delete --force e2e-vm-2 --project " + cfg.Project:
					deletes++
					return nil, nil, nil
				default:
					return nil, nil, fmt.Errorf("unexpected call: %s", joined)
				}
			}}
			runtime := &Runtime{Config: cfg, Runner: runner}
			err := runtime.deleteManagedPairForRebuild(context.Background())
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("foreign inventory error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if deletes != test.wantDeletes {
				t.Fatalf("delete count = %d, want %d", deletes, test.wantDeletes)
			}
		})
	}
}

func TestReaperFatalErrorIsDurable(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.BrokerSource = "test-yard"
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.leaseState(), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		Config: cfg,
		Runner: &fakeRunner{},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
	err := runtime.runGC(context.Background())
	if err == nil {
		t.Fatal("corrupt lease state did not fail the reaper")
	}
	batch, exportErr := runtime.eventRecorder().Export()
	if exportErr != nil {
		t.Fatal(exportErr)
	}
	if len(batch.Events) != 2 ||
		batch.Events[0].Kind != "reaper.start" ||
		batch.Events[1].Kind != "reaper.fatal" ||
		batch.Events[1].Error == "" {
		t.Fatalf("reaper failure timeline = %#v", batch.Events)
	}
}

func TestProjectLimitsShrinkOnlyAfterVMReconciliation(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Memory = "768MiB"
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "list --project " + cfg.Project + " -f csv -c n":
			return []byte(cfg.vm(1) + "\n" + cfg.vm(2) + "\n"), nil, nil
		case "project get " + cfg.Project + " limits.cpu":
			return []byte("4\n"), nil, nil
		case "project get " + cfg.Project + " limits.memory":
			return []byte("2GiB\n"), nil, nil
		case "profile device list default --project " + cfg.Project:
			return []byte("root\neth0\n"), nil, nil
		}
		return nil, nil, nil
	}}
	runtime := Runtime{Config: cfg, Runner: runner}
	if err := runtime.ensureProject(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if strings.Join(call, " ") ==
			"incus project set "+cfg.Project+" limits.memory 1536MiB" {
			t.Fatal("aggregate memory was lowered before VM limits")
		}
	}
	runner.calls = nil
	if err := runtime.tightenProject(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := callsText(runner.calls)
	for _, expected := range []string{
		"incus project set " + cfg.Project + " limits.cpu 4",
		"incus project set " + cfg.Project + " limits.memory 1536MiB",
		"incus project unset " + cfg.Project + " restricted.virtual-machines.lowlevel",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("missing call %q in:\n%s", expected, got)
		}
	}
	if strings.Contains(got, "lowlevel allow") {
		t.Fatal("obsolete low-level allowance returned")
	}
}

func TestExistingVMDropsLegacyRawAppArmorPolicy(t *testing.T) {
	cfg := fixtureConfig(t)
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "info " + cfg.vm(1) + " --project " + cfg.Project:
			return nil, nil, nil
		case "list " + cfg.vm(1) + " --project " + cfg.Project + " -f csv -c t":
			return []byte("VIRTUAL-MACHINE\n"), nil, nil
		case "config get " + cfg.vm(1) + " user.subyard.managed --project " + cfg.Project:
			return []byte(managedMarker + "\n"), nil, nil
		case "config get " + cfg.vm(1) + " raw.apparmor --project " + cfg.Project:
			return []byte("legacy-rule\n"), nil, nil
		}
		return nil, nil, nil
	}}
	runtime := Runtime{Config: cfg, Runner: runner}
	if err := runtime.ensureVM(context.Background(), cfg.vm(1)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(callsText(runner.calls),
		"incus config unset "+cfg.vm(1)+" raw.apparmor --project "+cfg.Project) {
		t.Fatal("legacy raw.apparmor was not removed")
	}
}

func TestGuardedCleanupUsesNormalProjectDelete(t *testing.T) {
	cfg := fixtureConfig(t)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project list --format csv -c n":
			return []byte(cfg.Project + "\n"), nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "list --project " + cfg.Project + " -f csv -c n":
			return []byte(cfg.vm(1) + "\n" + cfg.vm(2) + "\n"), nil, nil
		case "info " + cfg.vm(1) + " --project " + cfg.Project,
			"info " + cfg.vm(2) + " --project " + cfg.Project:
			return nil, nil, nil
		case "config get " + cfg.vm(1) + " user.subyard.managed --project " + cfg.Project,
			"config get " + cfg.vm(2) + " user.subyard.managed --project " + cfg.Project:
			return []byte(managedMarker + "\n"), nil, nil
		}
		return nil, nil, nil
	}}
	runtime := Runtime{Config: cfg, Runner: runner, Stdout: io.Discard}
	if err := runtime.cleanupManaged(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	got := callsText(runner.calls)
	if !strings.Contains(got, "incus project delete "+cfg.Project) {
		t.Fatal("empty project was not deleted")
	}
	if strings.Contains(got, "project delete "+cfg.Project+" --force") {
		t.Fatal("interactive forced project deletion was used")
	}
}

func TestCleanupRejectsForeignProject(t *testing.T) {
	cfg := fixtureConfig(t)
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if strings.Join(arguments, " ") == "project list --format csv -c n" {
			return []byte(cfg.Project + "\n"), nil, nil
		}
		if arguments[0] == "project" && arguments[1] == "get" {
			return []byte("foreign\n"), nil, nil
		}
		return nil, nil, nil
	}}
	runtime := Runtime{Config: cfg, Runner: runner, Stdout: io.Discard}
	if err := runtime.cleanupManaged(context.Background(), true); err == nil {
		t.Fatal("foreign project was accepted")
	}
}

func TestLeaseDataAccountPolicyIsAtomic(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.AgentAuthorizedKeys = filepath.Join(cfg.AgentHome, ".ssh", "authorized_keys")
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Config: cfg, Runner: &fakeRunner{}}
	if err := runtime.writeAgentAuthorizedKeys("10.42.0.11", "10.42.0.12"); err != nil {
		t.Fatal(err)
	}
	authorized, err := os.ReadFile(cfg.AgentAuthorizedKeys)
	if err != nil {
		t.Fatal(err)
	}
	expected := `restrict,port-forwarding,permitopen="10.42.0.11:22",` +
		`permitopen="10.42.0.12:22",command="` + cfg.StatusCommand + `"`
	if !strings.HasPrefix(string(authorized), expected) {
		t.Fatalf("authorized_keys = %q", authorized)
	}
	first := append([]byte(nil), authorized...)
	if err := runtime.writeAgentAuthorizedKeys("10.42.0.11", "10.42.0.12"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(cfg.AgentAuthorizedKeys)
	if !bytes.Equal(first, second) {
		t.Fatal("agent reconciliation is not idempotent")
	}
	if err := runtime.restrictAgentAccess("operator-down"); err != nil {
		t.Fatal(err)
	}
	authorized, _ = os.ReadFile(cfg.AgentAuthorizedKeys)
	if strings.Contains(string(authorized), "port-forwarding") {
		t.Fatal("down policy retained forwarding")
	}
}

func TestRuntimeMutatingSlotCommandsRequireExpectedIdentity(t *testing.T) {
	for _, action := range []string{"revoke-slot-1", "recover-slot-1"} {
		t.Run(action, func(t *testing.T) {
			runtime := Runtime{
				Config: fixtureConfig(t), Runner: &fakeRunner{},
				Stdout: io.Discard, Stderr: io.Discard,
			}
			err := runtime.Run(context.Background(), []string{action, "--yes"}, nil)
			if err == nil || !strings.Contains(err.Error(), "complete lease target identity") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRuntimeRejectsStaleRevokeBeforePhysicalStop(t *testing.T) {
	cfg := fixtureConfig(t)
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	original, err := store.AcquireSlot("original", "SHA256:key", "", "stale", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	expected := LeaseIdentity{
		SlotID: original.SlotID, ResourceGeneration: original.ResourceGeneration,
		LeaseEpoch: original.LeaseEpoch,
	}
	if err := store.BeginDrainSlot(original.SlotID, "fixture replacement"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(original.SlotID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireSlot("replacement", "SHA256:key", "", "stale", "slot-001"); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runtime := Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard}
	if err := runtime.RevokeExpectedSlot(context.Background(), store, expected); !errors.Is(err, ErrLeaseTargetStale) {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("stale revoke reached physical runner: %#v", runner.calls)
	}
}

func TestRuntimeExpectedRevokeDoesNotReapUnrelatedDrainingSlot(t *testing.T) {
	cfg := fixtureConfig(t)
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	target, err := store.AcquireSlot("target", "SHA256:key", "", "targeted-revoke", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.AcquireSlot("unrelated", "SHA256:key", "", "targeted-revoke", "slot-002")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDrainSlot(unrelated.SlotID, "unrelated background drain"); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{
		Config: cfg, Runner: &fakeRunner{}, Stdout: io.Discard, Stderr: io.Discard,
	}
	err = runtime.RevokeExpectedSlot(context.Background(), store, LeaseIdentity{
		SlotID: target.SlotID, ResourceGeneration: target.ResourceGeneration,
		LeaseEpoch: target.LeaseEpoch,
	})
	if err == nil || !strings.Contains(err.Error(), "agent bastion account is missing") {
		t.Fatalf("target fixture error = %v", err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotQuarantined {
		t.Fatalf("target state = %s", pool.Slots[0].State)
	}
	if pool.Slots[1].State != SlotDraining || pool.Slots[1].LeaseEpoch != unrelated.LeaseEpoch {
		t.Fatalf("unrelated slot was reaped by targeted revoke: %#v", pool.Slots[1])
	}
}

func TestRuntimeSerializesReleaseWithBackgroundDrain(t *testing.T) {
	cfg := fixtureConfig(t)
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	grant, err := store.AcquireSlot("client", "SHA256:key", "", "release", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkHeld(grant); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	allowFinish := make(chan struct{})
	stops := 0
	runtime := Runtime{
		Config: cfg, Runner: &fakeRunner{}, Stdout: io.Discard, Stderr: io.Discard,
	}
	runtime.finishDrain = func(_ context.Context, store LeaseStore, slot LeaseSlot) error {
		stops++
		if stops == 1 {
			close(entered)
			<-allowFinish
		}
		return store.FinishDrain(slot.SlotID, nil)
	}
	releaseDone := make(chan error, 1)
	go func() {
		releaseDone <- runtime.ReleaseSlot(context.Background(), store, grant)
	}()
	<-entered
	reapDone := make(chan error, 1)
	go func() {
		reapDone <- runtime.ReapExpired(context.Background(), store)
	}()
	close(allowFinish)
	if err := <-releaseDone; err != nil {
		t.Fatalf("release = %v", err)
	}
	if err := <-reapDone; err != nil {
		t.Fatalf("reap = %v", err)
	}
	if stops != 1 {
		t.Fatalf("physical stop count = %d, want 1", stops)
	}
	replacement, err := store.AcquireSlot(
		"replacement", "SHA256:replacement", "", "replacement", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.LeaseEpoch == grant.LeaseEpoch {
		t.Fatal("replacement reused the released lease epoch")
	}
}

func TestRuntimeSerializesQuarantineFencingBeforeRecovery(t *testing.T) {
	cfg := fixtureConfig(t)
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	grant, err := store.AcquireSlot("client", "SHA256:key", "", "quarantine", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkHeld(grant); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("fixture fencing failure")
	if err := store.Quarantine(grant, cause); err != nil {
		t.Fatal(err)
	}
	fencingStarted := make(chan struct{})
	allowFencing := make(chan struct{})
	recoveryStarted := make(chan struct{})
	quarantineCalls := 0
	runtime := Runtime{
		Config: cfg, Runner: &fakeRunner{}, Stdout: io.Discard, Stderr: io.Discard,
	}
	runtime.finishQuarantine = func(
		_ context.Context, store LeaseStore, slot LeaseSlot, _ error,
	) error {
		quarantineCalls++
		close(fencingStarted)
		<-allowFencing
		return store.SetQuarantineIncident(
			slot.SlotID,
			"00000000000000000001-0123456789abcdef",
			"00000000000000000002-fedcba9876543210",
		)
	}
	runtime.finishRecovery = func(
		_ context.Context, store LeaseStore, slot LeaseSlot,
	) error {
		close(recoveryStarted)
		_, err := store.FinishRecovery(slot.SlotID, nil, "", "")
		return err
	}
	fencingDone := make(chan error, 1)
	go func() {
		fencingDone <- runtime.HandleQuarantine(
			context.Background(), store, grant.SlotID, cause,
		)
	}()
	<-fencingStarted
	recoveryDone := make(chan error, 1)
	go func() {
		recoveryDone <- runtime.RecoverScheduled(
			context.Background(), store, grant.SlotID, true,
		)
	}()
	select {
	case <-recoveryStarted:
		t.Fatal("recovery started before quarantine fencing completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(allowFencing)
	if err := <-fencingDone; err != nil {
		t.Fatalf("quarantine fencing = %v", err)
	}
	if err := <-recoveryDone; err != nil {
		t.Fatalf("recovery = %v", err)
	}
	if quarantineCalls != 1 {
		t.Fatalf("quarantine fencing calls = %d, want 1", quarantineCalls)
	}
	replacement, err := store.AcquireSlot(
		"replacement", "SHA256:replacement", "", "replacement", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ResourceGeneration <= grant.ResourceGeneration {
		t.Fatal("recovery did not publish a new resource generation")
	}
}

func TestRuntimeRejectsStaleRecoveryBeforePhysicalRebuild(t *testing.T) {
	cfg := fixtureConfig(t)
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	original, err := store.AcquireSlot("original", "SHA256:key", "", "stale", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	expected := LeaseIdentity{
		SlotID: original.SlotID, ResourceGeneration: original.ResourceGeneration,
		LeaseEpoch: original.LeaseEpoch,
	}
	if err := store.Quarantine(original, errors.New("fixture quarantine")); err != nil {
		t.Fatal(err)
	}
	if _, started, err := store.BeginScheduledRecovery(original.SlotID, true); err != nil || !started {
		t.Fatalf("begin fixture recovery = %v, %v", started, err)
	}
	if _, err := store.FinishRecovery(original.SlotID, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runtime := Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard}
	if err := runtime.RecoverExpectedSlot(context.Background(), store, expected); !errors.Is(err, ErrLeaseTargetStale) {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("stale recovery reached physical runner: %#v", runner.calls)
	}
}

func TestRuntimeRejectsInvalidExpectedRecoveryIdentityBeforeCreatingLockPath(t *testing.T) {
	cfg := fixtureConfig(t)
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	runtime := Runtime{Config: cfg, Runner: &fakeRunner{}, Stdout: io.Discard, Stderr: io.Discard}
	err := runtime.RecoverExpectedSlot(context.Background(), store, LeaseIdentity{
		SlotID: "../escape", ResourceGeneration: 1, LeaseEpoch: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "complete lease target identity") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(cfg.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid identity created recovery state: %v", err)
	}
}

func TestLeaseReaperDoesNotDeleteRetainedAllocation(t *testing.T) {
	cfg := fixtureConfig(t)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(arguments, " ") {
		case "project show " + cfg.Project:
			return nil, nil, nil
		case "project get " + cfg.Project + " user.subyard.managed":
			return []byte(managedMarker + "\n"), nil, nil
		case "list --project " + cfg.Project + " -f csv -c n":
			return nil, nil, nil
		}
		if arguments[0] == "info" {
			return nil, nil, errors.New("missing")
		}
		return nil, nil, nil
	}}
	runtime := Runtime{
		Config: cfg, Runner: runner, Stdout: io.Discard, Now: func() time.Time { return now },
	}
	if err := runtime.gc(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(callsText(runner.calls), "incus project delete "+cfg.Project) {
		t.Fatal("lease reaper deleted retained project")
	}
	if _, err := os.Stat(cfg.leaseState()); err != nil {
		t.Fatalf("lease reaper did not initialize broker state: %v", err)
	}
}

func callsText(calls [][]string) string {
	var lines []string
	for _, call := range calls {
		lines = append(lines, strings.Join(call, " "))
	}
	return strings.Join(lines, "\n")
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
