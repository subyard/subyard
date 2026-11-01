//go:build linux

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

const originalCallbackDiagnosticLimit = 32 << 10

type originalCallbackDiagnostics struct {
	body      []byte
	truncated bool
}

func (d *originalCallbackDiagnostics) Write(p []byte) (int, error) {
	n := min(len(p), originalCallbackDiagnosticLimit-len(d.body))
	d.body = append(d.body, p[:n]...)
	d.truncated = d.truncated || n != len(p)
	return len(p), nil
}

// Only this synthetic fixture may retain an untyped component error. The
// production protocol still chooses its public diagnostic and UNKNOWN status.
type originalCallbackComponent struct {
	*releaseOwnerComponents
	diagnostics io.Writer
}

func (c *originalCallbackComponent) ApplyOwnerAction(ctx context.Context, before releaseowner.OwnerBefore, token string, action releaseowner.OwnerActionReference) error {
	err := c.releaseOwnerComponents.ApplyOwnerAction(ctx, before, token, action)
	if err != nil {
		var diagnostic originalCallbackDiagnostics
		fmt.Fprintf(&diagnostic, "original callback ApplyOwnerAction error type=%T value=%q\n", err, err.Error())
		_, _ = c.diagnostics.Write(diagnostic.body)
		if diagnostic.truncated {
			fmt.Fprintln(c.diagnostics, "original callback ApplyOwnerAction error truncated")
		}
	}
	return err
}

func originalCallbackFactDiagnostics(directory string, ordinal uint64) string {
	var diagnostic originalCallbackDiagnostics
	entries, err := os.ReadDir(directory)
	if err != nil {
		fmt.Fprintf(&diagnostic, "original callback facts unavailable: %v\n", err)
		return string(diagnostic.body)
	}
	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.Name()] = true
	}
	fmt.Fprintf(&diagnostic, "original callback ordinal=%d accepted-present=%t terminal-present=%t\n", ordinal, present[fmt.Sprintf("%03d.accepted.json", ordinal)], present[fmt.Sprintf("%03d.terminal.json", ordinal)])
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > 128 || strings.Trim(name, "0123456789abcdefghijklmnopqrstuvwxyz.-") != "" || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			fmt.Fprintf(&diagnostic, "original callback fact %s unavailable: %v\n", name, err)
			continue
		}
		fmt.Fprintf(&diagnostic, "original callback fact %s mode=%#o bytes=%d\n", name, info.Mode(), info.Size())
		if !info.Mode().IsRegular() || !(strings.HasSuffix(name, ".accepted.json") || strings.HasSuffix(name, "-accepted.json") || strings.HasSuffix(name, ".terminal.json") || strings.HasSuffix(name, "-terminal.json")) {
			continue
		}
		file, err := os.Open(filepath.Join(directory, name))
		if err != nil {
			fmt.Fprintf(&diagnostic, "original callback fact %s unreadable: %v\n", name, err)
			continue
		}
		// Select public fixture fields only: bindings, grants, recipes and other
		// fact payloads never enter the already captured callback-output log.
		var terminal struct {
			State      string                  `json:"state"`
			Operations []ports.NativeOperation `json:"operations"`
		}
		var operation ports.NativeOperation
		raw, err := io.ReadAll(io.LimitReader(file, originalCallbackDiagnosticLimit+1))
		_ = file.Close()
		if err != nil || len(raw) > originalCallbackDiagnosticLimit || json.Unmarshal(raw, &terminal) != nil || json.Unmarshal(raw, &operation) != nil {
			fmt.Fprintf(&diagnostic, "original callback fact %s summary unavailable\n", name)
			continue
		}
		if terminal.State != "" {
			fmt.Fprintf(&diagnostic, "original callback fact %s state=%q\n", name, terminal.State)
		}
		if operation.ID != "" {
			terminal.Operations = append(terminal.Operations, operation)
		}
		for _, operation := range terminal.Operations {
			fmt.Fprintf(&diagnostic, "original callback native-operation id=%q status=%d kind=%q project=%q resource=%q\n", operation.ID, operation.Status, operation.Kind, operation.Project, operation.Resource)
		}
	}
	if diagnostic.truncated {
		return string(diagnostic.body) + "\noriginal callback fact diagnostics truncated\n"
	}
	return string(diagnostic.body)
}

func TestOriginalCallbackFailureDiagnosticsPreserveCauseAndPrivateFacts(t *testing.T) {
	var diagnostic bytes.Buffer
	component := &originalCallbackComponent{releaseOwnerComponents: &releaseOwnerComponents{}, diagnostics: &diagnostic}
	err := component.ApplyOwnerAction(context.Background(), releaseowner.OwnerBefore{State: "legacy-directory", PreparedRecipe: "unbound"}, "unused", releaseowner.OwnerActionReference{Action: releaseowner.OwnerAction{Operation: "start", Slot: 1}})
	if !errors.Is(err, domain.ErrPlanStale) || !strings.Contains(diagnostic.String(), "original callback ApplyOwnerAction error") || !strings.Contains(diagnostic.String(), domain.ErrPlanStale.Error()) {
		t.Fatal("original component error lost or changed", err, diagnostic.String())
	}
	root := testkit.TempDir(t)
	const secret = "private-fixture-binding-and-recipe"
	operation := ports.NativeOperation{ID: "sdk-failure", Status: 400, Kind: "instance-state", Project: "project", Resource: "yard", Server: secret}
	raw, _ := json.Marshal(releaseowner.Terminal{Version: 1, Binding: releaseowner.Binding{Session: secret}, State: "accepted", Operations: []ports.NativeOperation{operation}})
	testkit.WriteFile(t, filepath.Join(root, "001.accepted.json"), raw, 0600)
	testkit.WriteFile(t, filepath.Join(root, "001.recipe.json"), []byte(secret), 0600)
	if err := os.Symlink(filepath.Join(root, "001.recipe.json"), filepath.Join(root, "001.operation-002-terminal.json")); err != nil {
		t.Fatal(err)
	}
	text := originalCallbackFactDiagnostics(root, 1)
	if !strings.Contains(text, "accepted-present=true terminal-present=false") || !strings.Contains(text, "001.recipe.json") || !strings.Contains(text, "state=\"accepted\"") || !strings.Contains(text, "native-operation id=\"sdk-failure\" status=400") || strings.Contains(text, secret) {
		t.Fatal("missing or unsafe original callback fact summary", text)
	}
}

