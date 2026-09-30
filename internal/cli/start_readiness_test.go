package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
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
