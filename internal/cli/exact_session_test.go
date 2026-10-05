package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/rpc"
)

func TestExactSessionOwnerClockPrunesAndLiveInputsRemainGuarded(t *testing.T) {
	for _, mode := range []string{"owner-clock", "live-input", "executing-bound"} {
		t.Run(mode, func(t *testing.T) {
			program, _, runtime, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
			runtime.plan.Scope = "captured synthetic integration targets"
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(100, 0)
			handler := &rpcHandler{cli: program, loaded: loaded, clock: func() time.Time { return now }}
			defer handler.closePlans()
			request := json.RawMessage(`{"command":"integration","arguments":["enable","codex"],"exact":true,"stepSchema":1}`)
			if mode == "executing-bound" {
				handler.executingPlans = make(map[string]struct{})
				for i := 0; i < 64; i++ {
					handler.executingPlans["executing-"+strconv.Itoa(i)] = struct{}{}
				}
				_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "extra", Params: request}, nil)
				var fault *rpc.Error
				if !errors.As(err, &fault) || fault.Code != "too_many_plans" || len(handler.plans) != 0 {
					t.Fatalf("executing operations evaded the bound: %v", err)
				}
				return
			}
			value, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "captured", Params: request}, nil)
			if err != nil {
				t.Fatal(err)
			}
			exact := value.(exactOperationPlan)
			prepared := handler.plans["captured"]
			if exact.ExpiresAt != now.Add(exactPlanLifetime) {
				t.Fatal("expiry does not use owner clock")
			}
			if mode == "owner-clock" {
				now = exact.ExpiresAt
				if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "next", Params: request}, nil); err != nil {
					t.Fatal(err)
				}
				if !prepared.closed || handler.plans["captured"] != nil || handler.exactPlans["captured"].Digest != "" {
					t.Fatal("expired plan retained resources")
				}
				return
			}
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			payload = append(payload, []byte("SSH_PORT=2244\n")...)
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			params, _ := json.Marshal(map[string]any{"confirmed": true, "digest": exact.Digest})
			if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "captured", Params: params}, nil); err == nil {
				t.Fatal("real config drift accepted")
			}
			if runtime.applied != 0 || !prepared.closed {
				t.Fatal("real config drift reached native apply or leaked plan")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(payload) {
				t.Fatal("stale operation overwrote live configuration")
			}
		})
	}
}
