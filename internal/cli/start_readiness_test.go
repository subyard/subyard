package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
)

func TestAddressWaitingIsVisibleAcrossStatusAndInventory(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	fake := lifecycleIncus()
	instance := fake.Instances["subyard/yard"]
	instance.Config["user.subyard.desired_power"] = "running"
	instance.Devices = map[string]map[string]string{"synthetic": {
		"type": "proxy", "listen": "tcp:192.0.2.12:8080",
	}}
	fake.Instances["subyard/yard"] = instance
	var addresses []netip.Addr
	for _, available := range []bool{false, true} {
		if available {
			addresses = []netip.Addr{netip.MustParseAddr("192.0.2.12")}
		}
		for _, arguments := range [][]string{{"-Y", "default", "status"}, {"status"}, {"yards"}, {"yards", "--json"}} {
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
				Incus: fake, Executor: fake, StatusFacts: statusFactsStub{},
				LocalAddresses: func() ([]netip.Addr, error) { return addresses, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("%v returned %d: %s", arguments, code, stderr.String())
			}
			output := stdout.String()
			if (strings.Contains(output, "WAITING_FOR_ADDRESS") || strings.Contains(output, `"startState":"waiting-for-address"`)) == available ||
				(!available && !strings.Contains(output, "192.0.2.12") && arguments[0] != "yards") {
				t.Fatalf("availability=%v arguments=%v output=%s", available, arguments, output)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
			for _, method := range []string{"yard.status", "owner.inventory"} {
				result, err := handler.Handle(context.Background(), rpc.Call{Method: method, Params: json.RawMessage(`{}`)}, nil)
				if err != nil {
					t.Fatal(err)
				}
				var physical string
				var start domain.StartState
				var missing []string
				switch value := result.(type) {
				case domain.YardStatus:
					physical, start, missing = value.State, value.StartState, value.WaitingForAddresses
				case domain.OwnerInventory:
					if err := value.Validate(); err != nil {
						t.Fatal(err)
					}
					physical, start, missing = value.Yards[0].State, value.Yards[0].StartState, value.Yards[0].WaitingForAddresses
				default:
					t.Fatalf("unexpected %s result: %T", method, result)
				}
				if !strings.EqualFold(physical, "stopped") || (start == domain.StartWaitingForAddress) == available ||
					(len(missing) != 0) == available {
					t.Fatalf("%s availability=%v physical=%s start=%s missing=%v", method, available, physical, start, missing)
				}
			}
		}
	}
	if len(fake.PowerUpdates) != 0 {
		t.Fatalf("read-only status changed power: %+v", fake.PowerUpdates)
	}
}

func TestDeferredPublicationIsVisibleWithoutChangingRunningStatus(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	fake := lifecycleIncus()
	instance := fake.Instances["subyard/yard"]
	instance.Status = "Running"
	instance.Config["user.subyard.desired_power"] = "running"
	instance.Config["user.subyard.deferred_publication.synthetic"] = `{"version":1,"ownership":"fixture","device":{"listen":"tcp:192.0.2.12:8080"}}`
	fake.Instances["subyard/yard"] = instance
	want := []string{"tcp:192.0.2.12:8080"}
	for _, arguments := range [][]string{{"-Y", "default", "status"}, {"status"}, {"yards", "--json"}} {
		var stdout, stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments,
			Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
			Incus: fake, Executor: fake, StatusFacts: statusFactsStub{},
			LocalAddresses: func() ([]netip.Addr, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("%v returned %d: %s", arguments, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "RUNNING") || !strings.Contains(stdout.String(), want[0]) || strings.Contains(stdout.String(), "WAITING_FOR_ADDRESS") {
			t.Fatalf("running yard publication diagnostic missing: %s", stdout.String())
		}
		loaded, err := program.loadContext("default")
		if err != nil {
			t.Fatal(err)
		}
		handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
		for _, method := range []string{"yard.status", "owner.inventory"} {
			result, rpcErr := handler.Handle(context.Background(), rpc.Call{Method: method, Params: json.RawMessage(`{}`)}, nil)
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			var state string
			var start domain.StartState
			var waiting, publications []string
			switch value := result.(type) {
			case domain.YardStatus:
				state, start, waiting, publications = value.State, value.StartState, value.WaitingForAddresses, value.DeferredPublications
			case domain.OwnerInventory:
				if err := value.Validate(); err != nil {
					t.Fatal(err)
				}
				yard := value.Yards[0]
				state, start, waiting, publications = yard.State, yard.StartState, yard.WaitingForAddresses, yard.DeferredPublications
			}
			if !strings.EqualFold(state, "running") || start != "" || len(waiting) != 0 || !reflect.DeepEqual(publications, want) {
				t.Fatalf("%s state=%s start=%s required=%v publications=%v", method, state, start, waiting, publications)
			}
			assertPublicationResultSchema(t, method, result)
		}
	}
	if len(fake.PowerUpdates) != 0 {
		t.Fatal("status changed physical power")
	}
}

// This explicit public contract check validates the native serialized result
// against the closed schema and the publication property's declared item type.
func assertPublicationResultSchema(t *testing.T, method string, result any) {
	t.Helper()
	name := "core-results.schema.json"
	if method == "owner.inventory" {
		name = "owner-inventory.schema.json"
	}
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "api/yard-rpc/v1", name))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatal(err)
	}
	if method == "yard.status" {
		schema = schema["$defs"].(map[string]any)["status"].(map[string]any)
	} else {
		schema = schema["properties"].(map[string]any)["yards"].(map[string]any)["items"].(map[string]any)
		value = value["yards"].([]any)[0].(map[string]any)
	}
	if schema["additionalProperties"] != false {
		t.Fatal("status schema must remain closed")
	}
	properties := schema["properties"].(map[string]any)
	for key := range value {
		if _, exists := properties[key]; !exists {
			t.Fatalf("%s emits %s outside its closed schema", method, key)
		}
	}
	property, exists := properties["deferredPublications"].(map[string]any)
	if !exists || property["type"] != "array" || property["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("%s publication property must be an array of strings", method)
	}
	for _, item := range value["deferredPublications"].([]any) {
		if _, ok := item.(string); !ok {
			t.Fatal("publication diagnostic must be a string")
		}
	}
}
