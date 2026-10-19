package releaseowner

import (
	"strings"
	"testing"
)

func TestPreparedFenceGenerationChainsExactPredecessor(t *testing.T) {
	session := strings.Repeat("a", 64)
	target := Target{Owner: "synthetic", Yard: "default", Project: "project", InstanceUUID: "uuid", Desired: strings.Repeat("b", 64), Source: strings.Repeat("c", 64)}
	initial, err := PrepareFenceRecipe(session, target, "0")
	if err != nil {
		t.Fatal(err)
	}
	again, err := PrepareFenceRecipe(session, target, "0")
	if err != nil || again != initial {
		t.Fatal("same inspected snapshot changed generation", err)
	}
	original, _ := ParseFenceRecipe(initial)
	successor := strings.Repeat("d", 64)
	replacement, err := PrepareFenceRecipe(session, target, successor)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseFenceRecipe(replacement)
	if err != nil || next.Expected != successor || next.Generation == original.Generation || next.Generation == successor {
		t.Fatal("changed predecessor restored old authority", next, err)
	}
}
