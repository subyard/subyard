package cli

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestStartCompletesPendingDiskTmpAndReportsFailures(t *testing.T) {
	for _, startup := range []bool{false, true} {
		for _, outcome := range []string{"success", "marked", "running-failure", "stopped-failure"} {
			t.Run(fmt.Sprintf("startup=%t/%s", startup, outcome), func(t *testing.T) {
				program, loaded, _ := orphanIngressFixture(t, true)
				root := program.options.RepositoryRoot
				res := filepath.Join(root, "config/profiles/sample/resources/relay.res")
				body, err := os.ReadFile(res)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, res, append(body, []byte("\nSTARTUP=bringup\n")...), 0600)
				if err := os.MkdirAll(filepath.Join(root, "config/yards/profiles"), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "config/yards/profiles/dedicated.env"), []byte("EXCLUSIVE_ENVIRONMENT_PROFILE=sample\nREQUIRED_YARD_KIND=vm\nYARD_KIND=vm\n"), 0600)
				settings := filepath.Join(root, "state/yards/private/config.env")
				testkit.WriteFile(t, settings, []byte("YARD_TEMPLATE=dedicated\nENVIRONMENT_PROFILES=sample\nRESOURCE_RELAY_IPV4=10.20.30.40\nRESOURCE_RELAY_PORT=42000\nRESOURCE_RELAY_INTERFACE=fixture0\n"), 0600)
				program, err = New(program.options)
				if err != nil {
					t.Fatal(err)
				}
				loaded, err = program.loadContext("private")
				if err != nil {
					t.Fatal(err)
				}
				definition := program.resources.Definitions()[0]
				calls := filepath.Join(root, "startup-calls")
				testkit.WriteFile(t, definition.HandlerPath(), []byte(fmt.Sprintf(`#!/bin/sh
case "$SUBYARD_RESOURCE_MODE" in
 prepare|prepare-start) printf '%%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"up","changed":true,"consequences":["activate sample relay"]}' ;;
 *) printf '%%s\n' "$1" >> %q ;;
esac
`, calls)), 0700)
				contract := *definition.Proxy
				device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
				intent := startupDisabled
				if startup {
					intent = startupPending
				}
				values := map[string]string{"volatile.uuid": "same-instance", "user.subyard.desired_power": "running", "user.subyard.managed": "true", "user.subyard.initialized": "true", contract.OwnershipKey(): contract.OwnershipValue(device), startupIntentKey(definition): intent}
				if outcome == "marked" {
					values["user.subyard.disk_tmp"], values["user.subyard.preview_sha256"] = "v1", "old-runtime"
				}
				instance := ports.InstanceInfo{Project: loaded.Context.IncusProject, Name: loaded.Context.YardInstanceName, Status: "Stopped", Type: domain.YardVM, Config: maps.Clone(values), LocalConfig: maps.Clone(values), LocalDevices: map[string]map[string]string{contract.Device: maps.Clone(device)}, Devices: map[string]map[string]string{contract.Device: maps.Clone(device), "eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}}
				status := filepath.Join(root, "power")
				testkit.WriteFile(t, status, []byte("Stopped"), 0600)
				native := &stoppedActivationNative{adoptionIncus: &adoptionIncus{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}, Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}}}, statusPath: status}
				host := &rollbackIngressHost{snapshot: yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{Yard: networkYard(loaded.Context), InstanceFound: true, InstanceInfo: instance}}}}
				service := &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{}, ClearStaleUDP: func(context.Context, netip.AddrPort) error { return nil }}
				program.options.Incus = native
				program.options.Executor = native
				program.options.NetworkPolicy = service
				program.options.AdapterRunner = nil
				program.effectiveUID = func() int { return 0 }
				program.options.InitPlatform = reconcileruntime.Runtime{RepositoryRoot: root, Incus: native, Executor: native, Yard: loaded.Context, NetworkPolicy: legacyContinuationNetworkFixture{}}
				testkit.WriteFile(t, filepath.Join(root, "scripts/06-network.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
				if outcome != "running-failure" {
					testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), []byte("policy"), 0600)
				}
				startExit := 0
				if outcome == "stopped-failure" {
					startExit = 1
				}
				testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte(fmt.Sprintf("#!/bin/sh\ncase $1 in stop) printf Stopped > %q;; start) [ %d = 0 ] || exit 1; printf Running > %q;; esac\n", status, startExit, status)), 0700)
				if outcome == "success" {
					scriptPath := filepath.Join(root, "scripts/lifecycle-guard.sh")
					original, err := os.ReadFile(scriptPath)
					if err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, scriptPath, []byte(strings.Replace(string(original), "[ 0 = 0 ]", "[ 1 = 0 ]", 1)), 0700)
					testkit.WriteFile(t, status, []byte("Running"), 0600)
					runtime := program.options.InitPlatform.(reconcileruntime.Runtime)
					if err := migration.CompleteRegisteredDiskTmp(context.Background(), runtime); err == nil {
						t.Fatal("injected physical start succeeded")
					}
					actual, err := native.Instance(context.Background(), instance.Project, instance.Name)
					if err != nil || actual.Status != "Stopped" || actual.Config["user.subyard.desired_power"] != "running" || actual.Config["user.subyard.disk_tmp"] != "" {
						t.Fatal("physical failure fixture lost pending desired-running state", actual, err)
					}
					testkit.WriteFile(t, scriptPath, original, 0700)
				}

				start, _ := program.manifest.Lookup("start")
				p := &preparedCommand{CLI: program, Definition: start, Loaded: loaded, Arguments: []string{"--yes"}}
				ctx := context.Background()
				if err := p.prepareLifecycle(ctx, nil); err != nil {
					t.Fatal(err)
				}
				if err := p.preparePlan(ctx); err != nil {
					t.Fatal(err)
				}
				if outcome != "marked" && !strings.Contains(p.Plan.Steps[0].Consequence, "sessions") {
					t.Fatal("start omitted restart disclosure", p.Plan.Steps)
				}
				o := program.operationOrchestrator(p.Plan.OperationID, loaded, nil, &start)
				o.Runner = &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, OperationID: p.Plan.OperationID, Status: "ok"}, Apply: func(domain.AdapterRequest) {
					testkit.WriteFile(t, status, []byte("Running"), 0600)
					host.snapshot.Yards[0].InstanceInfo.Status = "Running"
				}}}}
				if _, err := p.Confirm(ctx, o, true); err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				result, err := p.execute(ctx, o, &output)
				if outcome == "stopped-failure" {
					if err == nil || !strings.Contains(err.Error(), "disk-/tmp completion failed") {
						t.Fatalf("stopped yard failure lost: %v %s", err, output.String())
					}
					return
				}
				if err != nil || result.Status != "ok" {
					t.Fatalf("running yard start failed: %+v %v %s", result, err, output.String())
				}
				after, err := native.Instance(ctx, instance.Project, instance.Name)
				if err != nil {
					t.Fatal(err)
				}
				if after.Status != "Running" || after.Config["volatile.uuid"] != "same-instance" {
					t.Fatal("start lost identity or power", after)
				}
				if outcome == "marked" {
					if after.Config["user.subyard.disk_tmp"] != "v1" || after.Config["user.subyard.preview_sha256"] != "old-runtime" || native.markerPower != "" || strings.Contains(output.String(), "pending") {
						t.Fatal("ordinary marked start attempted maintenance or runtime repair", after.Config, output.String())
					}
				} else if outcome == "success" {
					if after.Config["user.subyard.disk_tmp"] != "v1" || native.markerPower != "Running" {
						t.Fatal("explicit start failed to finish pending policy", after.Config)
					}
				} else if !strings.Contains(output.String(), "warning: yard private: disk-/tmp completion pending") || after.Config["user.subyard.disk_tmp"] != "" {
					t.Fatal("running completion failure hidden", output.String())
				}
				if startup && after.Config[startupIntentKey(definition)] != startupEnabled {
					t.Fatal("startup resource did not finish", after.Config)
				}
			})
		}
	}
}
