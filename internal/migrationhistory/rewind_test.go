package migrationhistory_test

import (
	"reflect"
	"testing"

	mh "github.com/Subyard/Subyard/internal/migrationhistory"
)

func TestRewindIndependentStateModelCorpus(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		migrations := corpusMigrations(dependent)
		owner := registry(t, migrations)
		for mask := 0; mask < 1<<len(migrations); mask++ {
			states, valid := oracle(migrations, mask)
			if !valid {
				continue
			}
			authority, err := owner.History(states)
			if err != nil {
				t.Fatal(err)
			}
			for index, migration := range migrations {
				expected, validAfter := oracle(migrations, mask & ^(1<<index))
				expectedAllowed := mask&(1<<index) != 0 && validAfter
				edge, err := owner.Rewind(authority, migration.ID)
				if (err == nil) != expectedAllowed {
					t.Fatalf("dependent=%t mask=%03b migration=%s allowed=%t err=%v", dependent, mask, migration.ID, expectedAllowed, err)
				}
				if err != nil {
					continue
				}
				if !reflect.DeepEqual(edge.After().Domains(), expected) || owner.CheckReverseEdgeBinding(edge) != nil {
					t.Fatal("rewind lost exact frontier or binding")
				}
				foreign := registry(t, migrations)
				if _, err := foreign.Rewind(authority, migration.ID); err == nil {
					t.Fatal("foreign authority authorized rewind")
				}
				if err := foreign.CheckReverseEdgeBinding(edge); err != nil {
					t.Fatalf("identical semantic binding rejected: %v", err)
				}
			}
		}
	}
}

func TestRewindPathRequiresClosedCutAndReversesDependencies(t *testing.T) {
	owner := registry(t, corpusMigrations(true))
	states, _ := oracle(corpusMigrations(true), 7)
	authority, err := owner.History(states)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := owner.RewindPath(authority, map[string]int{"a": 1, "b": 1})
	if err != nil || len(edges) != 3 || edges[0].Migration().ID != "a3" || edges[1].Migration().ID != "b2" || edges[2].Migration().ID != "a2" {
		t.Fatalf("reverse path = %+v, %v", edges, err)
	}
	for _, target := range []map[string]int{{"a": 3, "b": 1}, {"a": 1}, {"a": 4, "b": 2}, {"a": 0, "b": 1}} {
		if _, err := owner.RewindPath(authority, target); err == nil {
			t.Fatalf("invalid target accepted: %v", target)
		}
	}
	if owner.CheckReverseEdgeBinding(mh.ReverseEdge{}) == nil {
		t.Fatal("zero reverse edge accepted")
	}
	edges[0].Migration().DependsOn[0] = "changed"
	if edges[0].Migration().DependsOn[0] != "b2" {
		t.Fatal("reverse handle leaked mutable definitions")
	}
}
