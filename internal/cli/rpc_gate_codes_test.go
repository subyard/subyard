package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// The client maps this shared list to owner repair messages; it must name
// exactly the gate refusal codes the RPC handler can return.
func TestRPCGateErrorCodesFixtureMatchesGateRefusals(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join(repositoryRoot(t), "api/yard-rpc/v1/gate-error-codes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Codes []string `json:"codes"`
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	want := []string{rpcMutationGateFailedCode, rpcTransitionRequiredCode}
	for _, code := range releasetransition.OutcomeCodes() {
		if code == releasetransition.CodeReady {
			continue
		}
		outcome := releasetransition.Outcome{
			Status: releasetransition.StatusOperatorActionRequired, Code: code, Message: "refused",
		}
		want = append(want, mutationGateRPCError(&outcome, nil).Code)
	}
	if !slices.Equal(fixture.Codes, want) {
		t.Fatalf("gate error code fixture drifted:\n got %v\nwant %v", fixture.Codes, want)
	}
}
