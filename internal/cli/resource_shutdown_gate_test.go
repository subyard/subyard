package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func shutdownGateFixture(t *testing.T, scenario string, installedGuest ...bool) (*CLI, string, *bytes.Buffer) {
	t.Helper()
	program, _, home := completedSourceAuthoringFixture(t, false, map[string]string{
		"config/profiles/sample/resources/relay.res": `
COMMAND=relay
HANDLER=resources/relay/handler.sh
TITLE="Sample relay"
PROXY="sample-relay RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT RESOURCE_RELAY_INTERFACE udp:guest:41999 owner-metadata-v1 owner-ipv4-udp"
ACTION="up up public-ingress-change reversible"
ACTION="down down public-ingress-change reversible"
BRINGUP=up
SHUTDOWN=down
`,
		"config/profiles/sample/resources/relay/handler.sh": `#!/bin/sh
set -eu
case "$SUBYARD_RESOURCE_MODE" in
prepare)
  printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":true,"consequences":["close owned ingress; preserve state and retain unverified guest cleanup"]}\n' "$1"
  ;;
apply)
  [ "$1" = down ]
  [ "$SUBYARD_RESOURCE_ACTION" = down ]
  [ -n "$SUBYARD_OPERATION_ID" ]
  printf '%s\n' pending >"$SUBYARD_CONFIG_HOME/cleanup-pending"
  jq 'del(.Yards[0].InstanceInfo.localDevices["sample-relay"], .Yards[0].InstanceInfo.devices["sample-relay"]) | .Yards[0].InstanceInfo.localConfig["user.subyard.resource.sample-relay"] |= sub("^v1:"; "v1:pending:") | .Yards[0].InstanceInfo.config["user.subyard.resource.sample-relay"] = .Yards[0].InstanceInfo.localConfig["user.subyard.resource.sample-relay"]' "$SUBYARD_CONFIG_HOME/instance.json" >"$SUBYARD_CONFIG_HOME/instance.tmp"
  chmod 0600 "$SUBYARD_CONFIG_HOME/instance.tmp"
  mv "$SUBYARD_CONFIG_HOME/instance.tmp" "$SUBYARD_CONFIG_HOME/instance.json"
  printf '%s\n' 'guest shutdown is unverified' >&2
  exit 1
  ;;
esac
`,
	})
	root := program.options.RepositoryRoot
	if err := os.MkdirAll(filepath.Join(home, "yards/default"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/default/config.env"), []byte("# unchanged owner context\n"), 0600)
	if scenario == "v2" {
		handler := filepath.Join(root, "config/profiles/sample/resources/relay/handler.sh")
		content := string(mustReadConfigFixture(t, handler))
		start := strings.Index(content, "  printf '{\"schema\"")
		end := strings.Index(content[start:], "\n  ;;") + start
		content = content[:start] + `  if [ -e "$SUBYARD_CONFIG_HOME/native-drift" ]; then printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"down","changed":true,"consequences":["different native target"]}'; exit 0; fi
  printf '%s\n' '{"schema":"yard.resource-action-assessment.v2","action":"down","changed":true,"consequences":["close owned ingress; retain unverified guest cleanup"],"binding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","steps":[{"id":"ingress","target":"incus:subyard-private/yard-private owned route","observed":"open owned route","desired":"closed owned route with pending cleanup","decision":"apply","verify":"inspect exact owned route","consequence":"close owned ingress; retain unverified guest cleanup"},{"id":"guest","target":"fixture guest runtime","observed":"unknown","desired":"disabled","decision":"conditional","dependsOn":["ingress"],"preconditions":["the owned ingress is closed"],"verify":"read guest runtime","consequence":"attempt guest shutdown; retain pending cleanup"}]}'` + content[end:]
		testkit.WriteFile(t, handler, []byte(content), 0700)
	}
	if err := os.MkdirAll(filepath.Join(home, "yards/private"), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(home, "yards/private/config.env"), "YARD_KIND=vm\nENVIRONMENT_PROFILES=sample\nSSH_PORT=2234\n", 0o600)
	journalPath := filepath.Join(home, "release-transition/v2/journal.json")
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, journalPath))
	if err != nil {
		t.Fatal(err)
	}
	if scenario == "unfinished" {
		journal.Checkpoint = releasetransition.JournalReconciling
	}
	outcome := releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired,
		Active: journal.Goal.Target, Target: journal.Goal.Target, Code: releasetransition.CodeDependencyUnavailable,
		Message: "yard private: cannot inspect profile sample-runtime runtime for release activation", Retry: "run yard -Y private status", Transaction: &journal.Transaction}
	response := releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true,
		Inspection: &releasetransition.Inspection{Plan: releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
			Outcome: &outcome, Assessment: domain.ActionAssessment{Action: "release.transition.v2", Effect: domain.ActionMutation,
				Changed: true, Impacts: []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime},
				Recovery: domain.RecoveryReversible, Consequences: []string{"verify installed runtime"}},
			Blockers: []releasetransition.Blocker{{Code: outcome.Code, Resource: "activation.sample-runtime", Message: outcome.Message, Retry: outcome.Retry}}}}
	if len(installedGuest) != 0 && installedGuest[0] {
		loaded, err := program.loadContext("private")
		if err != nil {
			t.Fatal(err)
		}
		agentLost := errors.New("fixture guest agent unavailable")
		incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
			loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
				Status: "Running", Config: map[string]string{"user.subyard.preview_sha256": "owned"},
			},
		}, ExecSteps: []testkit.IncusExecStep{{Err: agentLost}}}
		// Exercise the native installed observer before sealing the response that
		// the public Run fixture consumes; do not substitute a stale/absent state.
		state, err := observeProfileRuntime(context.Background(), reconcileruntime.Runtime{
			Yard: loaded.Context, Incus: incus, Executor: incus,
		}, "core-guest-runtime")
		if !errors.Is(err, agentLost) || state.State != "" || len(incus.ExecCalls) != 1 {
			t.Fatalf("installed guest failure was not observed: %#v %v calls=%d", state, err, len(incus.ExecCalls))
		}
		owner := program.profileActivationReconciler(releasetransition.ProcessRequest{}, "core-guest-runtime").(*profileRuntimeActivationReconciler)
		message, retry := owner.failure("private", "inspect", err).(materializedConfigActivationError).ActivationDiagnostic()
		response.Inspection.Blockers = append(response.Inspection.Blockers,
			releasetransition.Blocker{Code: outcome.Code, Resource: "activation.materialized-config", Message: outcome.Message, Retry: outcome.Retry},
			releasetransition.Blocker{Code: outcome.Code, Resource: "activation." + owner.ID(), Message: message, Retry: retry})
	}
	if scenario == "foreign-blocker" {
		response.Inspection.Blockers[0].Resource = "activation.route-consumers"
	}
	if scenario == "key-tools-blocker" {
		response.Inspection.Blockers[0].Resource = "activation.key-tools-runtime"
	}
	if scenario == "ambiguous" {
		response.Inspection.Blockers[0].Code = releasetransition.CodeActivationAmbiguous
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n--version) printf 'yard-engine 1.2.3\\n';;\n_release-transition) cat >/dev/null; printf '%%s\\n' %q;;\n*) exit 64;;\nesac\n", payload)
	changed := *response.Inspection
	changed.Blockers = append([]releasetransition.Blocker(nil), changed.Blockers...)
	changed.Blockers[0].Resource = "activation.route-consumers"
	if len(installedGuest) != 0 && installedGuest[0] {
		changed.Blockers[len(changed.Blockers)-1].Resource = "activation.key-tools-runtime"
	}
	replacement := response
	replacement.Inspection = &changed
	replacementPayload, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	engine = strings.Replace(engine, "cat >/dev/null; printf", fmt.Sprintf("cat >/dev/null; if [ -e %q ]; then printf '%%s\\n' %q; exit 0; fi; printf", filepath.Join(home, "blocker-change"), replacementPayload), 1)
	testkit.WriteFile(t, filepath.Join(root, "bin/yard-engine"), []byte(engine), 0o700)
	manifest := ""
	for _, tree := range []string{"bin", "config", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			manifest += fmt.Sprintf("%x  ./%s\n", sha256.Sum256(mustReadConfigFixture(t, path)), filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest), 0o600)
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256([]byte(manifest))))
	payload, err = releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, journalPath, payload, 0o600)
	var output bytes.Buffer
	options := program.options
	options.Arguments, options.Stdout, options.Stderr = []string{"-Y", "private", "relay", "down", "--yes"}, &output, &output
	program, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("private")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.resources.Lookup("relay")
	contract := *definition.Proxy
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	if scenario == "foreign-route" {
		device["connect"] = "udp:10.80.0.11:41999"
	}
	marker := contract.OwnershipValue(device)
	if scenario == "foreign-route" {
		marker = "v1:" + strings.Repeat("0", 64)
	}
	host := &rollbackIngressHost{snapshot: yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{
		Yard: networkYard(loaded.Context), InstanceFound: true,
		InstanceInfo: ports.InstanceInfo{Type: domain.YardVM, Status: "Running",
			LocalDevices: map[string]map[string]string{contract.Device: maps.Clone(device)},
			Devices:      map[string]map[string]string{contract.Device: maps.Clone(device), "eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}},
			LocalConfig:  map[string]string{contract.OwnershipKey(): marker}, Config: map[string]string{contract.OwnershipKey(): marker}},
	}}}}
	data, err := json.Marshal(host.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "instance.json"), data, 0600)
	testkit.WriteFile(t, filepath.Join(home, "retained-key"), []byte("synthetic retained state"), 0600)
	program.options.NetworkPolicy = &yardnetwork.Service{Host: shutdownGateHost{rollbackIngressHost: host, path: filepath.Join(home, "instance.json")}}
	return program, home, &output
}

