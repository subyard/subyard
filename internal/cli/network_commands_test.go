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

func TestNetworkIsolationRequiresTypedConsentBeforeMutation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	p, _ := yardnetwork.Decode(nil)
	host := &networkCLIHost{policy: p, ready: true}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, Incus: &testkit.Incus{}, NetworkPolicy: &yardnetwork.Service{Host: host}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition := command.Definition{Name: "network", Handler: "@network", Remote: command.RemoteLocal, Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{"isolation", "on"}})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Plan.Assessment == nil || prepared.Plan.Assessment.Action != "yard.network.apply" || !prepared.Plan.Assessment.Changed || prepared.Plan.Confirmed {
		t.Fatalf("isolation did not require confirmation: %+v", prepared.Plan)
	}
	if _, err = prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); !errors.Is(err, domain.ErrConfirmationRequired) {
		t.Fatalf("execution without consent: %v", err)
	}
	if host.writes != 0 {
		t.Fatal("unconfirmed isolation wrote policy")
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
