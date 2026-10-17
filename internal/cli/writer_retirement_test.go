package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/crypto/ssh"
)

func retireWriterInstallation(t *testing.T, loaded config.Loaded, configRoot bool) {
	t.Helper()
	_, release, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err = uninstall.AdvanceOwnerEpoch(loaded.Context.Paths.DataHome, 0, 1); err != nil {
		t.Fatal(err)
	}
	paths := []string{loaded.Context.Paths.DataHome}
	if configRoot {
		paths = append(paths, loaded.Context.Paths.ConfigHome)
	}
	for _, path := range paths {
		if err = os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInitProjectInventoryAssessmentDoesNotCreateState(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = program.prepareProjectInventory(context.Background(), loaded, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(loaded.Context.Paths.ConfigHome); !os.IsNotExist(err) {
		t.Fatal("inventory assessment published the configuration root", err)
	}
}

func TestUnpreparedWritersRetainOriginalInstallation(t *testing.T) {
	for _, actor := range []string{"state", "query", "status"} {
		for _, replacement := range []bool{false, true} {
			t.Run(actor+map[bool]string{false: "/removed", true: "/replacement"}[replacement], func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				var stderr bytes.Buffer
				program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: lifecycleIncus(), Stderr: &stderr})
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := program.loadContext("default")
				if err != nil {
					t.Fatal(err)
				}
				nativeInstallationFixture(t, environment)
				if _, err = program.originalOwnerWrite(loaded); err != nil {
					t.Fatal(err)
				}
				// The actor has loaded the original context; a completed full removal wins
				// before its next native write. A new setup must not admit this old actor.
				retireWriterInstallation(t, loaded, true)
				if replacement {
					nativeInstallationFixture(t, environment)
				}
				switch actor {
				case "state":
					if code := program.runProjectState(context.Background(), loaded, []string{"upsert", "sample", "sample", "sync", "yard"}, true); code == 0 {
						t.Error("retired state mutation succeeded")
					}
				case "query":
					program.allOwnerInventoriesForQueries(context.Background(), loaded, false)
				case "status":
					program.allOwnerInventoriesForStatus(context.Background(), loaded)
				}
				for _, path := range []string{loaded.Context.Paths.StateDir, filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Errorf("retired %s recreated namespace %s: %v", actor, path, err)
					}
				}
			})
		}
	}
}

func TestAuditFirstQueriesRetainOriginalInstallation(t *testing.T) {
	for _, status := range []bool{false, true} {
		t.Run(map[bool]string{false: "query", true: "status"}[status], func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: lifecycleIncus()})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			nativeInstallationFixture(t, environment)
			gate := program.auditWriteAdmission(loaded.Context.Paths.DataHome)
			if err = audit.WriteInvocation(audit.Invocation{Home: loaded.Context.Paths.DataHome, Command: "status", BeforeWrite: gate}); err != nil {
				t.Fatal(err)
			}
			retireWriterInstallation(t, loaded, true)
			nativeInstallationFixture(t, environment)
			if status {
				program.allOwnerInventoriesForStatus(context.Background(), loaded)
			} else {
				program.allOwnerInventoriesForQueries(context.Background(), loaded, false)
			}
			if _, err = os.Lstat(filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")); !os.IsNotExist(err) {
				t.Fatal("audit-first old query recreated inventory", err)
			}
		})
	}
}