type shutdownGateHost struct {
	*rollbackIngressHost
	path string
}

func (host shutdownGateHost) InspectNetwork(context.Context, []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	data, err := os.ReadFile(host.path)
	if err != nil {
		return yardnetwork.Snapshot{}, err
	}
	var snapshot yardnetwork.Snapshot
	err = json.Unmarshal(data, &snapshot)
	return snapshot, err
}

func TestResourceShutdownAdmitsCompletedHistoryUnavailableGuest(t *testing.T) {
	for _, scenario := range []struct {
		name, protocol string
		installedGuest bool
	}{{"v1", "", false}, {"v2", "v2", false}, {"installed-guest-v1", "", true}, {"installed-guest-v2", "v2", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			program, home, output := shutdownGateFixture(t, scenario.protocol, scenario.installedGuest)
			invariants := nativeCleanupRecoveryInvariants(t, program, home)
			if code := program.Run(context.Background()); code != 1 || !strings.Contains(output.String(), "guest shutdown is unverified") {
				t.Fatalf("declared shutdown blocked before guest cleanup: code=%d output=%s", code, output)
			}
			if got := mustReadConfigFixture(t, filepath.Join(home, "cleanup-pending")); string(got) != "pending\n" {
				t.Fatal("pending cleanup not retained")
			}
			var snapshot yardnetwork.Snapshot
			if err := json.Unmarshal(mustReadConfigFixture(t, filepath.Join(home, "instance.json")), &snapshot); err != nil {
				t.Fatal(err)
			}
			instance := snapshot.Yards[0].InstanceInfo
			if instance.LocalDevices["sample-relay"] != nil || instance.Devices["sample-relay"] != nil || !strings.HasPrefix(instance.LocalConfig["user.subyard.resource.sample-relay"], "v1:pending:") {
				t.Fatal("unverified cleanup did not close only owned ingress and retain pending marker")
			}
			if got := mustReadConfigFixture(t, filepath.Join(home, "retained-key")); string(got) != "synthetic retained state" {
				t.Fatal("cleanup lost retained data")
			}
			invariants()
		})
	}
}

