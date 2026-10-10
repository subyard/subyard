package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestNetworkHelpDoesNotRequireAnInitializedYard(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	engine, err := New(Options{
		RepositoryRoot: root,
		Arguments:      []string{"network", "--help"},
		Environment: []string{
			"HOME=" + home,
			"SUBYARD_CONFIG_HOME=" + filepath.Join(home, "config"),
			"SUBYARD_HOME=" + filepath.Join(home, "data"),
		},
		Incus: &testkit.Incus{}, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := engine.Run(context.Background()); code != 0 {
		t.Fatalf("network help returned %d: %s", code, stderr.String())
	}
	for _, command := range []string{"status", "link", "unlink", "isolation", "reconcile"} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("network help omitted %q: %s", command, stdout.String())
		}
	}
}

type networkCLIHost struct {
	yardnetwork.Host
	policy      yardnetwork.Policy
	reads       int
	writes      int
	ready       bool
	allowWrites bool
	instances   map[string]ports.InstanceInfo
}

func (h *networkCLIHost) ReadPolicy(context.Context) (yardnetwork.StoredPolicy, error) {
	h.reads++
	return yardnetwork.StoredPolicy{Policy: h.policy, ETag: "one"}, nil
}
func (h *networkCLIHost) InspectNetwork(_ context.Context, yards []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	s := yardnetwork.Snapshot{}
	if h.ready {
		s.Firewall = "nftables"
		s.Networks = map[string]yardnetwork.Network{}
	}
	for _, y := range yards {
		observed := yardnetwork.ObservedYard{Yard: y}
		observed.InstanceInfo, observed.InstanceFound = h.instances[y.Name]
		if h.ready {
			observed.ProjectFound = true
			observed.ProfileFound = true
			observed.ProjectConfig = map[string]string{"restricted": "true"}
			observed.ProfileDevices = map[string]map[string]string{"eth0": {"type": "nic", "network": y.Network, "name": "eth0"}}
			s.Networks[y.Network] = yardnetwork.Network{Name: y.Network, Type: "bridge", Managed: true, Config: map[string]string{"ipv4.address": "10.80.0.1/24"}}
		}
		s.Yards = append(s.Yards, observed)
	}
	return s, nil
}

func TestNetworkRestartRequiresTypedConsentBeforeMutation(t *testing.T) {
	for _, args := range [][]string{{"isolation", "on"}, {"reconcile"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			p, _ := yardnetwork.Decode(nil)
			host := &networkCLIHost{policy: p, ready: true}
			if args[0] == "reconcile" {
				host.policy.PendingStart = []string{"default"}
				host.instances = map[string]ports.InstanceInfo{"default": {Type: domain.YardVM, Status: "Stopped"}}
			}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, Incus: &testkit.Incus{}, NetworkPolicy: &yardnetwork.Service{Host: host}})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition := command.Definition{Name: "network", Handler: "@network", Remote: command.RemoteLocal, Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if prepared.Plan.Assessment == nil || prepared.Plan.Assessment.Action != "yard.network.apply" || !prepared.Plan.Assessment.Changed || prepared.Plan.Confirmed {
				t.Fatalf("restart did not require confirmation: action=%v confirmed=%t", prepared.Plan.Assessment, prepared.Plan.Confirmed)
			}
			if _, err = prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); !errors.Is(err, domain.ErrConfirmationRequired) {
				t.Fatalf("execution without consent: %v", err)
			}
			if host.writes != 0 {
				t.Fatal("unconfirmed isolation wrote policy")
			}
		})
	}
}
func (h *networkCLIHost) WritePolicy(_ context.Context, _ yardnetwork.StoredPolicy, policy yardnetwork.Policy) (yardnetwork.StoredPolicy, error) {
	h.writes++
	if h.allowWrites {
		h.policy = policy
		content, _ := yardnetwork.Encode(policy)
		return yardnetwork.StoredPolicy{Policy: policy, Content: string(content), ETag: "one"}, nil
	}
	return yardnetwork.StoredPolicy{}, errors.New("unexpected mutation during assessment")
}