func TestOriginalCallbackDiagnosticsRetainBoundedHostCheck(t *testing.T) {
	diagnostics := &originalCallbackDiagnostics{}
	err := (hostruntime.HostCheck{Output: diagnostics, Probe: func(context.Context, hostruntime.HostCheck) (hostruntime.HostFacts, error) {
		return hostruntime.HostFacts{OSName: "fixture", CPUFlags: []string{"vmx"}, KVM: true, CPUs: 2, MemoryBytes: 4 << 30, StorageKnown: true, StorageFree: 1 << 30, StoragePath: "/fixture", IncusPresent: true, IncusVersion: "6.23"}, nil
	}}).Run(context.Background(), hostruntime.CheckOptions{Strict: true})
	if !errors.Is(err, hostruntime.ErrHostNotReady) || !strings.Contains(string(diagnostics.body), "[fail] only 1 GiB free") || diagnostics.truncated {
		t.Fatal("missing original host-check reason", err, string(diagnostics.body), diagnostics.truncated)
	}
	diagnostics = &originalCallbackDiagnostics{}
	full := bytes.Repeat([]byte("x"), originalCallbackDiagnosticLimit)
	if n, err := diagnostics.Write(full); n != len(full) || err != nil || diagnostics.truncated {
		t.Fatal("exact diagnostic limit", n, err, diagnostics.truncated)
	}
	if n, err := diagnostics.Write([]byte("discarded")); n != len("discarded") || err != nil || !diagnostics.truncated || !bytes.Equal(diagnostics.body, full) {
		t.Fatal("diagnostic overflow changed output or caller result", n, err, diagnostics.truncated)
	}
}

