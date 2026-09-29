package cli

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProvisionEndpointSelection(t *testing.T) {
	public := hostruntime.OwnerIPv4{Interface: "eth0", Address: netip.MustParseAddr("8.8.8.8")}
	other := hostruntime.OwnerIPv4{Interface: "eth1", Address: netip.MustParseAddr("1.1.1.1")}
	private := hostruntime.OwnerIPv4{Interface: "eth2", Address: netip.MustParseAddr("10.0.0.2")}
	for _, test := range []struct {
		name           string
		addresses      []hostruntime.OwnerIPv4
		address, iface string
		want           hostruntime.OwnerIPv4
		ok             bool
	}{
		{"unique public", []hostruntime.OwnerIPv4{private, public}, "", "", public, true},
		{"duplicate observation", []hostruntime.OwnerIPv4{public, public}, "", "", public, true},
		{"ambiguous", []hostruntime.OwnerIPv4{public, other}, "", "", hostruntime.OwnerIPv4{}, false},
		{"NAT", []hostruntime.OwnerIPv4{private}, "", "", hostruntime.OwnerIPv4{}, false},
		{"explicit interface", []hostruntime.OwnerIPv4{public, other}, "", "eth1", other, true},
		{"explicit private test address", []hostruntime.OwnerIPv4{private, public}, "10.0.0.2", "", private, true},
		{"address absent", []hostruntime.OwnerIPv4{private}, "8.8.8.8", "", hostruntime.OwnerIPv4{}, false},
		{"same address on two interfaces", []hostruntime.OwnerIPv4{public, {Interface: "eth1", Address: public.Address}}, "", "", hostruntime.OwnerIPv4{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := selectProvisionEndpoint(test.addresses, test.address, test.iface)
			if ok != test.ok || got != test.want {
				t.Fatalf("got=%+v ok=%v", got, ok)
			}
		})
	}
	for _, address := range []string{"127.0.0.1", "169.254.1.2", "100.64.0.1", "192.168.1.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "0.1.2.3", "::1", "2001:4860:4860::8888"} {
		if hostruntime.PublicEndpointIPv4(netip.MustParseAddr(address)) {
			t.Errorf("auto-selected special address %s", address)
		}
	}
}

