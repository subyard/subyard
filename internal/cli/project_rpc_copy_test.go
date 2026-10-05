package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
)

func activeProjectRPCFixture(t *testing.T) (*rpcHandler, *preparedCommand, exactOperationPlan, *time.Time) {
	t.Helper()
	program, prepared, _ := ownerProjectCopyFixture(t)
	now := time.Now()
	exact := bindExactOperationPlan(prepared, now)
	id := prepared.Plan.OperationID
	handler := &rpcHandler{cli: program, loaded: prepared.Loaded, plans: map[string]*preparedCommand{id: prepared}, exactPlans: map[string]exactOperationPlan{id: exact}, clock: func() time.Time { return now }}
	t.Cleanup(handler.closePlans)
	params, _ := json.Marshal(map[string]any{"confirmed": true, "digest": exact.Digest})
	if _, err := handler.Handle(context.Background(), rpc.Call{OperationID: id, Method: "operation.execute", Params: params}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if len(handler.plans) != 0 || len(handler.activeProjectCopies) != 1 || prepared.Project.ownerCopy.reservation == nil {
		t.Fatal("admission was not retained as one active copy")
	}
	if _, err := prepared.Project.Store.GetReadOnly(context.Background(), prepared.Project.Record.ProjectID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("execute committed before transfer: %v", err)
	}
	return handler, prepared, exact, &now
}

func TestProjectRPCActiveCopyFinalizesOnce(t *testing.T) {
	handler, prepared, exact, _ := activeProjectRPCFixture(t)
	executeParams, _ := json.Marshal(map[string]any{"confirmed": true, "digest": exact.Digest})
	if _, err := handler.Handle(context.Background(), rpc.Call{OperationID: prepared.Plan.OperationID, Method: "operation.execute", Params: executeParams}, nil); err == nil {
		t.Fatal("admission execution replay accepted")
	}
	params, _ := json.Marshal(map[string]string{"digest": exact.Digest})
	call := rpc.Call{OperationID: prepared.Plan.OperationID, Method: "project.copy.finalize", Params: params}
	if _, err := handler.Handle(context.Background(), call, nil); err != nil {
		t.Fatal(err)
	}
	if len(handler.activeProjectCopies) != 0 || prepared.Project.ownerCopy.reservation != nil {
		t.Fatal("finalization retained active admission")
	}
	if _, err := prepared.Project.Store.GetReadOnly(context.Background(), prepared.Project.Record.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Handle(context.Background(), call, nil); err == nil {
		t.Fatal("finalization replay accepted")
	}
}

func TestProjectRPCActiveCopyRejectsDigestExpiryAndDisconnect(t *testing.T) {
	for _, mode := range []string{"digest", "expiry", "prune", "disconnect", "abort"} {
		t.Run(mode, func(t *testing.T) {
			handler, prepared, exact, now := activeProjectRPCFixture(t)
			switch mode {
			case "prune":
				*now = exact.ExpiresAt
				handler.pruneExpiredPlans()
			case "disconnect":
				handler.closePlans()
			case "abort":
				if _, err := handler.Handle(context.Background(), rpc.Call{OperationID: prepared.Plan.OperationID, Method: "project.copy.abort", Params: json.RawMessage(`{}`)}, nil); err != nil {
					t.Fatal(err)
				}
			default:
				digest := exact.Digest
				if mode == "digest" {
					digest = "wrong"
				} else {
					*now = exact.ExpiresAt
				}
				params, _ := json.Marshal(map[string]string{"digest": digest})
				if _, err := handler.Handle(context.Background(), rpc.Call{OperationID: prepared.Plan.OperationID, Method: "project.copy.finalize", Params: params}, nil); err == nil {
					t.Fatal("invalid finalization accepted")
				}
			}
			if len(handler.activeProjectCopies) != 0 || prepared.Project.ownerCopy.reservation != nil {
				t.Fatal("failed/disconnected transfer leaked active admission")
			}
			if _, err := prepared.Project.Store.GetReadOnly(context.Background(), prepared.Project.Record.ProjectID); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("aborted transfer registered project: %v", err)
			}
		})
	}
}