func TestRemoteSelectedHostMutationCannotRecreateRetiredController(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0600)
	hostAddSSHFixture(t, inventoryResult("remote-owner", "default", "").inventory)
	environment = append(environment, "ACCESS_KIND=remote", "OWNER_ENDPOINT=owner.example", "OWNER_YARD_NAME=default")
	program, err := New(Options{RepositoryRoot: root, Arguments: []string{"host", "add", "owner-alias"}, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil || loaded.Context.AccessKind != domain.AccessRemote {
		t.Fatal("fixture did not select remote yard", err)
	}
	program.options.Prompt = retireWriterPrompt(func() { retireWriterInstallation(t, loaded, false) })
	if code := program.Run(context.Background()); code == 0 {
		t.Fatal("retired controller mutation succeeded")
	}
	if _, err = os.Lstat(loaded.Context.Paths.DataHome); !os.IsNotExist(err) {
		t.Fatal("remote-selected mutation recreated controller", err)
	}
}

type retireWriterPrompt func()

func (p retireWriterPrompt) Confirm(context.Context, domain.ConfirmationRequest) (bool, error) {
	p()
	return true, nil
}

func writerUnlockFixture(t *testing.T) (*CLI, config.Loaded, string, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	var stderr bytes.Buffer
	executor := &sshAgentDispatchExecutor{}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: lifecycleIncus(), Executor: executor, Stderr: &stderr, DispatcherPath: filepath.Join(root, "missing-worker")})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	nativeInstallationFixture(t, environment)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(loaded.Context.Paths.DataHome, "ssh")
	if err = os.MkdirAll(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(keyDir, "id_ed25519")
	testkit.WriteFile(t, identity, pem.EncodeToMemory(block), 0600)
	pub, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, identity+".pub", ssh.MarshalAuthorizedKey(pub), 0644)
	if err = os.MkdirAll(filepath.Join(loaded.Context.Paths.OperatorHome, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.OperatorHome, ".ssh/subyard.config"), []byte("Host yard\n IdentityFile \""+identity+"\"\n IdentitiesOnly yes\n"), 0600)
	block, err = ssh.MarshalPrivateKeyWithPassphrase(private, "synthetic", []byte("synthetic-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "external-encrypted-key")
	testkit.WriteFile(t, key, pem.EncodeToMemory(block), 0600)
	testkit.WriteFile(t, filepath.Join(root, "scripts/ssh-agent-environment.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	incus := program.options.Incus.(*testkit.Incus)
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	directory := sshagentruntime.Directory(loaded.Context.Paths.DataHome, loaded.Context.YardName)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	program.operatorTerminal = func() bool { return true }
	return program, loaded, key, directory
}

func TestSSHUnlockCannotRecreateRetiredSigningNamespace(t *testing.T) {
	program, loaded, key, directory := writerUnlockFixture(t)
	stderr := program.options.Stderr.(*bytes.Buffer)
	// Configuration/source roots remain valid (e.g. preserved Git); the native
	// grant must still be denied after the installation epoch is retired.
	program.options.Prompt = retireWriterPrompt(func() { retireWriterInstallation(t, loaded, false) })
	definition, _ := program.manifest.Lookup("ssh-agent")
	if code := program.runSSHAgent(context.Background(), loaded, definition, []string{"unlock", "--key", key, "--ttl", time.Hour.String()}); code == 0 {
		t.Fatal("retired grant succeeded")
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("unlock recreated removed signing state: %v; diagnostic=%s", err, stderr.String())
	}
	if len(program.options.Executor.(*sshAgentDispatchExecutor).requests) != 0 {
		t.Fatal("retired unlock changed the guest environment")
	}
	if !strings.Contains(stderr.String(), "epoch changed") {
		t.Fatalf("unlock did not retain its original epoch: %s", stderr.String())
	}
}

// Done is queried by the real owner lock only when its original inode is busy.
type writerBarrierWait struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (ctx *writerBarrierWait) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestStartupRecoveryQueuesBeforeAnyControllerWrite(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	nativeInstallationFixture(t, environment)
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	inventory := filepath.Join(dataHome, "owner-inventory")
	if err := os.Mkdir(inventory, 0700); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"_state", "upsert-yard", "sample", "sample", "sync", "yard", "yard"}, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := uninstall.OwnerLock(context.Background(), dataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx := &writerBarrierWait{Context: base, waiting: make(chan struct{})}
	done := make(chan int, 1)
	go func() { done <- program.Run(ctx) }()
	select {
	case <-ctx.waiting:
	case code := <-done:
		t.Fatalf("startup recovery skipped owner barrier: %d %s", code, stderr.String())
	case <-base.Done():
		t.Fatal("writer did not reach existing owner barrier")
	}
	if _, err := os.Lstat(filepath.Join(inventory, ".lock")); !os.IsNotExist(err) {
		t.Fatal("startup recovery wrote before owner admission")
	}
	if err = uninstall.AdvanceOwnerEpoch(dataHome, 0, 1); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dataHome, loaded.Context.Paths.ConfigHome} {
		if err = os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	release()
	release = nil
	if code := <-done; code == 0 || !strings.Contains(stderr.String(), "epoch changed") {
		t.Fatalf("retired startup writer = %d %s", code, stderr.String())
	}
	for _, path := range []string{dataHome, loaded.Context.Paths.ConfigHome} {
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("startup recovery recreated a retired root")
		}
	}
}

func TestDirectStateRetainsItsOwnFirstAuditPublication(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	for i, value := range environment {
		if strings.HasPrefix(value, "SUBYARD_NO_AUDIT=") {
			environment[i] = "SUBYARD_NO_AUDIT="
		}
	}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"_state", "upsert-yard", "sample", "sample", "sync", "yard", "yard"}, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("initial audit invalidated its own state operation: %d %s", code, stderr.String())
	}
}

