package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type startupRecordHost struct {
	yardnetwork.Host
	*testkit.Incus
	loaded     config.Loaded
	definition resource.Definition
	fault      string
}

func TestOriginalResourceStepsRetainEmptyContainers(t *testing.T) {
	original := []domain.OperationStep{{ID: "service", Preconditions: []string{}, DependsOn: nil}, {ID: "route", Preconditions: nil, DependsOn: []string{}}}
	record := struct{ Steps []ownerResourceStep }{teardownSlice(original, describeResourceStep)}
	raw, err := nativejson.EncodeLosslessNativeJSON(record, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var restored struct{ Steps []ownerResourceStep }
	if err := nativejson.DecodeLosslessNativeJSON(raw, &restored, 4096); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, teardownSlice(restored.Steps, ownerResourceStep.restore)) {
		t.Fatal("original empty step containers changed")
	}
}

type ownerStartupProcessFixture struct {
	Root, Retained, Fault string
	Environment           []string
	Original              []byte
}

type startupProcessObserver struct {
	ownerGraphObserver
	root string
}

func (o *startupProcessObserver) CommandStarted(kind, source string) (uint64, error) {
	n, err := o.ownerGraphObserver.CommandStarted(kind, source)
	if err != nil {
		return 0, err
	}
	return n, os.WriteFile(filepath.Join(o.root, "dispatch-intent"), []byte(source), 0600)
}
func (o *startupProcessObserver) CommandTerminal(n uint64) error {
	return os.WriteFile(filepath.Join(o.root, "dispatch-terminal"), []byte(strconv.FormatUint(n, 10)), 0600)
}

func (h *startupRecordHost) originalPending() {
	key := h.loaded.Context.IncusProject + "/" + h.loaded.Context.YardInstanceName
	i := h.Instances[key]
	marker := "v1:pending:" + strings.TrimPrefix(h.definition.Proxy.OwnershipValue(i.LocalDevices[h.definition.Proxy.Device]), "v1:")
	delete(i.Devices, h.definition.Proxy.Device)
	delete(i.LocalDevices, h.definition.Proxy.Device)
	i.Config[h.definition.Proxy.OwnershipKey()] = marker
	i.LocalConfig[h.definition.Proxy.OwnershipKey()] = marker
	h.Instances[key] = i
}

func (h *startupRecordHost) ReadPolicy(context.Context) (yardnetwork.StoredPolicy, error) {
	return yardnetwork.StoredPolicy{Policy: yardnetwork.Policy{Schema: 1}}, nil
}
func (h *startupRecordHost) InspectNetwork(ctx context.Context, yards []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	i := h.Instances[h.loaded.Context.IncusProject+"/"+h.loaded.Context.YardInstanceName]
	snapshot := yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{Yard: networkYard(h.loaded.Context), InstanceFound: true, InstanceInfo: i}}}
	return (&rollbackIngressHost{snapshot: snapshot}).InspectNetwork(ctx, yards)
}
func (h *startupRecordHost) SetInstanceConfigIf(_ context.Context, project, name, uuid string, before, values map[string]string) error {
	if h.fault == "intent" || h.fault == "cleanup-uuid" {
		return domain.ErrPlanStale
	}
	i := h.Instances[project+"/"+name]
	if i.Config["volatile.uuid"] != uuid {
		return domain.ErrPlanStale
	}
	for k, v := range before {
		if i.Config[k] != v {
			return domain.ErrPlanStale
		}
	}
	for k, v := range values {
		i.Config[k] = v
		i.LocalConfig[k] = v
	}
	h.Instances[project+"/"+name] = i
	return os.WriteFile(filepath.Join(h.loaded.Context.Paths.DataHome, "committed"), []byte(uuid), 0600)
}