func originalCallbackStorageFixture(t *testing.T, c *releaseOwnerComponents, parent string) string {
	t.Helper()
	root, err := os.MkdirTemp(parent, "subyard-original-callback-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ".marker")
	identity := filepath.Base(root) + "\n"
	testkit.WriteFile(t, marker, []byte(identity), 0600)
	t.Cleanup(func() {
		if raw, err := os.ReadFile(marker); err != nil || string(raw) != identity {
			t.Errorf("refusing cleanup of unmarked callback storage %s: %v", root, err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	storage := filepath.Join(root, "storage")
	// Persist this setting so the captured graph and fresh Serve processes use
	// the same backing filesystem, including loads without command overrides.
	host := filepath.Join(c.context.RepositoryRoot, "config/host.env")
	settings, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	settings = append(settings, []byte(fmt.Sprintf("\nSTORAGE_PATH='%s'\n", storage))...)
	testkit.WriteFile(t, host, settings, 0600)
	return storage
}

func TestOriginalCallbackStoragePreservesPinnedRoots(t *testing.T) {
	var storage string
	t.Run("configuration", func(t *testing.T) {
		c, _ := nativeActivationSourceFixture(t)
		powerRoot := filepath.Join(c.context.DataHome, "host-power")
		host := filepath.Join(c.context.RepositoryRoot, "config/host.env")
		testkit.WriteFile(t, host, []byte(fmt.Sprintf("SUBYARD_POWER_LIBEXEC_DIR='%s'\nSUBYARD_POWER_RECONCILER_PATH='%s'\nSUBYARD_POWER_UNIT_PATH='%s'\n", powerRoot, filepath.Join(powerRoot, "engine"), filepath.Join(powerRoot, "unit.service"))), 0600)
		if err := os.MkdirAll(filepath.Join(c.context.ConfigHome, "yards/e2e-yard"), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(c.context.ConfigHome, "yards/e2e-yard/config.env"), nil, 0600)
		storage = originalCallbackStorageFixture(t, c, testkit.TempDir(t))
		c.cli = c.cli.releaseActivationOperation()
		captured, err := c.load("e2e-yard")
		if err != nil {
			t.Fatal(err)
		}
		command, err := c.cli.loadContext("e2e-yard")
		if err != nil {
			t.Fatal(err)
		}
		for _, environment := range []map[string]string{captured.Environment, command.Environment} {
			for key, expected := range map[string]string{"SUBYARD_POWER_LIBEXEC_DIR": powerRoot, "SUBYARD_POWER_RECONCILER_PATH": filepath.Join(powerRoot, "engine"), "SUBYARD_POWER_UNIT_PATH": filepath.Join(powerRoot, "unit.service")} {
				if environment[key] != expected {
					t.Errorf("loaded private power setting %s differs from fixture intent", key)
				}
			}
		}
		for _, loaded := range []domain.Context{captured.Context, command.Context} {
			if loaded.Paths.StoragePath != storage || loaded.Paths.DataHome != c.context.DataHome || loaded.Paths.ConfigHome != c.context.ConfigHome || loaded.Paths.RepositoryRoot != c.context.RepositoryRoot {
				t.Fatal("callback storage configuration changed pinned roots", loaded.Paths)
			}
		}
	})
	if _, err := os.Stat(filepath.Dir(storage)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("callback storage fixture survived cleanup", err)
	}
}

const originalCallbackChildKey = "SUBYARD_TEST_ORIGINAL_CALLBACK_CHILD"

func originalCallbackChildPath(t *testing.T) string {
	t.Helper()
	path := os.Getenv(originalCallbackChildKey)
	// This selector belongs to the test process, not the sealed native runtime.
	if err := os.Unsetenv(originalCallbackChildKey); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOriginalCallbackFreshOriginalBind(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH_ENGINE") == "" {
		t.Skip("requires the normal owner graph engine")
	}
	for _, changed := range []string{"unchanged", "configuration", "source", "custom-storage", "custom-storage-changed"} {
		t.Run(changed, func(t *testing.T) {
			c, home, _ := ownerOriginalGraphFixture(t)
			var provision string
			if strings.HasPrefix(changed, "custom-storage") {
				storage := filepath.Join(c.context.RepositoryRoot, "custom-storage")
				if err := os.Mkdir(storage, 0700); err != nil {
					t.Fatal(err)
				}
				provision = filepath.Join(storage, "provision.sh")
				testkit.WriteFile(t, provision, []byte("#!/bin/sh\nexit 0\n"), 0700)
				// Original authoring drafts read persisted inputs rather than
				// allowing command overrides to mask their source configuration.
				host := filepath.Join(c.context.RepositoryRoot, "config/host.env")
				raw, err := os.ReadFile(host)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, []byte(fmt.Sprintf("\nSTORAGE_PATH='%s'\nCCUSAGE_PROVISION='%s'\n", storage, provision))...)
				testkit.WriteFile(t, host, raw, 0600)
				for _, env := range []map[string]string{c.cli.env, c.cli.baseEnv} {
					env["STORAGE_PATH"] = storage
					env["CCUSAGE_PROVISION"] = provision
				}
			}
			operator, err := user.Current()
			if err != nil {
				t.Fatal(err)
			}
			for _, env := range []map[string]string{c.cli.env, c.cli.baseEnv} {
				env["SUBYARD_USER"] = operator.Username
			}
			if _, _, err := c.CaptureInventory(context.Background()); err != nil {
				t.Fatal(err)
			}
			observed, err := c.cli.ownerRegistrationTransition(c.request()).Prepare(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			before := releaseowner.OwnerBefore{State: string(observed.State), Registration: string(observed.Registration), Overrides: string(observed.Overrides), Controller: string(observed.Controller), SharedImages: observed.SharedImages}
			original, err := c.prepareOwnerChildren(context.Background(), before)
			if err != nil {
				t.Fatal("capture", err)
			}
			defer original.Close()
			if provision != "" {
				for yard, baseline := range original.baselines {
					if _, present := baseline.files[provision]; !present {
						t.Fatal("custom storage input is outside owning baseline", yard)
					}
				}
			}
			for key, value := range environmentMap(os.Environ()) {
				if _, present := c.cli.env[key]; !present {
					t.Setenv(key, value)
					if err := os.Unsetenv(key); err != nil {
						t.Fatal(err)
					}
				}
			}
			for key, value := range c.cli.env {
				t.Setenv(key, value)
			}
			t.Setenv(originalCallbackChildKey, "dispatch-only")
			if originalCallbackChildPath(t) != "dispatch-only" || os.Getenv(originalCallbackChildKey) != "" {
				t.Fatal("child dispatch selector entered native runtime inputs")
			}

			repository, err := os.Open(c.context.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			defer uninstall.UseArtifactGuard(repositoryRoot(t))
			value := c.context
			value.PhysicalRepositoryRoot = fmt.Sprintf("/proc/self/fd/%d", repository.Fd())
			if changed == "configuration" {
				path := filepath.Join(c.context.ConfigHome, ".sync/settings/config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("SSH_PORT=2299\n"), 0600)
			}
			if changed == "source" {
				path := filepath.Join(c.context.RepositoryRoot, "config/subyard.env")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, append(raw, []byte("# changed original source\n")...), 0600)
			}
			if changed == "custom-storage-changed" {
				testkit.WriteFile(t, provision, []byte("#!/bin/sh\nexit 1\n"), 0700)
			}
			component, err := newReleaseOwnerComponents(context.Background(), value, c.root)
			if err != nil {
				t.Fatal(err)
			}
			fresh := component.(*releaseOwnerComponents)
			defer fresh.Close()
			fresh.cli.options.Incus = c.cli.options.Incus
			fresh.cli.options.NetworkPolicy = c.cli.options.NetworkPolicy
			fresh.cli.options.AdapterRunner = c.cli.options.AdapterRunner
			if _, _, err := fresh.CaptureInventory(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := fresh.bindOwnerChildren(context.Background(), original.binding(), original.raw); err != nil {
				t.Fatal("inert bind", err)
			}
			_, unlock, err := uninstall.OwnerLock(context.Background(), c.context.DataHome, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			observer := &ownerGraphObserver{}
			ctx := ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), nativeValueDigest(c.context.RepositoryRoot)), c.context.DataHome)
			err = fresh.bindOwnerChildrenForApply(ctx, original.binding(), original.raw, strings.Repeat("2", 32))
			unchanged := changed == "unchanged" || changed == "custom-storage"
			if unchanged && err != nil || !unchanged && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("fresh original binding", changed, err)
			}
			if !unchanged {
				return
			}
			p := fresh.ownerChildren
			if p == nil || p.binding() != original.binding() || p.operation == nil {
				t.Fatal("original child binding unavailable")
			}
			baseline := p.baselines[testyardmigration.LegacyYard]
			child, ok := p.inits[testyardmigration.LegacyYard]
			if baseline == nil || baseline.ownerSource == nil || baseline.initial.Context.YardName != testyardmigration.LegacyYard || !ok || child.power == nil || !child.power.Present {
				t.Fatal("original legacy baseline/power identity unavailable")
			}
			// Mirror the owning callback's local setup after original capture.
			runner := c.cli.options.AdapterRunner.(ownerGraphRunner)
			instance := c.cli.options.Incus.(*ownerGraphReader).Instances["subyard-e2e-yard/yard-e2e-yard"]
			input, err := json.Marshal(callbackContinuationInput{c.context.RepositoryRoot, runner.socket, instance})
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(home, "callback-input.json"), input, 0600)
			facts := filepath.Join(home, "migrations/native-custody/v1/callback-transaction")
			if err = os.MkdirAll(facts, 0700); err != nil {
				t.Fatal(err)
			}
			updatePath := filepath.Join(home, "migrations/update.lock")
			testkit.WriteFile(t, updatePath, nil, 0600)
			update, err := os.OpenFile(updatePath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer update.Close()
			if err = unix.Flock(int(update.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			owner, err := uninstall.BorrowOwnerLock(c.context.DataHome)
			if err != nil || owner == nil {
				t.Fatal("held original owner barrier unavailable", err)
			}
			defer owner.Close()
			ownerInfo, err := owner.Stat()
			namedOwner, statErr := os.Stat(uninstall.OwnerLockPath(c.context.DataHome))
			if err != nil || statErr != nil || !os.SameFile(ownerInfo, namedOwner) {
				t.Fatal("original owner barrier identity changed")
			}
			beforeCommands := observer.commands
			if err := baseline.check(ctx, p.operation); err != nil {
				t.Fatal("legacy baseline before power", err)
			}
			prepared, err := child.runtime.RestoreOwnerHostPower(ctx, *child.power)
			if err != nil {
				t.Fatal("original host power restore", err)
			}
			if prepared == nil {
				t.Fatal("original host-power preparation absent")
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
			if observer.commands != beforeCommands {
				t.Fatal("readonly baseline/host-power restore dispatched a command")
			}
			if changed == "unchanged" {
				record, err := original.record()
				if err != nil {
					t.Fatal(err)
				}
				capture, err := record.capture()
				if err != nil {
					t.Fatal(err)
				}
				actionBefore := before
				actionBefore.PreparedRecipe, actionBefore.Capture = original.binding(), &capture
				filesBefore, err := operationReplayFiles(home)
				if err != nil {
					t.Fatal(err)
				}
				action := releaseowner.OwnerActionReference{Program: strings.Repeat("a", 64), Action: releaseowner.OwnerAction{Operation: "validate-original"}}
				if err := fresh.ApplyOwnerAction(ctx, actionBefore, strings.Repeat("2", 32), action); err != nil {
					t.Fatalf("first original action refused: stale=%t type=%T", errors.Is(err, domain.ErrPlanStale), err)
				}
				filesAfter, err := operationReplayFiles(home)
				if err != nil || !reflect.DeepEqual(filesBefore, filesAfter) {
					t.Fatal("first original action changed protected files")
				}
			}
			t.Log("post-capture baseline and original host-power restore passed without effects")
		})
	}
}

// Only inventory/image preparation is simulated. The original callback's
// UUID-conditional power, publication and config leaves use the real SDK.
type callbackContinuationHost struct {
	*ownerGraphReader
	client *incusclient.Client
}

func (h *callbackContinuationHost) Instance(ctx context.Context, p, n string) (ports.InstanceInfo, error) {
	return h.client.Instance(ctx, p, n)
}
func (h *callbackContinuationHost) SetOriginalInstancePowerIf(ctx context.Context, p, n, a string, f bool, u string, k domain.YardKind) error {
	return h.client.SetOriginalInstancePowerIf(ctx, p, n, a, f, u, k)
}
func (h *callbackContinuationHost) SetOriginalInstanceConfigIf(ctx context.Context, p, n, u string, k domain.YardKind, b, v map[string]string) error {
	return h.client.SetOriginalInstanceConfigIf(ctx, p, n, u, k, b, v)
}
func (h *callbackContinuationHost) ReconcileOriginalPublicationsIf(ctx context.Context, p, n, u string, k domain.YardKind, c []ownerpublication.Contract, a []netip.Addr, checks ...func() error) ([]string, error) {
	return h.client.ReconcileOriginalPublicationsIf(ctx, p, n, u, k, c, a, checks...)
}
func (h *callbackContinuationHost) VMCPUConvergedIf(ctx context.Context, p, n string, a bool, u string) (bool, error) {
	return h.client.VMCPUConvergedIf(ctx, p, n, a, u)
}
func (h *callbackContinuationHost) ProbeOriginalInstanceAgentIf(ctx context.Context, p, n, u string, k domain.YardKind) (bool, error) {
	return h.client.ProbeOriginalInstanceAgentIf(ctx, p, n, u, k)
}

type callbackContinuationInput struct {
	Repository, Socket string
	Instance           ports.InstanceInfo
}

func callbackContinuationReader(input callbackContinuationInput) *callbackContinuationHost {
	return &callbackContinuationHost{ownerGraphReader: &ownerGraphReader{Incus: &testkit.Incus{ServerInfo: ports.ServerInfo{Version: "6.23.0"}, Instances: map[string]ports.InstanceInfo{input.Instance.Project + "/" + input.Instance.Name: input.Instance}}}, client: incusclient.New(input.Socket)}
}

// The callback's exact metadata writer needs an ETag even for a no-op.
// Its metadata leaf must leave the separately queued SDK start failure intact.
func TestOriginalCallbackSnapshotSupportsExactPowerEnsure(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("etag=%t", wrapped), func(t *testing.T) {
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			cfg := map[string]string{"volatile.uuid": "original", "user.subyard.desired_power": "running", "user.subyard.managed": "true", "user.subyard.name": "e2e-yard", "user.subyard.bridge": "incusbr0", "user.subyard.initialized": "true", "boot.autostart": "false"}
			yard := domain.Context{YardName: "e2e-yard", YardKind: domain.YardContainer, IncusProject: "p", YardInstanceName: "yard", IncusBridge: "incusbr0"}
			set := func() {
				server.SetInstance("p", "yard", map[string]any{"name": "yard", "type": "container", "status": "Stopped", "config": maps.Clone(cfg), "expanded_config": maps.Clone(cfg)})
			}
			set()
			socket := server.SocketPath
			if wrapped {
				socket = ownerPublicationSnapshotSocket(t, server)
			}
			client := incusclient.New(socket, "projects")
			cli := &CLI{options: Options{Incus: client}}
			power := application.PowerService{Instances: client, Config: ownerStartPowerConfig{cli: cli, yard: yard, uuid: "original"}}
			server.QueuePowerErrors("queued original callback start terminal400")
			intent, err := power.Ensure(context.Background(), yard)
			if !wrapped {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("bare fixture admitted an unbound metadata snapshot", err)
				}
			} else {
				if err != nil || intent.Desired != domain.PowerRunning {
					t.Fatal("exact metadata no-op failed", intent, err)
				}
				cfg["boot.autostart"] = "true"
				set()
				if _, err := power.Ensure(context.Background(), yard); err != nil {
					t.Fatal("conditional metadata update failed", err)
				}
				actual, err := client.Instance(context.Background(), "p", "yard")
				if err != nil || actual.Config["boot.autostart"] != "false" || actual.Config["volatile.uuid"] != "original" {
					t.Fatal("conditional update changed original identity", err)
				}
			}
			if len(server.PowerCalls()) != 0 {
				t.Fatal("metadata Ensure consumed queued start")
			}
			observer := &physicalStartupObserver{}
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			err = client.SetOriginalInstancePowerIf(ctx, "p", "yard", "start", false, "original", domain.YardContainer)
			if err == nil || !strings.Contains(err.Error(), "queued original callback start terminal400") || len(server.PowerCalls()) != 1 {
				t.Fatal("queued SDK terminal400 was not preserved", err)
			}
			var failure *ports.NativeOperationFailure
			if !errors.As(err, &failure) || observer.unknown || len(observer.started) != 1 || len(observer.accepted) != 1 || len(observer.terminal) != 1 {
				t.Fatal("queued SDK failure lost exact closed census", err, observer)
			}
			operation := observer.terminal[0]
			if operation.Kind != "instance-power" || operation.Status != 400 || operation.ID == "" || operation.Server == "" || operation.Project != "p" || operation.Resource != "yard" || failure.Operation != operation {
				t.Fatal("queued SDK terminal fact differs", operation, failure)
			}
			operation.Status = 0
			if observer.accepted[0] != operation {
				t.Fatal("queued SDK accepted projection differs", observer.accepted, operation)
			}
			operation.ID = ""
			if observer.started[0] != operation {
				t.Fatal("queued SDK pre-effect scope differs", observer.started, operation)
			}
		})
	}
}

// One prepared power action installs once, then two fresh real Serve processes
// bind that exact result and invoke the original prepared start action. Each
// start stops at a distinct SDK terminal400; this proves no whole-program Ready.
func TestOwnerOriginalCallbackContinuesAfterInstallationImport(t *testing.T) {
	if inputPath := originalCallbackChildPath(t); inputPath != "" {
		raw, err := os.ReadFile(inputPath)
		if err != nil {
			t.Fatal(err)
		}
		var input callbackContinuationInput
		if err = json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		err = releaseowner.Serve(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, c releaseowner.Context, fd *os.File) (releaseowner.Components, error) {
			component, err := newReleaseOwnerComponents(ctx, c, fd)
			if err != nil {
				return nil, err
			}
			actual := component.(*releaseOwnerComponents)
			actual.cli.options.Incus = callbackContinuationReader(input)
			actual.cli.options.NetworkPolicy = allowTestNetworkPolicy()
			actual.cli.options.AdapterRunner = ownerGraphRunner{binding: nativeValueDigest(input.Repository), root: input.Repository, socket: input.Socket}
			return &originalCallbackComponent{releaseOwnerComponents: actual, diagnostics: os.Stderr}, nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		os.Exit(0)
	}
	if os.Getenv("SUBYARD_TEST_ORIGINAL_CALLBACK_NS") != "1" {
		// The full graph captures SSH runtime ancestors and owner UIDs. Remapping
		// users would change those original identities, so this owning case uses
		// disposable-VM root in private mount/PID namespaces, preserving UID0.
		if os.Getenv("SUBYARD_ORIGINAL_CALLBACK_REQUIRED") != "1" {
			t.Skip("original production callback continuation requires explicitly selected disposable-VM root")
		}
		if os.Geteuid() != 0 {
			t.Fatal("required original callback continuation needs disposable-VM root; user remapping cannot preserve original ancestor ownership")
		}
		cmd := exec.Command("unshare", "--mount", "--pid", "--fork", "--mount-proc", os.Args[0], "-test.v", "-test.run=^TestOwnerOriginalCallbackContinuesAfterInstallationImport$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SUBYARD_TEST_ORIGINAL_CALLBACK_NS=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal("required original callback root namespace", err, string(output))
		}
		t.Log(string(output))
		return
	}
	if os.Geteuid() != 0 {
		t.Fatal("original callback namespace lost its root identity")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	// Keep the normal engine's actual network-lock installer leaf private.
	if err := unix.Mount("tmpfs", "/run/lock", "tmpfs", 0, "mode=0755"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount("/run/lock", 0)
	stage := os.Getenv("SUBYARD_HOST_INSTALL_RESULT_SOURCE")
	if stage == "" {
		t.Fatal("original callback requires its bound normal stage")
	}
	t.Setenv("SUBYARD_NATIVE_OWNER_GRAPH_ENGINE", filepath.Join(stage, "bin/yard-engine"))
	c, home, toolDir := ownerOriginalGraphFixture(t)
	// Disposable source/config roots stay on /tmp; actual storage needs the
	// leased VM's persistent filesystem and must pass the normal host check.
	originalCallbackStorageFixture(t, c, "/var/tmp")
	repository := c.context.RepositoryRoot
	seedCurrentReleaseLedger(t, repository, c.context.ConfigHome, repositoryRoot(t))
	if err := ownerpublication.RequireReader(repository, c.context.ConfigHome); err != nil {
		t.Fatal("original callback publication reader prerequisite", err)
	}
	// Restore the shipped installer and modern third unit before original capture.
	for _, rel := range []string{"scripts/install-power-reconciler.sh", "config/systemd/subyard-publication-reconcile.service.in"} {
		raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), rel))
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0755
		}
		testkit.WriteFile(t, filepath.Join(repository, rel), raw, mode)
	}
	powerRoot := filepath.Join(c.context.DataHome, "host-power")
	passive := filepath.Join(powerRoot, ownerpublication.HandoffService)
	manager := filepath.Join(toolDir, "systemctl")
	testkit.WriteFile(t, manager, []byte("#!/bin/sh\ncase \"$1\" in\nshow) if [ \"$2\" = subyard-publication-reconcile.service ]; then printf 'LoadState=loaded\\nNeedDaemonReload=no\\nFragmentPath="+passive+"\\nDropInPaths=\\n'; else printf 'LoadState=loaded\\nNeedDaemonReload=no\\n'; fi;;\nis-active) echo inactive; exit 3;;\nesac\n"), 0755)
	if err := unix.Mount(manager, "/usr/bin/systemctl", "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount("/usr/bin/systemctl", 0)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	cfg := map[string]string{"volatile.uuid": "original-legacy-uuid", "user.subyard.desired_power": "running", "user.subyard.managed": "true", "user.subyard.name": "e2e-yard", "user.subyard.bridge": "incusbr0", "user.subyard.initialized": "true", "boot.autostart": "false"}
	instance := ports.InstanceInfo{Project: "subyard-e2e-yard", Name: "yard-e2e-yard", Type: domain.YardContainer, Status: "Stopped", Config: maps.Clone(cfg), LocalConfig: maps.Clone(cfg)}
	server.SetInstance(instance.Project, instance.Name, map[string]any{"name": instance.Name, "type": "container", "status": "Stopped", "config": cfg, "expanded_config": maps.Clone(cfg)})
	server.QueuePowerErrors("original callback first start terminal400", "original callback retry start terminal400")
	socket := ownerPublicationSnapshotSocket(t, server)
	input := callbackContinuationInput{repository, socket, instance}
	c.cli.options.Incus = callbackContinuationReader(input)
	c.cli.options.AdapterRunner = ownerGraphRunner{binding: nativeValueDigest(repository), root: repository, socket: socket}
	for _, env := range []map[string]string{c.cli.env, c.cli.baseEnv} {
		env["SUBYARD_USER"] = "root"
		env["SUBYARD_INCUS_SOCKET"] = socket
	}
	if _, _, err = c.CaptureInventory(context.Background()); err != nil {
		t.Fatal("capture inventory", err)
	}
	port := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
	observed, err := port.Prepare(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	before := releaseowner.OwnerBefore{State: string(observed.State), Registration: string(observed.Registration), Overrides: string(observed.Overrides), Controller: string(observed.Controller), SharedImages: observed.SharedImages}
	diagnostics := &originalCallbackDiagnostics{}
	previousStderr := c.cli.options.Stderr
	c.cli.options.Stderr = diagnostics
	original, err := c.prepareOwnerChildren(context.Background(), before)
	c.cli.options.Stderr = previousStderr
	if err != nil {
		t.Logf("original callback preparation diagnostics (truncated=%t):\n%s", diagnostics.truncated, diagnostics.body)
		t.Fatal("capture original callback", err)
	}
	defer original.Close()
	before.PreparedRecipe = original.binding()
	record, err := original.record()
	if err != nil {
		t.Fatal(err)
	}
	capture, err := record.capture()
	if err != nil {
		t.Fatal(err)
	}
	before.Capture = &capture
	raw, _ := json.Marshal(input)
	inputPath := filepath.Join(home, "callback-input.json")
	testkit.WriteFile(t, inputPath, raw, 0600)
	facts := filepath.Join(home, "migrations/native-custody/v1/callback-transaction")
	if err = os.MkdirAll(facts, 0700); err != nil {
		t.Fatal(err)
	}
	updatePath := filepath.Join(home, "migrations/update.lock")
	testkit.WriteFile(t, updatePath, nil, 0600)
	update, err := os.OpenFile(updatePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer update.Close()
	if err = unix.Flock(int(update.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	_, unlock, err := uninstall.OwnerLock(context.Background(), c.context.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	owner, err := uninstall.BorrowOwnerLock(c.context.DataHome)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	defer owner.Close()
	repoFD, err := os.Open(repository)
	if err != nil {
		t.Fatal(err)
	}
	defer repoFD.Close()
	value := c.context
	value.Root, err = releaseowner.IdentityOf(c.root)
	if err != nil {
		t.Fatal(err)
	}
	value.Repository, err = releaseowner.IdentityOf(repoFD)
	if err != nil {
		t.Fatal(err)
	}
	updateID, _ := releaseowner.IdentityOf(update)
	ownerID, _ := releaseowner.IdentityOf(owner)
	ownerRaw, _ := json.Marshal(ownerID)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Repeat("a", 64)
	token := strings.Repeat("2", 32)
	binding := releaseowner.Binding{Session: value.Session, Transaction: "callback-transaction", Plan: "plan-v1-" + a, Journal: a, Artifact: value.Artifact, Helper: nativeDigest(image), Ordinal: 1}
	producer := binding
	power := releaseowner.OwnerActionReference{Group: producer, Program: nativeValueDigest("original callback power and start"), Index: 0, Action: releaseowner.OwnerAction{Operation: "power", Slot: 1}}
	start := power
	start.Index, start.Action = 1, releaseowner.OwnerAction{Operation: "start", Slot: 1}
	var firstResult ports.NativeHostPowerInstallation
	var operations []string
	for ordinal := uint64(1); ordinal <= 3; ordinal++ {
		binding.Ordinal = ordinal
		action := start
		if ordinal == 1 {
			action = power
		}
		recipeRaw, _ := json.Marshal(struct {
			Before *releaseowner.OwnerBefore          `json:"before"`
			Token  string                             `json:"token"`
			Action *releaseowner.OwnerActionReference `json:"action"`
		}{&before, token, &action})
		reservation := struct {
			Version int                  `json:"version"`
			Binding releaseowner.Binding `json:"binding"`
			Files   int                  `json:"files"`
			Bytes   int64                `json:"bytes"`
		}{1, binding, 2570, 16 << 20}
		write := func(name string, v any) {
			t.Helper()
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(facts, fmt.Sprintf("%03d.%s.json", ordinal, name)), append(raw, '\n'), 0600)
		}
		write("reservation", reservation)
		write("intent", releaseowner.Intent{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", Expected: a, Desired: a, Source: a, Recipe: nativeDigest(recipeRaw)})
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err = unix.SetNonblock(pair[0], true); err != nil {
			t.Fatal(err)
		}
		peer, child := os.NewFile(uintptr(pair[0]), "callback original parent"), os.NewFile(uintptr(pair[1]), "callback original helper")
		cmd := exec.Command(executable, "-test.run=^TestOwnerOriginalCallbackContinuesAfterInstallationImport$")
		cmd.Env = append(environmentList(c.cli.env, nil), originalCallbackChildKey+"="+inputPath)
		cmd.Stdin, cmd.Stdout = child, child
		diagnosticPath := filepath.Join(home, fmt.Sprintf("callback-%d.stderr", ordinal))
		testkit.WriteFile(t, diagnosticPath, nil, 0600)
		diagnosticFile, err := os.OpenFile(diagnosticPath, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer diagnosticFile.Close()
		diagnostic := func() string { raw, _ := os.ReadFile(diagnosticPath); return string(raw) }
		cmd.Stderr = diagnosticFile
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		child.Close()
		defer func() {
			peer.Close()
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			if t.Failed() {
				t.Log(diagnostic())
			}
		}()
		// Same fixed framing as framedOriginalCLIRecipes, kept local to this case.
		var sequence uint64
		exchange := func(request releaseowner.Request, files ...*os.File) releaseowner.Reply {
			t.Helper()
			if err := peer.SetDeadline(time.Now().Add(45 * time.Second)); err != nil {
				t.Fatal("callback peer deadline", err)
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			sequence++
			header := make([]byte, 52)
			copy(header, "SYC1")
			header[4], header[5] = 1, 15
			binary.BigEndian.PutUint64(header[8:16], sequence)
			sum := sha256.Sum256(raw)
			copy(header[16:48], sum[:])
			binary.BigEndian.PutUint32(header[48:], uint32(len(raw)))
			if len(files) > 0 {
				fds := []int{}
				for _, file := range files {
					fds = append(fds, int(file.Fd()))
				}
				if err = unix.Sendmsg(int(peer.Fd()), header[:1], unix.UnixRights(fds...), nil, unix.MSG_NOSIGNAL); err != nil {
					t.Fatal(err)
				}
				header = header[1:]
			}
			if _, err = peer.Write(append(header, raw...)); err != nil {
				t.Fatal(err)
			}
			reply := make([]byte, 52)
			if _, err = io.ReadFull(peer, reply); err != nil {
				t.Fatal("callback frame", err, diagnostic())
			}
			if !bytes.Equal(reply[:8], []byte{'S', 'Y', 'C', '1', 1, 15, 0, 0}) || binary.BigEndian.Uint64(reply[8:16]) != sequence || !bytes.Equal(reply[16:48], sum[:]) {
				t.Fatal("callback frame binding")
			}
			size := binary.BigEndian.Uint32(reply[48:])
			if size > releaseowner.MaxReply {
				t.Fatal("callback reply bound")
			}
			body := make([]byte, size)
			if _, err = io.ReadFull(peer, body); err != nil {
				t.Fatal(err)
			}
			var result releaseowner.Reply
			if err = json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
			return result
		}
		require := func(request releaseowner.Request, files ...*os.File) {
			t.Helper()
			reply := exchange(request, files...)
			if reply.Code != "" {
				t.Fatal("callback setup", request.Op, reply, diagnostic())
			}
		}
		require(releaseowner.Request{Op: "initialize", Context: &value}, c.root, repoFD)
		require(releaseowner.Request{Op: "capture-inventory", Session: value.Session})
		for offset := 0; offset < len(original.raw); {
			end := min(offset+(512<<10), len(original.raw))
			require(releaseowner.Request{Op: "bind-installed-recipe", Session: value.Session, RecipeHash: before.PreparedRecipe, Offset: uint64(offset), Total: uint64(len(original.raw)), Content: base64.StdEncoding.EncodeToString(original.raw[offset:end])})
			offset = end
		}
		require(releaseowner.Request{Op: "claim-lease", Binding: &binding, Update: &updateID, Owner: ownerRaw}, update, owner)
		if ordinal > 1 {
			require(releaseowner.Request{Op: "bind-owner-results", Binding: &binding, Producer: &producer, Before: &before, Token: token, Action: &action, ProducerAction: &power})
		}
		reply := exchange(releaseowner.Request{Op: "apply-owner-action", Binding: &binding, Before: &before, Token: token, Action: &action})
		peer.Close()
		if err = cmd.Wait(); err != nil {
			t.Fatal("original callback helper drain", err, diagnostic())
		}
		if ordinal == 1 {
			if reply.Code != "" || reply.Terminal == nil || reply.Terminal.State != "complete" || len(reply.Terminal.Operations) != 0 {
				t.Fatal("original power action did not close", reply.Code, diagnostic())
			}
			var fact struct {
				Result ports.NativeHostPowerInstallation `json:"result"`
			}
			raw, err = os.ReadFile(filepath.Join(facts, "001.host-power-installation.json"))
			if err != nil || json.Unmarshal(raw, &fact) != nil {
				t.Fatal("original installation result", err)
			}
			firstResult = fact.Result
			continue
		}
		if reply.Code != "failed" || reply.Terminal == nil || reply.Terminal.State != "failed" {
			t.Log(originalCallbackFactDiagnostics(facts, ordinal))
			t.Fatal("original callback must close WHOLEFAILED", ordinal, reply, diagnostic())
		}
		opRaw, err := os.ReadFile(filepath.Join(facts, fmt.Sprintf("%03d.operation-001-terminal.json", ordinal)))
		if err != nil {
			t.Fatal(err)
		}
		var operation ports.NativeOperation
		if err = json.Unmarshal(opRaw, &operation); err != nil {
			t.Fatal(err)
		}
		if operation.Status != 400 || operation.ID == "" || operation.Kind != "instance-power" || operation.Project != instance.Project || operation.Resource != instance.Name {
			t.Fatal("original SDK terminal fact", operation)
		}
		acceptedRaw, err := os.ReadFile(filepath.Join(facts, fmt.Sprintf("%03d.operation-001-accepted.json", ordinal)))
		if err != nil {
			t.Fatal(err)
		}
		var accepted ports.NativeOperation
		if err = json.Unmarshal(acceptedRaw, &accepted); err != nil {
			t.Fatal(err)
		}
		projection := operation
		projection.Status = 0
		if accepted != projection {
			t.Fatal("SDK terminal lost its durable accepted operation", accepted, operation)
		}
		if len(reply.Terminal.Operations) != 1 || reply.Terminal.Operations[0] != operation {
			t.Fatal("original callback added a later mutation", reply.Terminal.Operations, operation)
		}
		operations = append(operations, operation.ID)
		if _, err = os.Stat(filepath.Join(facts, fmt.Sprintf("%03d.host-power-installation.json", ordinal))); !os.IsNotExist(err) {
			t.Fatal("start reinstalled instead of importing", err)
		}
	}
	if operations[0] == operations[1] || len(server.PowerCalls()) != 2 {
		t.Fatal("distinct original SDK operations missing", operations, server.PowerCalls())
	}
	for _, call := range server.PowerCalls() {
		if call.Action != "start" || call.Force || call.Project != instance.Project || call.Name != instance.Name {
			t.Fatal("callback escaped exact original start", call)
		}
	}
	for _, p := range []string{"001.command-001-terminal.json", "001.host-power-installation.json", "001.terminal.json", "002.terminal.json", "003.terminal.json"} {
		if _, err = os.Stat(filepath.Join(facts, p)); err != nil {
			t.Fatal("durable closure missing", p, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(facts, "*.command-*-intent.json"))
	var installerCommands []string
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fact struct {
			Kind string `json:"kind"`
		}
		if err = json.Unmarshal(raw, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == "owner-host-power-install" {
			installerCommands = append(installerCommands, path)
		}
	}
	if len(installerCommands) != 1 || !strings.Contains(filepath.Base(installerCommands[0]), "001.command-") {
		t.Fatal("installer command repeated or lost", installerCommands)
	}

	if len(firstResult.Identities) != 3 || len(firstResult.Consumer.Generations) != 3 {
		t.Fatal("original three-resource installation result missing", firstResult)
	}
	for _, id := range firstResult.Identities {
		path := map[string]string{"engine": filepath.Join(powerRoot, "engine"), "service": filepath.Join(powerRoot, "unit.service"), "publication": passive}[id.Role]
		var st unix.Stat_t
		if err = unix.Stat(path, &st); err != nil || uint64(st.Dev) != id.Device || st.Ino != id.Inode {
			t.Fatal("protected consumer identity replaced", id, err)
		}
		custody := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".native-custody.json")
		raw, err := os.ReadFile(custody)
		if err != nil {
			t.Fatal(err)
		}
		var generation struct {
			Version    int    `json:"version"`
			Owner      string `json:"owner"`
			Generation string `json:"generation"`
		}
		if err = json.Unmarshal(raw, &generation); err != nil {
			t.Fatal(err)
		}
		var expected string
		for _, value := range firstResult.Consumer.Generations {
			if value.Role == id.Role {
				expected = value.Value
			}
		}
		if generation.Version != 1 || generation.Owner != "host-power" || generation.Generation != expected || expected == "" {
			t.Fatal("protected consumer generation changed", id.Role, generation)
		}

	}
	if !reflect.DeepEqual(firstResult.Consumer.Generations, []ports.NativeResourceGeneration{{Role: "engine", Value: firstResult.Consumer.Generations[0].Value}, {Role: "service", Value: firstResult.Consumer.Generations[0].Value}, {Role: "publication", Value: firstResult.Consumer.Generations[0].Value}}) {
		t.Fatal("consumer generation vector changed")
	}
	final, err := incusclient.New(input.Socket).Instance(context.Background(), instance.Project, instance.Name)
	if err != nil || final.LocalConfig["volatile.uuid"] != instance.LocalConfig["volatile.uuid"] || final.Status != "Stopped" || final.LocalConfig["user.subyard.desired_power"] != "running" {
		t.Fatal("original stopped/desired-running UUID changed", final, err)
	}
	t.Logf("prepared power complete; two fresh original start actions FAILED; one installer terminal; distinct SDK400 IDs %v; same recipe %s", operations, before.PreparedRecipe)
}
