package cli

import (
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
)

// The client maps this shared list to owner repair messages; it must name
// exactly the gate refusal codes the RPC handler can return.
func TestRPCGateErrorCodesFixtureMatchesGateRefusals(t *testing.T) {
	got := rpc.ErrorCodesForGroup("release-gate")
	want := []rpc.ErrorCode{rpcMutationGateFailedCode, rpcTransitionRequiredCode}
	for _, code := range releasetransition.OutcomeCodes() {
		if code == releasetransition.CodeReady {
			continue
		}
		outcome := releasetransition.Outcome{
			Status: releasetransition.StatusOperatorActionRequired, Code: code, Message: "refused",
		}
		want = append(want, mutationGateRPCError(&outcome, nil).Code)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("gate error code fixture drifted:\n got %v\nwant %v", got, want)
	}
}
