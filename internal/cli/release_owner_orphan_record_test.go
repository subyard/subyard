package cli

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestOwnerOrphanRecordReservedVMOverrideIsReachable(t *testing.T) {
	cli, initial, host := orphanIngressFixture(t, false)
	template, err := os.ReadFile("../../config/yards/profiles/test-vms.env")
	if err != nil {
		t.Fatal(err)
	}
	templatePath := filepath.Join(cli.options.RepositoryRoot, "config/yards/profiles/test-vms.env")
	if err := os.MkdirAll(filepath.Dir(templatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, templatePath, template, 0o600)
	definition, ok := cli.resources.Lookup("relay")
	if !ok {
		t.Fatal("fixture resource is missing")
	}
	for _, name := range []string{"e2e-yard", "test-yard"} {
		registration := filepath.Join(initial.Context.Paths.ConfigHome, "yards", name, "config.env")
		if err := os.MkdirAll(filepath.Dir(registration), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, registration, []byte("YARD_TEMPLATE=test-vms\nYARD_KIND=vm\nNESTED_E2E_VMS=0\nENVIRONMENT_PROFILES=\n"), 0o600)
		cli.env = maps.Clone(cli.baseEnv)
		loaded, err := cli.loadContext(name)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Context.YardKind != domain.YardVM || loaded.Context.NestedE2EVMs || loaded.Environment["YARD_TEMPLATE"] != "test-vms" {
			t.Fatal("valid template override did not retain the migration role")
		}
		yard := networkYard(loaded.Context)
		device := map[string]string{"type": "proxy", "listen": "udp:192.0.2.1:41999", "connect": "udp:10.0.0.2:41999", "bind": "host", "nat": "true"}
		marker := definition.Proxy.OwnershipValue(device)
		host.snapshot = yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{Yard: yard, InstanceFound: true, InstanceInfo: ports.InstanceInfo{Type: domain.YardVM, Status: "Stopped", LocalDevices: map[string]map[string]string{definition.Proxy.Device: device}, Devices: map[string]map[string]string{definition.Proxy.Device: device}, LocalConfig: map[string]string{definition.Proxy.OwnershipKey(): marker}}}}}
		plan, err := cli.prepareOrphanIngress(context.Background(), loaded)
		if err != nil || plan == nil || len(plan.entries) != 1 || plan.entries[0].shutdownEligible {
			t.Fatalf("reserved VM orphan is unreachable: plan=%+v err=%v", plan, err)
		}
	}
}

const ownerOrphanTestDescriptor = `COMMAND=relayctl
HANDLER=resources/relay/handler.sh
TITLE="Synthetic relay"
PROXY="sample-relay RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT RESOURCE_RELAY_INTERFACE udp:guest:41999 owner-metadata-v1 owner-ipv4-udp"
DASHBOARD="http RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT /status"
MANAGEMENT="client RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT dev"
ENDPOINT_DEFAULTS="tailscale-self 41999"
ACTION="up up public-ingress-change reversible"
ACTION="down down public-ingress-change reversible"
ACTION="idle down read-only not-needed"
ACTION="attach attach session not-needed"
BRINGUP=up
SHUTDOWN=down
STARTUP=bringup
CONTROLLER_SESSION=attach
`

func ownerOrphanTestCLI(t *testing.T, root, descriptor string) *CLI {
	t.Helper()
	directory := filepath.Join(root, "config/profiles/sample/resources/relay")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(filepath.Dir(directory), "relay.res"), []byte(descriptor), 0o600)
	for _, name := range []string{"handler.sh", "other.sh"} {
		testkit.WriteFile(t, filepath.Join(directory, name), []byte("#!/bin/sh\nprintf unexpected-native-effect > \"$0.effect\"\nexit 99\n"), 0o700)
	}
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return &CLI{options: Options{RepositoryRoot: root}, resources: registry}
}

