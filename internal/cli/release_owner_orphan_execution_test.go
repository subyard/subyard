package cli

import (
	"bytes"
	"context"
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

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type orphanOriginalHost struct {
	*startupRecordHost
	fault string
	calls int
}

func (h *orphanOriginalHost) RemoveOwnedResourceIngress(context.Context, yardnetwork.Yard, resource.ProxyContract, map[string]string, string, bool) error {
	return errors.New("native orphan used unguarded closure")
}
func (h *orphanOriginalHost) RemoveOwnedResourceIngressIf(ctx context.Context, y yardnetwork.Yard, c resource.ProxyContract, uuid string, nic, before map[string]string, marker string, pending bool) error {
	h.calls++
	key := y.Project + "/" + y.Instance
	if h.fault == "uuid" || h.fault == "late-uuid" && h.calls == 2 {
		i := h.Instances[key]
		i.Config["volatile.uuid"] = "replacement-uuid"
		h.Instances[key] = i
	}
	wanted := ""
	if pending {
		wanted = marker
		if len(before) != 0 {
			wanted = "v1:pending:" + strings.TrimPrefix(c.OwnershipValue(before), "v1:")
		}
	}
	if err := h.SetOwnedResourceIngressIf(ctx, y, c, uuid, nic, before, marker, nil, wanted); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.loaded.Context.Paths.DataHome, "route-state"), []byte(wanted), 0600)
}

type orphanProcessObserver struct {
	startupProcessObserver
	unknown bool
}

func (o *orphanProcessObserver) Uncertain() { o.unknown = true }