func TestResourceShutdownRepairRejectsUnsafeAdmission(t *testing.T) {
	for _, scenario := range []string{"unfinished", "foreign-blocker", "key-tools-blocker", "ambiguous", "foreign-route", "bring-up", "arguments", "corrupt-journal", "changed-source", "deselected", "foreign-links", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			program, home, output := shutdownGateFixture(t, scenario, scenario == "key-tools-blocker")
			switch scenario {
			case "bring-up":
				program.options.Arguments = []string{"-Y", "private", "relay", "up", "--yes"}
			case "arguments":
				program.options.Arguments = []string{"-Y", "private", "relay", "down", "--extra", "--yes"}
			case "corrupt-journal":
				testkit.WriteFile(t, filepath.Join(home, "release-transition/v2/journal.json"), []byte("corrupt"), 0600)
			case "changed-source":
				testkit.WriteFile(t, filepath.Join(program.options.RepositoryRoot, "config/profiles/sample/resources/relay/handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			case "deselected":
				testkit.WriteFile(t, filepath.Join(home, "yards/private/config.env"), []byte("YARD_KIND=vm\nENVIRONMENT_PROFILES=\nSSH_PORT=2234\n"), 0600)
			case "foreign-links":
				link := filepath.Join(program.env["YARD_RUNTIME_ROOT"], "current")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("releases/release-a", link); err != nil {
					t.Fatal(err)
				}
			}
			before := mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/journal.json"))
			ctx := context.Background()
			if scenario == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if code := program.Run(ctx); code == 0 {
				t.Fatalf("unsafe admission accepted: %s", output)
			}
			if _, err := os.Stat(filepath.Join(home, "cleanup-pending")); !os.IsNotExist(err) {
				t.Fatalf("unsafe admission reached cleanup: %v", err)
			}
			if !bytes.Equal(before, mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/journal.json"))) {
				t.Fatal("failed admission changed history")
			}
		})
	}
}