func provisionEndpointFixture(t *testing.T) (*CLI, config.Loaded, string, *testkit.ScriptedAdapter) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample")
	yardFile := filepath.Join(root, "state", "yards", "vpn", "config.env")
	if err := os.MkdirAll(filepath.Dir(yardFile), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, yardFile, "YARD_KIND=vm\nENVIRONMENT_PROFILES=sample\nRESOURCE_RELAY_PORT=42000\n", 0o600)
	directory := filepath.Join(root, "config", "profiles", "sample", "resources")
	if err := os.MkdirAll(filepath.Join(directory, "relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(directory, "relay.res"), `COMMAND=relay
HANDLER=resources/relay/handler.sh
TITLE="Sample relay"
PROXY="sample-relay RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT RESOURCE_RELAY_INTERFACE udp:guest:41999 owner-metadata-v1 owner-ipv4-udp"
ACTION="up up public-ingress-change reversible"
ACTION="down down public-ingress-change reversible"
BRINGUP=up
SHUTDOWN=down
`, 0o600)
	writeCLIFile(t, filepath.Join(directory, "relay", "handler.sh"), "#!/bin/sh\nexit 70\n", 0o700)
	incus := lifecycleIncus()
	runner := &testkit.ScriptedAdapter{}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, Incus: incus, AdapterRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("vpn")
	if err != nil {
		t.Fatal(err)
	}
	instance := incus.Instances["subyard/yard"]
	instance.Name, instance.Project, instance.Type, instance.Status = loaded.Context.YardInstanceName, loaded.Context.IncusProject, domain.YardVM, "Running"
	instance.Config["user.subyard.name"] = "vpn"
	instance.Config["user.subyard.desired_power"] = "running"
	incus.Instances[instance.Project+"/"+instance.Name] = instance
	program.provisionEndpointAddresses = func() ([]hostruntime.OwnerIPv4, error) {
		return []hostruntime.OwnerIPv4{{Interface: "eth0", Address: netip.MustParseAddr("8.8.8.8")}}, nil
	}
	return program, loaded, yardFile, runner
}

func TestProvisionEndpointRPCConsentAndStaleChecks(t *testing.T) {
	for _, scenario := range []string{"apply", "decline", "provision failed", "address changed", "config changed", "ingress appeared", "source appeared"} {
		t.Run(scenario, func(t *testing.T) {
			program, loaded, path, runner := provisionEndpointFixture(t)
			program.options.InitPlatform = convergedProvisionInit(t, program.options.RepositoryRoot)
			addresses, _ := program.provisionEndpointAddresses()
			program.provisionEndpointAddresses = func() ([]hostruntime.OwnerIPv4, error) { return addresses, nil }
			before, _ := os.ReadFile(path)
			for range 4 {
				runner.Steps = append(runner.Steps, testkit.AdapterStep{Result: domain.AdapterResult{Schema: 1, OperationID: "endpoint", Status: "ok"}, Stderr: "converged\n"})
			}
			if scenario == "provision failed" {
				for index := range 3 {
					runner.Steps[index].Stderr = "changed\n"
				}
				runner.Steps[3].Result.Status = "error"
			}
			handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
			emit := func(string, any) (uint64, error) { return 1, nil }
			if _, err := handler.Handle(context.Background(), rpc.Call{ID: "plan", OperationID: "endpoint", Method: "operation.plan", Params: json.RawMessage(`{"command":"provision","arguments":["sample"]}`)}, emit); err != nil {
				t.Fatal(err)
			}
			observed, _ := os.ReadFile(path)
			if string(observed) != string(before) {
				t.Fatal("assessment wrote configuration")
			}
			for _, prepared := range handler.plans {
				if prepared.Plan.Assessment == nil || !prepared.Plan.Assessment.Changed || !strings.Contains(strings.Join(prepared.Plan.Assessment.Consequences, "\n"), "RESOURCE_RELAY_IPV4=8.8.8.8") {
					t.Fatalf("missing endpoint assessment: %+v", prepared.Plan.Assessment)
				}
			}
			switch scenario {
			case "decline":
				_, err := handler.Handle(context.Background(), rpc.Call{ID: "execute", OperationID: "endpoint", Method: "operation.execute", Params: json.RawMessage(`{"confirmed":false}`)}, emit)
				if err == nil {
					t.Fatal("unconfirmed endpoint plan executed")
				}
				got, _ := os.ReadFile(path)
				if string(got) != string(before) || len(runner.Requests) != 1 {
					t.Fatal("declined plan mutated configuration or provisioned")
				}
				return
			case "address changed":
				addresses = nil
			case "config changed":
				writeCLIFile(t, path, string(before)+"RESOURCE_RELAY_INTERFACE=eth1\n", 0o600)
			case "ingress appeared":
				incus := program.options.Incus.(*testkit.Incus)
				instance := incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]
				instance.Config["user.subyard.resource.sample-relay"] = "foreign"
			case "source appeared":
				if err := os.MkdirAll(filepath.Join(loaded.Context.Paths.ConfigHome, ".sync"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, filepath.Join(loaded.Context.Paths.ConfigHome, config.SourceRecordRelativePath), "{}\n", 0o600)
			}
			expected, _ := os.ReadFile(path)
			_, err := handler.Handle(context.Background(), rpc.Call{ID: "execute", OperationID: "endpoint", Method: "operation.execute", Params: json.RawMessage(`{"confirmed":true}`)}, emit)
			if scenario == "apply" {
				if err != nil {
					t.Fatal(err)
				}
				fresh, err := program.loadContext("vpn")
				if err != nil {
					t.Fatal(err)
				}
				if fresh.Environment["RESOURCE_RELAY_IPV4"] != "8.8.8.8" || fresh.Environment["RESOURCE_RELAY_INTERFACE"] != "eth0" || fresh.Environment["RESOURCE_RELAY_PORT"] != "42000" {
					t.Fatal("endpoint configuration was not preserved/published")
				}
				plan, _, err := program.prepareProvisionEndpoint(fresh, []string{"sample"}, func() ([]hostruntime.OwnerIPv4, error) { t.Fatal("rediscovered explicit endpoint"); return nil, nil })
				if err != nil || plan != nil {
					t.Fatalf("repeat changed endpoint: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("stale endpoint plan applied")
				}
				got, _ := os.ReadFile(path)
				if string(got) != string(expected) {
					t.Fatal("stale plan changed configuration")
				}
			}
			for _, request := range runner.Requests {
				if request.Action != "profile-check" && !(scenario == "provision failed" && request.Action == "profile") {
					t.Fatalf("endpoint provisioning unexpectedly mutated guest: %+v", request)
				}
			}
		})
	}
}

func TestProvisionEndpointAmbiguousAndUnselectedRemainUnchanged(t *testing.T) {
	program, loaded, _, _ := provisionEndpointFixture(t)
	for _, profiles := range [][]string{{"sample"}, {"other"}} {
		plan, notes, err := program.prepareProvisionEndpoint(loaded, profiles, func() ([]hostruntime.OwnerIPv4, error) { return nil, nil })
		if err != nil || plan != nil {
			t.Fatalf("unexpected plan: %+v, %v", plan, err)
		}
		if (len(notes) == 1) != (profiles[0] == "sample") {
			t.Fatalf("notes=%v", notes)
		}
	}
}
