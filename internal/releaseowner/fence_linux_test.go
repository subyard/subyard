package releaseowner

import (
	"encoding/json"
	"reflect"

	"github.com/Subyard/Subyard/internal/ports"
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

func TestPreparedFenceVectorPreservesExactResourceRoles(t *testing.T) {
	session := strings.Repeat("a", 64)
	target := Target{Owner: "host-power", Yard: "default", Project: "host", InstanceUUID: "host", Desired: strings.Repeat("b", 64), Source: strings.Repeat("c", 64)}
	for _, roles := range [][]string{{"engine", "service"}, {"engine", "service", "publication"}} {
		t.Run(roles[len(roles)-1], func(t *testing.T) {
			expected := []ports.NativeResourceGeneration{{Role: "engine", Value: "0"}, {Role: "service", Value: strings.Repeat("b", 64)}}
			if len(roles) == 3 {
				expected = append(expected, ports.NativeResourceGeneration{Role: "publication", Value: strings.Repeat("c", 64)})
			}
			raw, err := PrepareFenceVectorRecipe(session, target, expected)
			if err != nil {
				t.Fatal("original host-power vector refused", err)
			}
			original, err := ParseFenceVectorRecipe(raw)
			canonical, _ := json.Marshal(original)
			if err != nil || string(canonical) != raw || !reflect.DeepEqual(original.Expected, expected) {
				t.Fatal("original vector changed", err)
			}
			current := append([]ports.NativeResourceGeneration(nil), expected...)
			successor := strings.Repeat("d", 64)
			for i := range current {
				current[i].Value = successor
			}
			replacement, err := PrepareFenceVectorRecipe(session, target, current)
			if err != nil {
				t.Fatal(err)
			}
			next, err := ParseFenceVectorRecipe(replacement)
			if err != nil || !reflect.DeepEqual(next.Expected, current) || next.Generation == original.Generation {
				t.Fatal("replacement lost original role order", err)
			}
			target.Before = raw
			observed, err := fenceObserved(target, successor)
			var fact struct {
				Expected, Current             []ports.NativeResourceGeneration
				Generation, Successor, Source string
			}
			if err != nil || json.Unmarshal([]byte(observed), &fact) != nil || !reflect.DeepEqual(fact.Expected, expected) || !reflect.DeepEqual(fact.Current, current) || fact.Generation != original.Generation || fact.Successor != successor || fact.Source != target.Source {
				t.Fatal("fence observation lost original generations", err)
			}
			for _, invalid := range []string{original.Generation, expected[1].Value, "invalid"} {
				if _, err := fenceObserved(target, invalid); err == nil {
					t.Fatal("invalid successor admitted")
				}
			}
			if _, err := ParseFenceVectorRecipe(raw + "\n"); err == nil {
				t.Fatal("noncanonical vector admitted")
			}
		})
	}
	for _, roles := range [][]string{{"engine", "service", "timer"}, {"engine", "service", "unknown"}, {"service", "engine"}} {
		expected := make([]ports.NativeResourceGeneration, len(roles))
		for i, role := range roles {
			expected[i] = ports.NativeResourceGeneration{Role: role, Value: "0"}
		}
		if _, err := PrepareFenceVectorRecipe(session, target, expected); err == nil {
			t.Fatal("foreign or reordered roles admitted", roles)
		}
	}
	for _, invalid := range []FenceVectorRecipe{
		{2, []ports.NativeResourceGeneration{{Role: "engine", Value: "invalid"}, {Role: "service", Value: "0"}}, strings.Repeat("a", 64)},
		{2, []ports.NativeResourceGeneration{{Role: "engine", Value: "0"}, {Role: "service", Value: "0"}}, "invalid"},
	} {
		raw, _ := json.Marshal(invalid)
		if _, err := ParseFenceVectorRecipe(string(raw)); err == nil {
			t.Fatal("invalid generation admitted")
		}
	}
}