func TestResourceShutdownRepairRevalidatesAfterConsent(t *testing.T) {
	for _, scenario := range []string{"decline", "journal", "ledger", "source", "configuration", "blocker", "installed-blocker"} {
		t.Run(scenario, func(t *testing.T) {
			program, home, output := shutdownGateFixture(t, "", scenario == "installed-blocker")
			program.options.Arguments = []string{"-Y", "private", "relay", "down"}
			consentReached := false
			if scenario == "decline" {
				program.options.Prompt = &testkit.Prompt{Answers: []bool{false}}
			} else {
				program.options.Prompt = &callbackPrompt{callback: func() {
					consentReached = true
					if strings.HasSuffix(scenario, "blocker") {
						testkit.WriteFile(t, filepath.Join(home, "blocker-change"), []byte("changed"), 0600)
						return
					}
					paths := map[string]string{"journal": "release-transition/v2/journal.json", "ledger": "release-transition/v2/history-checkpoint.json", "configuration": "yards/private/config.env"}
					path := filepath.Join(home, paths[scenario])
					if scenario == "source" {
						path = filepath.Join(program.options.RepositoryRoot, "config/profiles/sample/resources/relay/handler.sh")
					}
					mode := os.FileMode(0600)
					if scenario == "source" {
						mode = 0700
					}
					testkit.WriteFile(t, path, append(mustReadConfigFixture(t, path), byte('\n')), mode)
				}}
			}
			if code := program.Run(context.Background()); code == 0 {
				t.Fatalf("changed/declined cleanup accepted: %s", output)
			}
			if scenario == "installed-blocker" && !consentReached {
				t.Fatal("installed guest blocker did not reach consent before drift")
			}
			if _, err := os.Stat(filepath.Join(home, "cleanup-pending")); !os.IsNotExist(err) {
				t.Fatalf("changed/declined operation mutated cleanup: %v; %s", err, output)
			}
		})
	}
}

func TestResourceShutdownRepairRejectsExactNativeDriftAfterConsent(t *testing.T) {
	program, home, output := shutdownGateFixture(t, "v2")
	program.options.Arguments = []string{"-Y", "private", "relay", "down"}
	program.options.Prompt = &callbackPrompt{callback: func() { testkit.WriteFile(t, filepath.Join(home, "native-drift"), []byte("changed"), 0600) }}
	if code := program.Run(context.Background()); code == 0 {
		t.Fatalf("changed native contract accepted: %s", output)
	}
	if _, err := os.Stat(filepath.Join(home, "cleanup-pending")); !os.IsNotExist(err) {
		t.Fatal("native drift reached cleanup")
	}
	if !strings.Contains(output.String(), "stale") && !strings.Contains(output.String(), "v2") {
		t.Fatalf("wrong native drift rejection: %s", output)
	}
}

func TestProfileRuntimeActivationUnavailableGuestRetainsDeselectedObservation(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "deselected", true: "selected"}[selected], func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			writeRuntimeProfileFixture(t, root)
			home := filepath.Join(root, "state")
			seedCurrentReleaseLedger(t, root, home)
			selection := ""
			if selected {
				selection = "sample"
			}
			writeCLIFile(t, filepath.Join(home, "config.env"), "ENVIRONMENT_PROFILES="+selection+"\n", 0600)
			handler := filepath.Join(root, "config", "profiles", "sample", "runtime.sh")
			writeCLIFile(t, handler, "#!/bin/sh\nprintf '%s\n' called >> \"$SUBYARD_CONFIG_HOME/observed\"\nexit 9\n", 0700)
			fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running"}}}
			program, err := New(Options{RepositoryRoot: root, Environment: append(environment, "PATH="+os.Getenv("PATH")), Incus: fake, Executor: fake})
			if err != nil {
				t.Fatal(err)
			}
			reconciler := program.profileActivationReconciler(releasetransition.ProcessRequest{ConfigHome: home}, "sample-runtime")
			observation, err := reconciler.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err == nil || observation.Converged || !strings.Contains(err.Error(), "profile runtime handler observe failed") {
				t.Fatalf("unavailable running guest accepted: %#v %v", observation, err)
			}
			if data, err := os.ReadFile(filepath.Join(home, "observed")); err != nil || string(data) != "called\n" {
				t.Fatalf("actual handler not observed: %q %v", data, err)
			}
			fake.Instances["subyard/yard"] = ports.InstanceInfo{Status: "Stopped"}
			observation, err = reconciler.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil || !observation.Converged {
				t.Fatalf("stopped runtime not deferred: %#v %v", observation, err)
			}
		})
	}
}
