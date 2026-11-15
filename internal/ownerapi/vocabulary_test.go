package ownerapi

import (
	"github.com/Subyard/Subyard/internal/rpc"
	"slices"
	"testing"
)

// Keep the query service's safe failure surface in sync with its public contract.
func TestOwnerQueryErrorCodes(t *testing.T) {
	var want []rpc.ErrorCode
	for _, code := range ErrorCodes() {
		want = append(want, rpc.ErrorCode(code))
	}
	got := rpc.ErrorCodesForGroup("owner-query")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("owner query error codes: got %v want %v", got, want)
	}
}