func (h *startupRecordHost) SetOwnedResourceIngressIf(_ context.Context, yard yardnetwork.Yard, contract resource.ProxyContract, uuid string, nic, before map[string]string, beforeMarker string, desired map[string]string, desiredMarker string) error {
	i := h.Instances[yard.Project+"/"+yard.Instance]
	if h.fault == "uuid" {
		i.Config["volatile.uuid"] = "replacement-uuid"
	}
	if i.Config["volatile.uuid"] != uuid || !maps.Equal(i.Devices["eth0"], nic) || !maps.Equal(i.LocalDevices[contract.Device], before) || i.LocalConfig[contract.OwnershipKey()] != beforeMarker {
		return domain.ErrPlanStale
	}
	if i.Status == "Running" && (!maps.Equal(before, desired) || beforeMarker != desiredMarker) {
		i.Config["volatile.vm.needs_reset"] = "true"
		i.LocalConfig["volatile.vm.needs_reset"] = "true"
	}
	if len(desired) == 0 {
		delete(i.Devices, contract.Device)
		delete(i.LocalDevices, contract.Device)
		delete(i.Config, contract.OwnershipKey())
		delete(i.LocalConfig, contract.OwnershipKey())
		if desiredMarker != "" {
			i.Config[contract.OwnershipKey()] = desiredMarker
			i.LocalConfig[contract.OwnershipKey()] = desiredMarker
		}
		if err := os.WriteFile(filepath.Join(h.loaded.Context.Paths.DataHome, "closed"), []byte(uuid), 0600); err != nil {
			return err
		}
	} else {
		i.Devices[contract.Device] = maps.Clone(desired)
		i.LocalDevices[contract.Device] = maps.Clone(desired)
		i.Config[contract.OwnershipKey()] = desiredMarker
		i.LocalConfig[contract.OwnershipKey()] = desiredMarker
	}
	if h.fault == "cleanup-uuid" && len(desired) == 0 {
		i.Config["volatile.uuid"] = "replacement-after-proxy-closure"
	}
	h.Instances[yard.Project+"/"+yard.Instance] = i
	return nil
}

