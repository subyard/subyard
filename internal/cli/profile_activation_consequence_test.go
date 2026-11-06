package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestRuntimeConsequencesNameYardsOnceWithinTextBound(t *testing.T) {
	order := []string{"service restarts", "worker restarts"}
	got := runtimeConsequences(order, map[string][]string{"service restarts": {"alpha", "beta"}, "worker restarts": {"gamma"}})
	if !slices.Equal(got, []string{"yards alpha, beta: service restarts", "yard gamma: worker restarts"}) {
		t.Fatalf("consequences = %q", got)
	}
	many := []string{}
	for range 40 {
		many = append(many, strings.Repeat("y", 20))
	}
	got = runtimeConsequences([]string{"service restarts"}, map[string][]string{"service restarts": many})
	if !slices.Equal(got, []string{"40 yards: service restarts"}) {
		t.Fatalf("bounded consequences = %q", got)
	}
	if got := runtimeConsequences(nil, nil); len(got) != 0 {
		t.Fatalf("empty consequences = %q", got)
	}
}