func ownerOrphanTestPlan(t *testing.T, cli *CLI) *orphanIngressPlan {
	t.Helper()
	definition, ok := cli.resources.Lookup("relayctl")
	if !ok {
		t.Fatal("sealed fixture command missing")
	}
	var shutdown domain.ActionDefinition
	for _, action := range cli.resources.ActionDefinitions() {
		if strings.HasSuffix(string(action.Action), ".down") {
			shutdown = action
		}
	}
	value := "external-path-\xff\xfe"
	return &orphanIngressPlan{yard: yardnetwork.Yard{Name: "test-yard", Project: "subyard-test-yard", Instance: "yard-test-yard", Network: value}, entries: []orphanIngressEntry{{definition: definition, contract: *definition.Proxy, device: map[string]string{"listen": value, "original-key-\xff": value}, marker: value, shutdownEligible: true, shutdown: &domain.ActionAssessment{Action: shutdown.Action, Effect: shutdown.Effect, Changed: true, Impacts: shutdown.Impacts, Recovery: shutdown.Recovery, Consequences: []string{value}}}}}
}

func TestOwnerOrphanRecordOriginalLosslessRoundTripAndNoAlias(t *testing.T) {
	root := filepath.Join(testkit.TempDir(t), "source-\xff")
	cli := ownerOrphanTestCLI(t, root, ownerOrphanTestDescriptor)
	original := ownerOrphanTestPlan(t, cli)
	raw, err := captureOwnerOrphanRecord(original, cli, root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restoreOwnerOrphanRecord(raw, cli, root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatal("original orphan definition/maps/assessment or nonUTF8 bytes changed")
	}
	again, err := captureOwnerOrphanRecord(got, cli, root)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("nondeterministic snapshot: %v", err)
	}
	got.entries[0].device["listen"] = "mutated"
	got.entries[0].shutdown.Consequences[0] = "mutated"
	got.entries[0].shutdown.Impacts[0] = "mutated"
	got.entries[0].definition.Verbs[0] = "mutated"
	got.entries[0].definition.Proxy.Device = "mutated"
	if original.entries[0].device["listen"] != "external-path-\xff\xfe" || original.entries[0].shutdown.Consequences[0] != "external-path-\xff\xfe" || original.entries[0].definition.Verbs[0] != "up" || original.entries[0].definition.Proxy.Device != "sample-relay" {
		t.Fatal("restored plan aliases original state")
	}
	sealed, _ := cli.resources.Lookup("relayctl")
	if sealed.Proxy.Device != "sample-relay" || sealed.Verbs[0] != "up" {
		t.Fatal("restored definition mutated sealed registry")
	}
}