func originalStartupProcessCLI(t *testing.T, root string, environment []string) (*CLI, config.Loaded, *startupRecordHost) {
	t.Helper()
	cli, err := New(Options{RepositoryRoot: root, WorkingDir: root, Environment: environment, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.loadContext("private")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"] = "sample"
	loaded.Environment["RESOURCE_RELAY_IPV4"] = "10.20.30.40"
	loaded.Environment["RESOURCE_RELAY_PORT"] = "42000"
	loaded.Environment["EXTERNAL"] = "/original-source-\xff"
	definition, _ := cli.resources.Lookup("relay")
	device := map[string]string{"type": "proxy", "bind": "host", "nat": "true", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999"}
	metadata := map[string]string{"volatile.uuid": "original-startup-uuid", startupIntentKey(definition): startupPending, definition.Proxy.OwnershipKey(): definition.Proxy.OwnershipValue(device)}
	i := ports.InstanceInfo{Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Type: domain.YardVM, Status: "Running", Config: maps.Clone(metadata), LocalConfig: maps.Clone(metadata), Devices: map[string]map[string]string{definition.Proxy.Device: maps.Clone(device), "eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}, LocalDevices: map[string]map[string]string{definition.Proxy.Device: maps.Clone(device)}}
	host := &startupRecordHost{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{i.Project + "/" + i.Name: i}}, loaded: loaded, definition: definition}
	cli.options.Incus = host
	cli.options.NetworkPolicy = &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{}, ClearStaleUDP: func(context.Context, netip.AddrPort) error { return nil }}
	return cli, loaded, host
}

// The helper is a fresh process with only original serialized bytes and the
// inherited owner OFD. Resource prepare/apply/verify are real child processes;
// only external Incus/network observations are synthetic.
func TestOwnerStartupRecordFreshProcessExecutesOriginalResource(t *testing.T) {
	if fixture := os.Getenv("SUBYARD_TEST_ORIGINAL_STARTUP"); fixture != "" {
		var input ownerStartupProcessFixture
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		cli, loaded, host := originalStartupProcessCLI(t, input.Root, input.Environment)
		if strings.HasPrefix(input.Fault, "pending-") {
			host.originalPending()
		}
		input.Fault = strings.TrimPrefix(input.Fault, "pending-")
		host.fault = input.Fault
		cli.options.RepositoryRoot = input.Retained
		cli.options.WorkingDir = input.Retained
		cli.resources, err = resource.Load(input.Retained)
		if err != nil {
			t.Fatal(err)
		}
		startup, err := restoreOwnerResourceStartup(input.Original, cli, loaded, input.Root)
		if err != nil {
			t.Fatal("original restore", err)
		}
		if startup.intent.instanceUUID != "original-startup-uuid" || startup.loaded.Environment["EXTERNAL"] != "/original-source-\xff" {
			t.Fatal("original target or source bytes changed")
		}
		again, err := captureOwnerResourceStartup(startup, cli, input.Root)
		if err != nil || !bytes.Equal(again, input.Original) {
			t.Fatal("original plan changed", err)
		}
		owner := os.NewFile(3, "original owner")
		defer owner.Close()
		release, err := uninstall.RegisterBorrowedOwnerLock(loaded.Context.Paths.DataHome, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		observer := &startupProcessObserver{root: loaded.Context.Paths.DataHome}
		ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), loaded.Context.Paths.DataHome)
		err = startup.apply(ctx, cli, "original-startup")
		if input.Fault != "" {
			if err == nil {
				t.Fatal("fault accepted")
			}
			if input.Fault == "verify" || input.Fault == "intent" {
				if observer.commands != 2 {
					t.Fatalf("rollback dispatches=%d error=%v", observer.commands, err)
				}
				if host.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName].Config[startupIntentKey(startup.definition)] != startupPending {
					t.Fatal("failed startup changed original intent")
				}
				owned, originalErr := startup.originalIngress()
				if originalErr != nil {
					t.Fatal(originalErr)
				}
				if len(owned.before) == 0 && host.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName].Config[startup.definition.Proxy.OwnershipKey()] != owned.beforeMarker {
					t.Fatal("original pending ingress marker changed")
				}
			}
			if input.Fault == "cleanup-uuid" {
				if !strings.Contains(err.Error(), "clean isolation ACL") || observer.commands != 2 {
					t.Fatalf("replacement after closure did not refuse cleanup: commands=%d err=%v", observer.commands, err)
				}
			}
			if input.Fault == "expanded" && observer.commands != 0 {
				t.Fatal("expanded native plan reached effect")
			}
			return
		}
		if err != nil {
			t.Fatal("original execution", err)
		}
		if observer.commands != 1 {
			t.Fatalf("effect dispatches=%d", observer.commands)
		}
		return
	}
	program, _, _ := orphanIngressFixture(t, true)
	root := program.options.RepositoryRoot
	path := filepath.Join(root, "config/profiles/sample/resources/relay.res")
	descriptor, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, append(descriptor, []byte("STARTUP=bringup\n")...), 0600)
	handler := originalStartupProcessHandler
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/resources/relay/handler.sh"), []byte(handler), 0700)
	cli, loaded, host := originalStartupProcessCLI(t, root, program.options.Environment)
	if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	startup, err := cli.prepareResourceStartup(context.Background(), loaded)
	if err != nil || startup == nil {
		t.Fatal("original preparation", err)
	}
	startup.intent.instanceUUID = "original-startup-uuid"
	original, err := captureOwnerResourceStartup(startup, cli, root)
	if err != nil {
		t.Fatal("original capture", err)
	}
	start, err := captureOwnerStart(context.Background(), cli, loaded, root)
	if err != nil || start == nil || !bytes.Equal(start.Startup, original) {
		t.Fatal("owner lifecycle did not retain original startup", err)
	}
	changed := loaded
	changed.Environment = maps.Clone(loaded.Environment)
	changed.Environment["EXTERNAL"] = "/replacement-source-\xfe"
	if _, err := restoreOwnerResourceStartup(original, cli, changed, root); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed original source/config admitted", err)
	}
	// Lossless reconstitution includes the complete ingress snapshots, not just
	// the public projection and fingerprints.
	restored, err := restoreOwnerResourceStartup(original, cli, loaded, root)
	if err != nil || !reflect.DeepEqual(restored.ingress.preview, startup.ingress.preview) {
		t.Fatal("original ingress snapshot changed", err)
	}
	host.originalPending()
	pending, err := cli.prepareResourceStartup(context.Background(), loaded)
	if err != nil || pending == nil {
		t.Fatal("original pending-marker prepare", err)
	}
	pending.intent.instanceUUID = "original-startup-uuid"
	pendingOriginal, err := captureOwnerResourceStartup(pending, cli, root)
	if err != nil {
		t.Fatal(err)
	}
	retained := testkit.TempDir(t)
	for _, relative := range []string{"config/profiles/sample/resources/relay.res", "config/profiles/sample/resources/relay/handler.sh"} {
		body, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(retained, relative)), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(retained, relative), body, 0700)
	}
	// A live-path edit after the source guard must not replace execution bytes.
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/resources/relay/handler.sh"), []byte("#!/bin/sh\nexit 77\n"), 0700)
	input := ownerStartupProcessFixture{Root: root, Retained: retained, Environment: program.options.Environment, Original: original}
	fixture := filepath.Join(testkit.TempDir(t), "original.json")
	_, release, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner, err := uninstall.BorrowOwnerLock(loaded.Context.Paths.DataHome)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	defer owner.Close()
	for _, fault := range []string{"", "verify", "intent", "expanded", "uuid", "pending-intent", "pending-verify", "cleanup-uuid", "loss"} {
		t.Run("fresh-"+fault, func(t *testing.T) {
			for _, name := range []string{"activated", "committed", "closed", "verify-fail", "expanded-fail", "loss-fail", "dispatch-intent", "dispatch-terminal"} {
				if err := os.Remove(filepath.Join(loaded.Context.Paths.DataHome, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			kind := strings.TrimPrefix(fault, "pending-")
			if kind == "verify" || kind == "expanded" || kind == "loss" {
				testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.DataHome, kind+"-fail"), nil, 0600)
			}
			input.Fault = fault
			input.Original = original
			if strings.HasPrefix(fault, "pending-") {
				input.Original = pendingOriginal
			}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, fixture, body, 0600)
			command := exec.Command(os.Args[0], "-test.run=^TestOwnerStartupRecordFreshProcessExecutesOriginalResource$")
			command.Env = append(os.Environ(), "SUBYARD_TEST_ORIGINAL_STARTUP="+fixture)
			command.ExtraFiles = []*os.File{owner}
			if fault == "loss" {
				command.Stdout, command.Stderr = io.Discard, io.Discard
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				defer command.Process.Kill()
				end := time.Now().Add(5 * time.Second)
				pid := 0
				for time.Now().Before(end) {
					raw, _ := os.ReadFile(filepath.Join(loaded.Context.Paths.DataHome, "handler-pid"))
					pid, _ = strconv.Atoi(string(raw))
					if pid > 0 {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if pid == 0 {
					_ = command.Process.Kill()
					_ = command.Wait()
					t.Fatal("resource child was not dispatched")
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
				contender, err := os.OpenFile(uninstall.OwnerLockPath(loaded.Context.Paths.DataHome), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer contender.Close()
				if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
					t.Fatal("resource child lost original owner OFD")
				}
				if _, err := os.Stat(filepath.Join(loaded.Context.Paths.DataHome, "dispatch-intent")); err != nil {
					t.Fatal("effect preceded dispatch intent", err)
				}
				if _, err := os.Stat(filepath.Join(loaded.Context.Paths.DataHome, "dispatch-terminal")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("helper loss invented terminal")
				}
				testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.DataHome, "release"), nil, 0600)
				for time.Now().Before(end) {
					if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
				t.Fatal("resource foreground retained original OFD after drain")
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("fresh original startup: %v\n%s", err, output)
			}
			if fault == "" {
				for name, want := range map[string]string{"activated": "original handler applied once", "committed": "original-startup-uuid"} {
					body, err := os.ReadFile(filepath.Join(loaded.Context.Paths.DataHome, name))
					if err != nil || string(body) != want {
						t.Fatalf("%s=%q err=%v", name, body, err)
					}
				}
			} else if kind == "verify" || kind == "intent" {
				if body, err := os.ReadFile(filepath.Join(loaded.Context.Paths.DataHome, "closed")); err != nil || string(body) != "original-startup-uuid" {
					t.Fatal("original guarded rollback missing", err)
				}
				if _, err := os.Stat(filepath.Join(loaded.Context.Paths.DataHome, "activated")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("original guest rollback missing", err)
				}
			}
		})
	}
	var record ownerResourceStartup
	if err := nativejson.DecodeLosslessNativeJSON(original, &record, ownerTeardownRecordLimit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(record.Steps), "original fixture service") {
		t.Fatal("original steps missing")
	}
}

const originalStartupProcessHandler = `#!/usr/bin/env python3
import hashlib,json,os,pathlib,sys,time
root=pathlib.Path(os.environ['SUBYARD_HOME'])
state=root/'activated'
mode=os.environ.get('SUBYARD_RESOURCE_MODE')
if sys.argv[1]=='is-up': sys.exit(0 if state.exists() else 1)
if sys.argv[1]=='rollback-ingress':
    state.unlink(missing_ok=True)
    sys.exit(0)
if mode=='apply':
    assert os.environ['SUBYARD_RESOURCE_BINDING']=='a'*64
    assert len(json.loads(os.environ['SUBYARD_RESOURCE_STEPS']))==1
    assert pathlib.Path('/proc/self/fd/'+os.environ['SUBYARD_OWNER_LOCK_FD']).exists()
    assert not state.exists()
    state.write_text('original handler applied once')
    if (root/'loss-fail').exists():
        (root/'handler-pid').write_text(str(os.getpid()))
        while not (root/'release').exists(): time.sleep(.01)
    sys.exit(0)
ready=state.exists() and not (mode=='verify' and (root/'verify-fail').exists())
step=dict(id='service',target='original fixture service',observed='running' if ready else 'inactive',desired='running',decision='skip' if ready else 'apply',verify='read original fixture service',consequence='start original fixture service')
steps=[step]
if (root/'expanded-fail').exists():
    steps.append(dict(step,id='foreign',target='expanded target'))
print(json.dumps(dict(schema='yard.resource-action-assessment.v2',action='up',changed=not ready,steps=steps,binding='a'*64,consequences=[] if ready else ['start original fixture service'])))
`