// This uses the production init callback's retained-resource restoration seam.
// Incus is synthetic; the fresh helper and original resource children are real.
func TestOwnerInitOrphanFreshProcessExecutesOriginalResource(t *testing.T) {
	if fixture := os.Getenv("SUBYARD_TEST_ORIGINAL_ORPHAN"); fixture != "" {
		var input ownerStartupProcessFixture
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		cli, loaded, base := originalStartupProcessCLI(t, input.Root, input.Environment)
		loaded.Environment["ENVIRONMENT_PROFILES"] = ""
		original := reconcileruntime.Runtime{RepositoryRoot: input.Root, LogicalRepositoryRoot: input.Root, Environment: input.Environment, Yard: loaded.Context}
		execution, err := restoreOwnerInitRecord(input.Original, loaded, original)
		if err != nil {
			t.Fatal("init restore", err)
		}
		retained := original
		retained.RepositoryRoot = input.Retained
		physical, err := restoreOwnerInitResources(cli, execution, original, retained)
		if err != nil {
			t.Fatal("retained resource restore", err)
		}
		host := &orphanOriginalHost{startupRecordHost: base, fault: input.Fault}
		physical.options.Incus = host
		again, err := captureOwnerInitRecord(execution)
		if err != nil || !bytes.Equal(again, input.Original) {
			t.Fatal("original init bytes changed", err)
		}
		if execution.orphanIngress.uuid != "original-startup-uuid" || loaded.Environment["EXTERNAL"] != "/original-source-\xff" {
			t.Fatal("original identity/config lost")
		}
		owner := os.NewFile(3, "original owner")
		defer owner.Close()
		release, err := uninstall.RegisterBorrowedOwnerLock(loaded.Context.Paths.DataHome, owner)
		if input.Fault == "missing-ofd" || input.Fault == "foreign-ofd" {
			if err == nil {
				release()
				t.Fatal("foreign or missing owner OFD accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		observer := &orphanProcessObserver{startupProcessObserver: startupProcessObserver{root: loaded.Context.Paths.DataHome}}
		ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), loaded.Context.Paths.DataHome)
		err = execution.orphanIngress.apply(ctx, physical, loaded, "original-init")
		switch input.Fault {
		case "uuid", "expanded", "binding":
			if !errors.Is(err, domain.ErrPlanStale) || observer.commands != 0 {
				t.Fatalf("drift dispatched: err=%v commands=%d", err, observer.commands)
			}
		case "guest-fail":
			if err == nil || observer.commands != 1 || !observer.unknown {
				t.Fatalf("failed guest invented certainty: err=%v commands=%d unknown=%v", err, observer.commands, observer.unknown)
			}
			if host.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName].LocalConfig[base.definition.Proxy.OwnershipKey()] == "" {
				t.Fatal("failed guest lost pending retry intent")
			}
		case "late-uuid":
			if !errors.Is(err, domain.ErrPlanStale) || observer.commands != 1 {
				t.Fatalf("late replacement accepted: %v", err)
			}
			if host.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName].LocalConfig[base.definition.Proxy.OwnershipKey()] == "" {
				t.Fatal("replacement marker cleared")
			}
		default:
			if err != nil || observer.commands != 1 {
				t.Fatalf("original orphan execution: err=%v commands=%d", err, observer.commands)
			}
			instance := host.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]
			if len(instance.LocalDevices[base.definition.Proxy.Device]) != 0 || instance.LocalConfig[base.definition.Proxy.OwnershipKey()] != "" {
				t.Fatal("original orphan did not converge")
			}
		}
		return
	}
	cli, loaded, _ := originalOrphanProcessCLI(t)
	root := cli.options.RepositoryRoot
	relative := "config/profiles/sample/resources/relay/handler.sh"
	plan, err := cli.prepareOrphanIngress(context.Background(), loaded)
	if err != nil || plan == nil || plan.entries[0].prepared == nil {
		t.Fatal("prepare original orphan", err)
	}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, LogicalRepositoryRoot: root, Environment: cli.options.Environment, Yard: loaded.Context}
	execution := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime, orphanIngress: plan}
	original, err := captureOwnerInitRecord(execution, cli)
	if err != nil {
		t.Fatal("capture init orphan", err)
	}
	// The same callback handles V1 without requiring a native-plan grammar.
	testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.DataHome, "v1"), nil, 0600)
	v1Plan, err := cli.prepareOrphanIngress(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	execution.orphanIngress = v1Plan
	v1, err := captureOwnerInitRecord(execution, cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(loaded.Context.Paths.DataHome, "v1")); err != nil {
		t.Fatal(err)
	}
	retained := testkit.TempDir(t)
	for _, path := range []string{"config/profiles/sample/resources/relay.res", relative} {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(retained, path)), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(retained, path), body, 0700)
	}
	testkit.WriteFile(t, filepath.Join(root, relative), []byte("#!/bin/sh\nprintf foreign > \"$SUBYARD_HOME/foreign-B\"\nexit 77\n"), 0700)
	_, release, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner, err := uninstall.BorrowOwnerLock(loaded.Context.Paths.DataHome)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	fixture := filepath.Join(testkit.TempDir(t), "orphan.json")
	for _, fault := range []string{"", "v1", "uuid", "expanded", "binding", "late-uuid", "guest-fail", "missing-ofd", "foreign-ofd", "loss"} {
		t.Run("fresh-"+fault, func(t *testing.T) {
			for _, name := range []string{"orphan-stopped", "route-state", "v1", "expanded", "binding", "guest-fail", "loss", "release", "handler-pid", "dispatch-intent", "dispatch-terminal"} {
				if err := os.Remove(filepath.Join(loaded.Context.Paths.DataHome, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			if fault == "v1" || fault == "expanded" || fault == "binding" || fault == "guest-fail" || fault == "loss" {
				testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.DataHome, fault), nil, 0600)
			}
			input := ownerStartupProcessFixture{Root: root, Retained: retained, Environment: cli.options.Environment, Original: original, Fault: fault}
			if fault == "v1" {
				input.Original = v1
			}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, fixture, body, 0600)
			command := exec.Command(os.Args[0], "-test.run=^TestOwnerInitOrphanFreshProcessExecutesOriginalResource$")
			command.Env = append(os.Environ(), "SUBYARD_TEST_ORIGINAL_ORPHAN="+fixture)
			command.ExtraFiles = []*os.File{owner}
			if fault == "missing-ofd" {
				command.ExtraFiles = nil
			}
			if fault == "foreign-ofd" {
				foreignPath := filepath.Join(testkit.TempDir(t), "foreign")
				testkit.WriteFile(t, foreignPath, nil, 0600)
				foreign, err := os.OpenFile(foreignPath, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer foreign.Close()
				command.ExtraFiles = []*os.File{foreign}
			}
			if fault == "loss" {
				assertOrphanChildLoss(t, command, loaded.Context.Paths.DataHome, owner, release)
				return
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("fresh original orphan: %v\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(loaded.Context.Paths.DataHome, "foreign-B")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("live handler B effected")
			}
			if fault == "" || fault == "v1" {
				body, err := os.ReadFile(filepath.Join(loaded.Context.Paths.DataHome, "orphan-stopped"))
				if err != nil || string(body) != "original A once" {
					t.Fatal("original handler effect absent", err)
				}
			}
		})
	}
}

func assertOrphanChildLoss(t *testing.T, command *exec.Cmd, home string, owner *os.File, release func()) {
	t.Helper()
	sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	command.Stdout, command.Stderr = sink, sink
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(filepath.Join(home, "handler-pid"))
		pid, _ = strconv.Atoi(string(body))
		if pid > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("orphan child not dispatched")
	}
	defer syscall.Kill(-pid, syscall.SIGKILL)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("lost helper reported success")
	}
	owner.Close()
	release()
	contender, err := os.OpenFile(uninstall.OwnerLockPath(home), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("surviving orphan lost original OFD")
	}
	if _, err := os.Stat(filepath.Join(home, "dispatch-intent")); err != nil {
		t.Fatal("missing dispatch intent", err)
	}
	if _, err := os.Stat(filepath.Join(home, "dispatch-terminal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("loss invented terminal")
	}
	body, err := os.ReadFile(filepath.Join(home, "orphan-stopped"))
	if err != nil || string(body) != "original A once" {
		t.Fatal("loss changed original effect count", err)
	}
	testkit.WriteFile(t, filepath.Join(home, "release"), nil, 0600)
	for time.Now().Before(deadline) {
		if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("orphan child retained OFD after drain")
}

func TestOriginalOrphanUnavailableShutdownRetainsPendingWithoutGuest(t *testing.T) {
	for _, mode := range []string{"nil", "unavailable", "pending"} {
		t.Run(mode, func(t *testing.T) {
			program, _, _ := orphanIngressFixture(t, false)
			cli, loaded, base := originalStartupProcessCLI(t, program.options.RepositoryRoot, program.options.Environment)
			loaded.Environment["ENVIRONMENT_PROFILES"] = ""
			if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); err != nil {
				t.Fatal(err)
			}
			key := loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName
			instance := base.Instances[key]
			if mode == "nil" {
				instance.Status = "Stopped"
				base.Instances[key] = instance
			}
			if mode == "pending" {
				base.originalPending()
				loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"] = "other"
			}
			if mode == "unavailable" {
				testkit.WriteFile(t, base.definition.HandlerPath(), []byte("#!/bin/sh\nexit 1\n"), 0700)
			}
			host := &orphanOriginalHost{startupRecordHost: base}
			cli.options.Incus = host
			plan, err := cli.prepareOrphanIngress(context.Background(), loaded)
			if err != nil || plan == nil {
				t.Fatal("original unavailable plan missing", err)
			}
			raw, err := captureOwnerOrphanRecord(plan, cli, cli.options.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			plan, err = restoreOwnerOrphanRecord(raw, cli, cli.options.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			plan.native = true
			observer := &startupProcessObserver{root: loaded.Context.Paths.DataHome}
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			if err := plan.apply(ctx, cli, loaded, "original-init"); err == nil || !strings.Contains(err.Error(), "shutdown") {
				t.Fatal("unverified guest accepted", err)
			}
			observed := host.Instances[key]
			if len(observed.LocalDevices[base.definition.Proxy.Device]) != 0 || !pendingResourceIngress.MatchString(observed.LocalConfig[base.definition.Proxy.OwnershipKey()]) || observer.commands != 0 {
				t.Fatal("pending ownership/guest semantics changed")
			}
		})
	}
}

func originalOrphanProcessCLI(t *testing.T) (*CLI, config.Loaded, *startupRecordHost) {
	t.Helper()
	program, _, _ := orphanIngressFixture(t, false)
	root := program.options.RepositoryRoot
	handler := `#!/usr/bin/env python3
import json,os,pathlib,sys,time
root=pathlib.Path(os.environ['SUBYARD_HOME'])
state=root/'orphan-stopped'
mode=os.environ.get('SUBYARD_RESOURCE_MODE')
if mode=='apply':
    assert os.environ['SUBYARD_RESOURCE_INGRESS_OWNER']=='parent'
    assert os.environ['SUBYARD_RESOURCE_INSTANCE_UUID']=='original-startup-uuid'
    assert os.environ['SUBYARD_NATIVE_OWNER_FD3']=='1'
    assert pathlib.Path.cwd()==pathlib.Path(os.environ['SUBYARD_REPOSITORY_ROOT'])
    assert os.environ['YARD_NAME']=='private'
    assert os.environ['EXCLUSIVE_ENVIRONMENT_PROFILE']=='sample'
    assert os.environ['ENVIRONMENT_PROFILES']==''
    assert b'EXTERNAL' not in os.environb # outside the existing handler setting allowlist
    assert pathlib.Path('/proc/self/fd/'+os.environ['SUBYARD_OWNER_LOCK_FD']).exists()
    assert (root/'route-state').read_text().startswith('v1:pending:')
    assert not state.exists()
    if not (root/'v1').exists():
        assert os.environ['SUBYARD_RESOURCE_BINDING']=='a'*64
        assert len(json.loads(os.environ['SUBYARD_RESOURCE_STEPS']))==1
    state.write_text('original A once')
    if (root/'guest-fail').exists(): sys.exit(3)
    if (root/'loss').exists():
        (root/'handler-pid').write_text(str(os.getpid()))
        while not (root/'release').exists(): time.sleep(.01)
    sys.exit(0)
if mode=='verify':
    assert os.environ['SUBYARD_RESOURCE_INGRESS_OWNER']=='parent'
    assert os.environ['SUBYARD_RESOURCE_INSTANCE_UUID']=='original-startup-uuid'
ready=state.exists()
result=dict(schema='yard.resource-action-assessment.v1',action='down',changed=not ready,consequences=[] if ready else ['stop original service'])
if not (root/'v1').exists():
    result['schema']='yard.resource-action-assessment.v2'
    result['binding']=('b' if (root/'binding').exists() else 'a')*64
    step=dict(id='service',target='original service',observed='stopped' if ready else 'running',desired='stopped',decision='skip' if ready else 'apply',preconditions=[],dependsOn=[],verify='read original service',consequence='stop original service')
    result['steps']=[step]
    if (root/'expanded').exists(): result['steps'].append(dict(step,id='foreign',target='expanded service'))
print(json.dumps(result))
`
	relative := "config/profiles/sample/resources/relay/handler.sh"
	testkit.WriteFile(t, filepath.Join(root, relative), []byte(handler), 0700)
	cli, loaded, host := originalStartupProcessCLI(t, root, program.options.Environment)
	loaded.Environment["ENVIRONMENT_PROFILES"] = ""
	if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	return cli, loaded, host
}