func TestProjectStateUsesInheritedOwnerBarrier(t *testing.T) {
	root, environment, stateDir := nativeFixture(t)
	nativeInstallationFixture(t, environment)
	home := environmentValue(environment, "SUBYARD_HOME")
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	borrowed, err := uninstall.BorrowOwnerLock(home)
	if err != nil || borrowed == nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	foreignHome := filepath.Join(root, "foreign-data")
	_, foreignRelease, err := uninstall.OwnerLock(context.Background(), foreignHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer foreignRelease()
	foreign, err := uninstall.BorrowOwnerLock(foreignHome)
	if err != nil || foreign == nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	for _, test := range []struct {
		name string
		fd   *os.File
	}{{"nested", borrowed}, {"foreign", foreign}} {
		t.Setenv("SUBYARD_OWNER_LOCK_FD", strconv.Itoa(int(test.fd.Fd())))
		var stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"_state", "upsert-yard", test.name, test.name, "sync", "yard", "yard"}, Stderr: &stderr})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		code := program.Run(ctx)
		cancel()
		if test.name == "nested" && code != 0 {
			t.Fatalf("inherited state barrier refused/deadlocked: %d %s", code, stderr.String())
		}
		if test.name == "foreign" && (code == 0 || !strings.Contains(stderr.String(), "another installation")) {
			t.Fatalf("foreign descriptor admitted: %d %s", code, stderr.String())
		}
	}
	if _, err = os.Lstat(filepath.Join(stateDir, "nested.json")); err != nil {
		t.Fatal("nested state did not publish", err)
	}
	if _, err = os.Lstat(filepath.Join(stateDir, "foreign.json")); !os.IsNotExist(err) {
		t.Fatal("foreign descriptor created state")
	}
	independent, err := os.OpenFile(uninstall.OwnerLockPath(home), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	if err = syscall.Flock(int(independent.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal("nested state released parent's real barrier", err)
	}
}

// Run the real native pending daemon from this test binary, without building
// another engine or changing the production worker/terminal implementation.
func TestWriterUnlockDaemon(t *testing.T) {
	if len(os.Args) > 2 && os.Args[len(os.Args)-2] == "--" {
		if err := sshagentruntime.RunDaemon(context.Background(), os.Args[len(os.Args)-1]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdmittedSSHInputWaitSerializesBeforeFullRemoval(t *testing.T) {
	program, loaded, key, directory := writerUnlockFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(program.options.RepositoryRoot, "native-worker")
	testkit.WriteFile(t, worker, []byte(fmt.Sprintf("#!/bin/sh\nexec '%s' -test.run=^TestWriterUnlockDaemon$ -- \"$2\"\n", strings.ReplaceAll(binary, "'", "'\\''"))), 0700)
	program.options.DispatcherPath = worker
	tools := filepath.Join(program.options.RepositoryRoot, "tools")
	if err = os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	waiting := filepath.Join(program.options.RepositoryRoot, "input-wait")
	testkit.WriteFile(t, filepath.Join(tools, "ssh-add"), []byte("#!/bin/sh\n: > \"$TEST_SSH_INPUT_WAIT\"\nexec /bin/sleep 30\n"), 0700)
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	program.env["TEST_SSH_INPUT_WAIT"] = waiting
	program.options.Prompt = &testkit.Prompt{Answers: []bool{true}}
	definition, _ := program.manifest.Lookup("ssh-agent")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- program.runSSHAgent(ctx, loaded, definition, []string{"unlock", "--key", key, "--ttl", "1h"})
	}()
	until := time.Now().Add(3 * time.Second)
	for {
		if _, err = os.Lstat(waiting); err == nil {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("unlock did not reach native ssh-add input boundary: %d %s", code, program.options.Stderr)
		default:
		}
		if time.Now().After(until) {
			cancel()
			<-done
			t.Fatal("native input boundary was not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	queued, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, release, err := uninstall.OwnerLock(queued, loaded.Context.Paths.DataHome, nil)
	stop()
	if err == nil {
		release()
		cancel()
		<-done
		t.Fatal("full removal passed the already-admitted input transaction")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		<-done
		t.Fatal(err)
	}
	cancel()
	if code := <-done; code == 0 {
		t.Fatal("cancelled unlock succeeded")
	}
	manager := sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: directory}}
	status, err := manager.Status(context.Background())
	if err != nil || status.State != "locked" {
		t.Fatal("cancelled input retained a grant", err)
	}
	for _, name := range []string{"private.sock", "control.sock"} {
		if _, err = os.Lstat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Fatal("cancelled native transaction retained socket", name)
		}
	}
	retireWriterInstallation(t, loaded, true)
	if err = os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatal("signing namespace survived completed removal")
	}
}

func TestRemoteAddGapRetainsOriginalOwnerAuthority(t *testing.T) {
	for _, scenario := range []string{"retirement", "replacement", "generation", "cancellation", "managed-drift"} {
		t.Run(scenario, func(t *testing.T) {
			options, runtime := auditedRemoteOwnerFixture(t)
			gap := testkit.TempDir(t)
			runtime.Environment = append(runtime.Environment, "REMOTE_GAP="+gap)
			options.RemoteControl = runtime
			var stderr bytes.Buffer
			options.Stderr = &stderr
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan int, 1)
			go func() { done <- program.Run(ctx) }()
			// Always release the finite fixture command even if an assertion fails.
			defer testkit.WriteFile(t, filepath.Join(gap, "release"), nil, 0600)
			deadline := time.Now().Add(time.Second)
			for {
				if _, err := os.Stat(filepath.Join(gap, "entered")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("authorization did not enter unlocked gap")
				}
				time.Sleep(5 * time.Millisecond)
			}
			switch scenario {
			case "retirement", "replacement":
				retireWriterInstallation(t, loaded, true)
				if scenario == "replacement" {
					nativeInstallationFixture(t, options.Environment)
				}
			case "generation":
				home := loaded.Context.Paths.DataHome
				if err := os.Rename(home, home+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(home, 0700); err != nil {
					t.Fatal(err)
				}
			case "cancellation":
				cancel()
			case "managed-drift":
				testkit.WriteFile(t, filepath.Join(runtime.Home, ".ssh", "config"), []byte("Host changed\n"), 0600)
			}
			testkit.WriteFile(t, filepath.Join(gap, "release"), nil, 0600)
			select {
			case code := <-done:
				if code == 0 {
					t.Fatal("changed original authority unexpectedly committed")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("remote gap did not return")
			}
			for _, path := range []string{filepath.Join(runtime.ConfigHome, "yards", "self-fixture", "config.env"), filepath.Join(runtime.Home, ".ssh", "subyard-self-fixture.config"), filepath.Join(runtime.DataHome, "remote-self-fixture.cache")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("failed gap published registration", err)
				}
			}
			if scenario == "retirement" {
				if _, err := os.Lstat(runtime.DataHome); !os.IsNotExist(err) {
					t.Fatal("retired data home was recreated", err)
				}
			}
		})
	}
}