func TestOwnerOrphanRecordPreservesNilEmptyAndUnavailableShape(t *testing.T) {
	if raw, err := captureOwnerOrphanRecord(nil, nil, ""); err != nil || raw != nil {
		t.Fatalf("nil capture changed: %v", err)
	}
	if plan, err := restoreOwnerOrphanRecord(nil, nil, ""); err != nil || plan != nil {
		t.Fatalf("nil restore changed: %v", err)
	}
	for _, entries := range [][]orphanIngressEntry{nil, {}} {
		original := &orphanIngressPlan{entries: entries}
		raw, err := captureOwnerOrphanRecord(original, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := restoreOwnerOrphanRecord(raw, nil, "")
		if err != nil || !reflect.DeepEqual(original, got) {
			t.Fatalf("empty original plan shape changed: %v", err)
		}
	}
	root := testkit.TempDir(t)
	cli := ownerOrphanTestCLI(t, root, ownerOrphanTestDescriptor)
	for _, shape := range []string{"nil device", "empty device", "nil slices", "empty slices", "unavailable", "ineligible"} {
		t.Run(shape, func(t *testing.T) {
			original := ownerOrphanTestPlan(t, cli)
			entry := &original.entries[0]
			switch shape {
			case "nil device":
				entry.device = nil
			case "empty device":
				entry.device = map[string]string{}
			case "nil slices", "empty slices":
				entry.shutdown = &domain.ActionAssessment{Action: "resource.sample.relay.idle", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded}
				if shape == "empty slices" {
					entry.shutdown.Impacts = []domain.ActionImpact{}
					entry.shutdown.Consequences = []string{}
				}
			case "unavailable":
				entry.shutdown = nil
				entry.shutdownUnavailable = true
			case "ineligible":
				entry.shutdown = nil
				entry.shutdownEligible = false
			}
			raw, err := captureOwnerOrphanRecord(original, cli, root)
			if err != nil {
				t.Fatal(err)
			}
			got, err := restoreOwnerOrphanRecord(raw, cli, root)
			if err != nil || !reflect.DeepEqual(original, got) {
				t.Fatalf("original shape changed: %v", err)
			}
		})
	}
}

func TestOwnerOrphanRecordRejectsMalformedAndDomainDescriptors(t *testing.T) {
	root := testkit.TempDir(t)
	cli := ownerOrphanTestCLI(t, root, ownerOrphanTestDescriptor)
	raw, err := captureOwnerOrphanRecord(ownerOrphanTestPlan(t, cli), cli, root)
	if err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string][]byte{"wrong type": []byte(`[]`), "truncated": raw[:len(raw)-1], "trailing": append(bytes.Clone(raw), ' '), "unknown": bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Unknown":null,"Version":1`), 1), "duplicate field": bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Version":1,"Version":1`), 1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := restoreOwnerOrphanRecord(invalid, cli, root); err == nil {
				t.Fatal("malformed descriptor accepted")
			}
		})
	}
	for _, change := range []string{"version", "root", "command alias", "definition", "handler source", "action identity", "action verb", "contract", "device duplicate", "device presence", "empty device rows", "assessment action", "assessment effect", "flags", "duplicate entry"} {
		t.Run(change, func(t *testing.T) {
			var record ownerOrphanRecord
			if err := nativejson.DecodeLosslessNativeJSON(raw, &record, ownerOrphanRecordLimit); err != nil {
				t.Fatal(err)
			}
			entry := &record.Entries[0]
			switch change {
			case "version":
				record.Version = 2
			case "root":
				record.RepositoryRoot += "-foreign"
			case "command alias":
				entry.Definition.Command = "relay"
			case "definition":
				entry.Definition.Title = "different"
			case "handler source":
				entry.Definition.HandlerPath += "-different"
			case "action identity":
				entry.Definition.Actions[0].Definition.Action = "resource.sample.relay.unknown"
			case "action verb":
				entry.Definition.Actions[0].Verb = "different"
			case "contract":
				entry.Contract.Device = "foreign"
			case "device duplicate":
				entry.Device = append(entry.Device, entry.Device[0])
			case "device presence":
				entry.DevicePresent = false
			case "empty device rows":
				entry.Device = []ownerOrphanDeviceRow{}
			case "assessment action":
				entry.Shutdown.Action = "resource.sample.relay.up"
			case "assessment effect":
				entry.Shutdown.Effect = domain.ActionDestruction
			case "flags":
				entry.ShutdownUnavailable = true
			case "duplicate entry":
				record.Entries = append(record.Entries, *entry)
			}
			invalid, err := nativejson.EncodeLosslessNativeJSON(record, ownerOrphanRecordLimit)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restoreOwnerOrphanRecord(invalid, cli, root); err == nil {
				t.Fatal("invalid original identity or domain accepted")
			}
		})
	}
}

func TestOwnerOrphanRecordRejectsChangedSealedDefinitionAndActionMapping(t *testing.T) {
	for _, change := range []string{"title", "proxy", "handler", "action verb", "action order"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			cli := ownerOrphanTestCLI(t, root, ownerOrphanTestDescriptor)
			plan := ownerOrphanTestPlan(t, cli)
			raw, err := captureOwnerOrphanRecord(plan, cli, root)
			if err != nil {
				t.Fatal(err)
			}
			descriptor := ownerOrphanTestDescriptor
			switch change {
			case "title":
				descriptor = strings.Replace(descriptor, "Synthetic relay", "Changed relay", 1)
			case "proxy":
				descriptor = strings.Replace(descriptor, "udp:guest:41999", "udp:guest:42000", 1)
			case "handler":
				descriptor = strings.Replace(descriptor, "handler.sh", "other.sh", 1)
			case "action verb":
				descriptor = strings.Replace(descriptor, `ACTION="down down`, `ACTION="down up`, 1)
			case "action order":
				descriptor = strings.Replace(descriptor, "ACTION=\"down down public-ingress-change reversible\"\nACTION=\"idle down read-only not-needed\"", "ACTION=\"idle down read-only not-needed\"\nACTION=\"down down public-ingress-change reversible\"", 1)
			}
			changed := ownerOrphanTestCLI(t, root, descriptor)
			if _, err := restoreOwnerOrphanRecord(raw, changed, root); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed sealed definition accepted: %v", err)
			}
			if _, err := captureOwnerOrphanRecord(plan, changed, root); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("foreign original definition captured: %v", err)
			}
		})
	}
}

func TestOwnerOrphanRecordSupportsCheckedSourceRebasingWithoutNativeCalls(t *testing.T) {
	root := filepath.Join(testkit.TempDir(t), "original-\xff")
	cli := ownerOrphanTestCLI(t, root, ownerOrphanTestDescriptor)
	plan := ownerOrphanTestPlan(t, cli)
	raw, err := captureOwnerOrphanRecord(plan, cli, root)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(testkit.TempDir(t), "checked-source-\xfe")
	checked := ownerOrphanTestCLI(t, physical, ownerOrphanTestDescriptor)
	got, err := restoreOwnerOrphanRecord(raw, checked, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.entries[0].definition.HandlerPath(), physical+"/") || !reflect.DeepEqual(got.entries[0].shutdown, plan.entries[0].shutdown) || !maps.Equal(got.entries[0].device, plan.entries[0].device) {
		t.Fatal("restored checked fixed source or original assessment changed")
	}
	again, err := captureOwnerOrphanRecord(got, checked, root)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("source label normalization changed original bytes: %v", err)
	}
	for _, path := range []string{plan.entries[0].definition.HandlerPath(), got.entries[0].definition.HandlerPath()} {
		if _, err := os.Stat(path + ".effect"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("handler prepare/effect ran: %v", err)
		}
	}
	alias := *checked
	alias.options.RepositoryRoot = "/proc/self/fd/72"
	leaf := "/config/profiles/sample/resources/relay/handler-\xff.sh"
	own := "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/72" + leaf
	foreign := "/proc/" + strconv.Itoa(os.Getpid()+1) + "/fd/72" + leaf
	if ownerOrphanHandlerLabel(own, &alias, root) != root+leaf || ownerOrphanHandlerLabel(foreign, &alias, root) != foreign {
		t.Fatal("owner handler aliases changed foreign source identity")
	}
}

func TestOwnerOrphanRecordFreshProcessRestoresOriginal(t *testing.T) {
	if root := os.Getenv("SUBYARD_TEST_ORPHAN_RECORD_ROOT"); root != "" {
		var unrelated bytes.Buffer
		if err := gob.NewEncoder(&unrelated).Encode(struct{ First string }{"unrelated codec"}); err != nil {
			t.Fatal(err)
		}
		registry, err := resource.Load(root)
		if err != nil {
			t.Fatal(err)
		}
		cli := &CLI{options: Options{RepositoryRoot: root}, resources: registry}
		raw, err := os.ReadFile(filepath.Join(root, "record"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := restoreOwnerOrphanRecord(raw, cli, root)
		if err != nil || !reflect.DeepEqual(got, ownerOrphanTestPlan(t, cli)) {
			t.Fatalf("fresh process lost original: %v", err)
		}
		again, err := captureOwnerOrphanRecord(got, cli, root)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatalf("fresh process byte identity changed: %v", err)
		}
		if _, err := os.Stat(got.entries[0].definition.HandlerPath() + ".effect"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fresh process called native handler: %v", err)
		}
		return
	}
	root := filepath.Join(testkit.TempDir(t), "original-\xff")
	cli := ownerOrphanTestCLI(t, root, ownerOrphanTestDescriptor)
	raw, err := captureOwnerOrphanRecord(ownerOrphanTestPlan(t, cli), cli, root)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "record"), raw, 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestOwnerOrphanRecordFreshProcessRestoresOriginal$", "-test.count=1")
	command.Env = append(os.Environ(), "SUBYARD_TEST_ORPHAN_RECORD_ROOT="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh process failed: %v\n%s", err, output)
	}
}