func TestNetworkCPUAuthorizationPrecedesMutation(t *testing.T) {
	commands := testkit.TempDir(t)
	writeCLIFile(t, filepath.Join(commands, "sudo"), "#!/bin/sh\nexit 1\n", 0o700)
	t.Setenv("PATH", commands)
	for _, tc := range []struct {
		name       string
		kind       domain.YardKind
		status     string
		weight     string
		isolation  bool
		pending    bool
		wantError  string
		wantWrites int
	}{
		{"running weighted VM", domain.YardVM, "Running", "1000", true, false, "sudo authorization is required", 0},
		{"pending stopped weighted VM", domain.YardVM, "Stopped", "1000", false, true, "sudo authorization is required", 0},
		{"stopped weighted VM", domain.YardVM, "Stopped", "1000", true, false, "unexpected mutation during assessment", 1},
		{"unweighted VM", domain.YardVM, "Running", "", true, false, "unexpected mutation during assessment", 1},
		{"container", domain.YardContainer, "Running", "", true, false, "unexpected mutation during assessment", 1},
		{"already running pending VM", domain.YardVM, "Running", "1000", false, true, "unexpected mutation during assessment", 1},
		{"no-op weighted VM", domain.YardVM, "Running", "1000", false, false, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			p, _ := yardnetwork.Decode(nil)
			if tc.pending {
				p.PendingStart = []string{"default"}
			}
			host := &networkCLIHost{policy: p, ready: true,
				instances: map[string]ports.InstanceInfo{"default": {Type: tc.kind, Status: tc.status,
					Config: map[string]string{"user.subyard.vm_cpu_weight": tc.weight}}}}
			program, err := New(Options{RepositoryRoot: root, WorkingDir: root,
				Environment: append(environment, "PATH="+commands), Incus: &testkit.Incus{},
				NetworkPolicy: &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{},
					Guard: func(context.Context) error { return nil }}})
			if err != nil {
				t.Fatal(err)
			}
			program.effectiveUID = func() int { return 1000 }
			program.operatorTerminal = func() bool { return false }
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"reconcile"}
			if tc.isolation {
				args = []string{"isolation", "on"}
			}
			definition := command.Definition{Name: "network", Handler: "@network", Remote: command.RemoteLocal,
				Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
			prepared, err := program.prepareCommand(context.Background(),
				prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, loaded, nil, &definition)
			prepared.Plan, err = orchestrator.Confirm(context.Background(), prepared.Plan, true)
			if err != nil {
				t.Fatal(err)
			}
			_, err = prepared.Execute(context.Background(), orchestrator, io.Discard)
			if (err != nil) != (tc.wantError != "") ||
				(err != nil && !strings.Contains(err.Error(), tc.wantError)) || host.writes != tc.wantWrites {
				t.Fatalf("network authorization: err=%v writes=%d; want %q/%d", err, host.writes, tc.wantError, tc.wantWrites)
			}
		})
	}
}

func TestNetworkLinkExecutesThroughAuthorizedOperation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "state", "yards"), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "state", "yards", "beta.env"), "SSH_PORT=2233\n", 0600)
	manifestPath := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, manifestPath, string(manifest)+"network||@network||local|mutate|dynamic|public|lifecycle|simple|network <command>|network|--json --yes --help|status link unlink isolation reconcile\n", 0600)
	p, _ := yardnetwork.Decode(nil)
	host := &networkCLIHost{policy: p, allowWrites: true}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"network", "link", "default", "beta"}, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: &testkit.Incus{}, NetworkPolicy: &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{}}})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("link exit=%d stderr=%s", code, stderr.String())
	}
	if host.writes != 2 || len(host.policy.Peers("default")) != 1 || host.policy.AppliedRevision != host.policy.Revision {
		t.Fatalf("link not applied: writes=%d policy=%+v", host.writes, host.policy)
	}
}

func TestNetworkPreparationIsReadOnlyAndRejectsForeignHost(t *testing.T) {
	for _, args := range [][]string{{"link", "default", "beta"}, {"link", "remote-host/default", "beta"}, {"status", "--json"}, {"isolation", "sometimes"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			if err := os.MkdirAll(filepath.Join(root, "state", "yards"), 0700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(root, "state", "yards", "beta.env"), "SSH_PORT=2233\n", 0600)
			p, _ := yardnetwork.Decode(nil)
			host := &networkCLIHost{policy: p}
			var stdout bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, Stdout: &stdout, NetworkPolicy: &yardnetwork.Service{Host: host}, Incus: &testkit.Incus{}})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition := command.Definition{Name: "network", Handler: "@network", Remote: command.RemoteLocal, Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: args})
			invalid := args[0] == "isolation" || strings.Contains(args[1], "/")
			if invalid {
				if err == nil {
					prepared.Close()
					t.Fatal("invalid network action accepted")
				}
				if code := program.reportPreparationError(definition, err); code != 2 {
					t.Fatalf("invalid arguments returned %d", code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if host.writes != 0 {
				t.Fatal("assessment wrote network policy")
			}
			if args[0] == "status" {
				if prepared.displayOnly == nil {
					t.Fatal("status needs read-only display")
				}
				prepared.displayOnly()
				var result struct {
					Converged bool `json:"converged"`
				}
				if err = json.Unmarshal(stdout.Bytes(), &result); err != nil || !result.Converged {
					t.Fatalf("status=%s err=%v", stdout.String(), err)
				}
				return
			}
			if prepared.Plan.Assessment == nil || prepared.Plan.Assessment.Action != domain.ActionID("yard.network.save") || !prepared.Plan.Assessment.Changed {
				t.Fatalf("unexpected graph assessment: %+v", prepared.Plan.Assessment)
			}
		})
	}
}
