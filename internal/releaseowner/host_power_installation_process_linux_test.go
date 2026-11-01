//go:build linux

package releaseowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

type installationProcessInput struct {
	RuntimeRoot, Socket string
	Environment         []string
	Yard                domain.Context
	Record              reconcileruntime.OwnerPublicationRecord
	Sources             []byte
	Before              OwnerBefore
}
type installationProcessComponent struct {
	noEffects
	input   installationProcessInput
	runtime reconcileruntime.Runtime
	result  *ports.NativeHostPowerInstallation
}

func (c *installationProcessComponent) BindOwnerResults(ctx context.Context, b OwnerBefore, _ string, created []ports.NativeOwnerCreated) error {
	if b != c.input.Before || len(created) != 0 {
		return errors.New("original install-only capsule differs")
	}
	o, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeHostPowerInstallationObserver)
	if !ok {
		return errors.New("original installation reader missing")
	}
	v, e := o.ReadHostPowerInstallation("")
	if e != nil || v == nil || v.Output != c.input.Record.OutputDigest || v.HostSource != string(c.input.Record.Power.Source) {
		return errors.New("original installation result unavailable")
	}
	c.result = v
	return nil
}
func (c *installationProcessComponent) ObserveOwnerAction(context.Context, OwnerBefore, string, OwnerActionReference) (string, error) {
	return "desired", nil
}
func (c *installationProcessComponent) ApplyOwnerAction(ctx context.Context, before OwnerBefore, token string, action OwnerActionReference) error {
	runtime := c.runtime
	var e error
	if c.result != nil {
		checked := ports.NativeOperationObserverFrom(ctx).(ports.NativeCheckedInvocation)
		_, recipe, err := checked.CheckedInvocation()
		if err != nil || recipe == c.result.Recipe {
			return errors.New("cross-action consumer recipe did not differ")
		}
		runtime, e = runtime.ImportOwnerPublicationInstallation(ctx, *c.result)
		if e != nil {
			return errors.Join(e, publicComponentFailure{message: "cross-action runtime import refused"})
		}
	}
	power, e := runtime.RestoreOwnerHostPower(ctx, c.input.Record.Power)
	if e != nil {
		return e
	}
	defer power.Close()
	sources, e := runtime.BindInstalledRuntimeContext(ctx, c.input.Sources)
	if e != nil {
		return e
	}
	defer sources.Close()
	if e = runtime.ApplyOwnerPublicationPower(ctx, power, sources, &c.input.Record.Consumer); e != nil {
		return e
	}
	if c.result != nil {
		return nil
	}
	// The official SDK records the exact accepted operation's status400 before
	// this real production singleton cleanup join reaches session.apply.
	client := incusclient.New(c.input.Socket)
	e = client.SetOriginalInstancePowerIf(ctx, "project", "yard", "start", false, "original", domain.YardContainer)
	return errors.Join(e, nil)
}
func TestNativeHostInstallHelperProcess(t *testing.T) {
	if path := os.Getenv("SUBYARD_HOST_INSTALL_HELPER_INPUT"); path != "" {
		raw, e := os.ReadFile(path)
		var input installationProcessInput
		if e != nil || json.Unmarshal(raw, &input) != nil {
			os.Exit(3)
		}
		e = Serve(context.Background(), os.Stdin, os.Stdout, func(_ context.Context, _ Context, _ *os.File) (Components, error) {
			definitions, e := profile.Load(input.RuntimeRoot)
			if e != nil {
				return nil, e
			}
			runtime := reconcileruntime.Runtime{RepositoryRoot: input.RuntimeRoot, Environment: input.Environment, Yard: input.Yard, Profiles: definitions, Stderr: os.Stderr}
			runtime, e = runtime.RestoreOwnerPublication(&input.Record)
			if e != nil {
				return nil, e
			}
			return &installationProcessComponent{input: input, runtime: runtime}, nil
		})
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(3)
		}
		os.Exit(0)
	}
	source := os.Getenv("SUBYARD_HOST_INSTALL_RESULT_SOURCE")
	if source == "" {
		t.Skip("paired actual native installer process fixture requires its own normal stage")
	}
	if os.Getenv("SUBYARD_HOST_INSTALL_PROCESS_NS") != "1" {
		cmd := exec.Command("unshare", "--user", "--map-root-user", "--mount", os.Args[0], "-test.v", "-test.run=^TestNativeHostInstallHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SUBYARD_HOST_INSTALL_PROCESS_NS=1")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatal("required native helper namespace", e, string(out))
		}
		t.Log(string(out))
		return
	}
	source, e := filepath.EvalSymlinks(source)
	if e != nil {
		t.Fatal(e)
	}
	root := testkit.TempDir(t)
	repo := filepath.Join(root, "runtime")
	if e := os.Mkdir(repo, 0700); e != nil {
		t.Fatal(e)
	}
	e = filepath.WalkDir(source, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(repo, rel)
		info, e := d.Info()
		if e != nil {
			return e
		}
		if d.IsDir() {
			return os.Mkdir(dst, 0700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		testkit.WriteFile(t, dst, raw, info.Mode().Perm())
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	data := filepath.Join(root, "data")
	if e = os.Mkdir(data, 0700); e != nil {
		t.Fatal(e)
	}
	installed := filepath.Join(root, "installed")
	if e = os.Mkdir(installed, 0755); e != nil {
		t.Fatal(e)
	}
	contracts, e := ownerpublication.Load(repo)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := ownerpublication.Encode(contracts)
	if e != nil {
		t.Fatal(e)
	}
	engine := filepath.Join(repo, "bin/yard-engine")
	count := filepath.Join(root, "install-count")
	testkit.WriteFile(t, engine, []byte("#!/bin/sh\ncase \"$*\" in _publication-contracts) printf '%s\\n' '"+encoded+"';; '_network-lock ensure') printf x >> '"+count+"';; *) exit 64;; esac\n"), 0755)
	passive := filepath.Join(installed, ownerpublication.HandoffService)
	manager := filepath.Join(root, "systemctl")
	testkit.WriteFile(t, manager, []byte("#!/bin/sh\nif [ \"$1\" = show ] && [ \"$2\" != subyard-publication-reconcile.service ]; then printf 'LoadState=loaded\\nNeedDaemonReload=no\\n'; exit 0; fi\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nNeedDaemonReload=no\\nFragmentPath="+passive+"\\nDropInPaths=\\n'; fi\n"), 0755)
	if e = unix.Mount(manager, "/usr/bin/systemctl", "", unix.MS_BIND, ""); e != nil {
		t.Fatal(e)
	}
	defer unix.Unmount("/usr/bin/systemctl", 0)
	s, b, facts := nativeFactFixture(t)
	config := s.root.Name()
	definitions, e := profile.Load(repo)
	if e != nil {
		t.Fatal(e)
	}
	environment := []string{"PATH=/usr/bin:/bin", "SUBYARD_USER=root", "SUBYARD_ENGINE_CONTEXT=1", "SUBYARD_ENGINE_CONTEXT_SCHEMA=1", "ASSUME_YES=1", "ALLOWS_CODING_TOOLS=false", "SUBYARD_HOME=" + data, "SUBYARD_CONFIG_HOME=" + config, "SUBYARD_POWER_ENGINE_SOURCE=" + engine, "SUBYARD_POWER_LIBEXEC_DIR=" + installed, "SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(installed, "engine"), "SUBYARD_POWER_UNIT_PATH=" + filepath.Join(installed, "power.service")}
	environment = append(environment, "SUBYARD_OPERATOR_HOME="+data, "SUBYARD_CONFIG_DIR="+repo, "STORAGE_PATH="+filepath.Join(data, "storage"), "HOST_BASE="+filepath.Join(data, "host"), "RESTRICTED_DISK_PATHS="+data, "ACCESS_KIND=local", "YARD_KIND=container", "YARD_INSTANCE_NAME=yard", "INCUS_PROJECT=project", "INCUS_BRIDGE=incusbr0", "SSH_HOST=yard", "DEV_USER=dev", "DEV_UID=1000", "DEV_SUDO=0", "FORWARD_SSH_AGENT=0", "NESTED_E2E_VMS=0")
	yard := domain.Context{Paths: domain.RuntimePaths{DataHome: data, ConfigHome: config, OperatorHome: data}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: repo, Environment: environment, Yard: yard, Profiles: definitions, Stderr: io.Discard}
	record, e := runtime.PrepareOwnerPublication(context.Background())
	if e != nil {
		t.Fatal("prepare original publication", e)
	}
	sources, e := runtime.PrepareOwnerRuntimeSources(context.Background())
	if e != nil {
		t.Fatal("prepare original source", e)
	}
	defer sources.Close()
	before := OwnerBefore{State: "legacy-directory", Registration: b.Artifact, Overrides: b.Artifact, Controller: b.Artifact, PreparedRecipe: sources.Binding()}
	token := strings.Repeat("1", 32)
	server, e := testkit.NewIncusServer(testkit.TempDir(t))
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	server.SetInstance("project", "yard", map[string]any{"name": "yard", "status": "Stopped", "type": "container", "config": map[string]string{"volatile.uuid": "original"}, "expanded_config": map[string]string{"volatile.uuid": "original"}})
	server.QueuePowerErrors("confirmed initial start failure")
	input := installationProcessInput{repo, server.SocketPath, environment, yard, *record, sources.Recipe(), before}
	raw, _ := json.Marshal(input)
	inputPath := filepath.Join(root, "input.json")
	testkit.WriteFile(t, inputPath, raw, 0600)
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	image, e := os.ReadFile(executable)
	if e != nil {
		t.Fatal(e)
	}
	b.Helper = digest(image)
	updatePath := filepath.Join(config, "migrations/update.lock")
	testkit.WriteFile(t, updatePath, nil, 0600)
	update, e := os.OpenFile(updatePath, os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer update.Close()
	if e = unix.Flock(int(update.Fd()), unix.LOCK_EX); e != nil {
		t.Fatal(e)
	}
	_, release, e := uninstall.OwnerLock(context.Background(), data, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	owner, e := uninstall.BorrowOwnerLock(data)
	if e != nil || owner == nil {
		t.Fatal(e)
	}
	defer owner.Close()
	repoFD, e := os.Open(repo)
	if e != nil {
		t.Fatal(e)
	}
	defer repoFD.Close()
	rootID, _ := IdentityOf(s.root)
	repoID, _ := IdentityOf(repoFD)
	updateID, _ := IdentityOf(update)
	ownerID, _ := IdentityOf(owner)
	ownerRaw, _ := json.Marshal(ownerID)
	original := b
	producer := OwnerActionReference{Group: original, Program: b.Artifact, Action: OwnerAction{Operation: "power", Slot: 1}}
	for _, ordinal := range []uint64{1, 2, 3, 4} {
		b.Ordinal = ordinal
		reservation, _ := json.Marshal(nativeReservation{1, b, 2570, 16 << 20})
		testkit.WriteFile(t, filepath.Join(facts, factName(ordinal, "reservation")), append(reservation, '\n'), 0600)
		action := producer
		if ordinal > 1 {
			action.Index, action.Action = ordinal-1, OwnerAction{Operation: "init", Slot: 0}
		}
		request := Request{Op: "apply-owner-action", Binding: &b, Before: &before, Token: token, Action: &action}
		recipe, _ := recipeFingerprint(request)
		intent, _ := json.Marshal(Intent{1, b, "owner-registration", "test-yard", b.Artifact, b.Artifact, b.Artifact, recipe})
		testkit.WriteFile(t, filepath.Join(facts, factName(ordinal, "intent")), append(intent, '\n'), 0600)
		pair, e := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if e != nil {
			t.Fatal(e)
		}
		parent := os.NewFile(uintptr(pair[0]), "result parent")
		child := os.NewFile(uintptr(pair[1]), "result helper")
		cmd := exec.Command(executable, "-test.run=^TestNativeHostInstallHelperProcess$")
		cmd.Env = append(os.Environ(), "SUBYARD_HOST_INSTALL_HELPER_INPUT="+inputPath)
		cmd.Stdin = child
		cmd.Stdout = child
		var diagnostics strings.Builder
		cmd.Stderr = &diagnostics
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		child.Close()
		peer := nativeTestPeer{t: t, socket: parent, timeout: 30 * time.Second}
		c := Context{Session: b.Session, Artifact: b.Artifact, DataHome: data, ConfigHome: config, RuntimeRoot: repo, RepositoryRoot: repo, Yard: "default", Target: "next", Root: rootID, Repository: repoID}
		peer.request(Request{Op: "initialize", Context: &c}, s.root, repoFD)
		peer.request(Request{Op: "claim-lease", Binding: &b, Update: &updateID, Owner: ownerRaw}, update, owner)
		if ordinal >= 3 {
			leaf := "001.terminal.json"
			if ordinal == 4 {
				leaf = "001.host-power-installation.json"
			}
			path := filepath.Join(facts, leaf)
			saved, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Remove(path); e != nil {
				t.Fatal(e)
			}
			raw := peer.frame(15, Request{Op: "bind-owner-results", Binding: &b, Producer: &original, Before: &before, Token: token, Action: &action, ProducerAction: &producer})
			var refused Reply
			if e = json.Unmarshal(raw, &refused); e != nil {
				t.Fatal(e)
			}
			parent.Close()
			if e = cmd.Wait(); e != nil {
				t.Fatal(e, diagnostics.String())
			}
			testkit.WriteFile(t, path, saved, 0600)
			if refused.Code == "" {
				t.Fatal("fresh helper admitted missing original whole/result", leaf)
			}
			continue
		}
		if ordinal == 2 {
			peer.request(Request{Op: "bind-owner-results", Binding: &b, Producer: &original, Before: &before, Token: token, Action: &action, ProducerAction: &producer})
		}
		replyRaw := peer.frame(15, request)
		var reply Reply
		if e = json.Unmarshal(replyRaw, &reply); e != nil {
			t.Fatal(e)
		}
		parent.Close()
		if e = cmd.Wait(); e != nil {
			t.Fatal("actual helper exit", e, diagnostics.String())
		}
		if ordinal == 1 {
			if reply.Code != "failed" || reply.Terminal == nil || reply.Terminal.State != "failed" {
				t.Fatal("successful install then SDKfailure did not produce WHOLEFAILED", reply, diagnostics.String())
			}
			if _, e = os.Stat(filepath.Join(facts, "001.host-power-installation.json")); e != nil {
				t.Fatal("successful installation evidence lost", e)
			}
			if output := os.Getenv("SUBYARD_NATIVE_HOST_INSTALL_CORPUS"); output != "" {
				files := map[string][]byte{}
				entries, _ := os.ReadDir(facts)
				for _, entry := range entries {
					raw, e := os.ReadFile(filepath.Join(facts, entry.Name()))
					if e != nil {
						t.Fatal(e)
					}
					files[entry.Name()] = raw
				}
				raw, _ := json.Marshal(struct {
					Binding Binding
					Files   map[string][]byte
				}{b, files})
				if e = os.WriteFile(output, raw, 0600); e != nil {
					t.Fatal(e)
				}
			}
		} else if reply.Code != "" || reply.Terminal == nil || reply.Terminal.State != "complete" {
			t.Fatalf("fresh original retry/import code=%s diagnostic=%+v stderr=%s", reply.Code, reply.Diagnostic, diagnostics.String())
		}
	}
	n, e := os.ReadFile(count)
	if e != nil || string(n) != "x" {
		t.Fatal("fresh original retry repeated installation", string(n), e)
	}
	if calls := server.PowerCalls(); len(calls) != 1 {
		t.Fatal("retry unexpectedly replayed physical startup", calls)
	}
}
