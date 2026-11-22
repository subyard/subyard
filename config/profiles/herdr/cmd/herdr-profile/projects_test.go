package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestClosedSpaceRequiresRestore(t *testing.T) {
	const id = "example"
	projects := map[string]project{id: {Name: "example", Folder: "/srv/workspaces/example/src"}}
	state := map[string]registration{id: {ID: spaceID(id)}}
	for _, restore := range []bool{false, true} {
		rows, _, changed, err := planSpaces(document{}, nil, projects, state, nil, "core", restore, nil)
		if err != nil || changed != restore || (len(rows) != 0) != restore {
			t.Fatalf("restore=%v: rows %v, changed %v, error %v", restore, rows, changed, err)
		}
	}
}

func TestRenameAndRemovalPreserveLayout(t *testing.T) {
	const id = "example"
	projectRow := project{Name: "before", Folder: "/srv/workspaces/example/src"}
	for _, test := range []struct {
		name, tabs   string
		live, remove bool
	}{
		{"empty", `[]`, false, true},
		{"layout", `[{"id":"tab","future":{"x":1}}]`, false, false},
		{"live", `[]`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := projectSpace(id, projectRow)
			row["tabs"] = json.RawMessage(test.tabs)
			value := jsonValue(map[string]any{"spaces": []any{row}})
			_, spaces, err := loadLayout(value)
			if err != nil {
				t.Fatal(err)
			}
			state := map[string]registration{}
			projects := map[string]project{id: {Name: "after", Folder: projectRow.Folder}}
			rows, _, changed, err := planSpaces(document{}, spaces, projects, state, nil, "core", false, nil)
			if err != nil || !changed || len(rows) != 1 || string(rows[0]["name"]) != `"after"` || string(rows[0]["tabs"]) != test.tabs {
				t.Fatalf("rename: %v %v %v", rows, changed, err)
			}
			_, spaces, err = loadLayout(jsonValue(map[string]any{"spaces": rows}))
			if err != nil {
				t.Fatal(err)
			}
			rows, _, changed, err = planSpaces(document{}, spaces, nil, state, map[string]bool{spaceID(id): test.live}, "core", false, nil)
			if err != nil || changed != test.remove || (len(rows) == 0) != test.remove || (state[id].Pending == "") != test.remove {
				t.Fatalf("retire: %v %v %v, pending %q", rows, changed, err, state[id].Pending)
			}
		})
	}
}

func TestSyncRetriesCASConflict(t *testing.T) {
	root, stateDir := testkit.TempDir(t), testkit.TempDir(t)
	workspace := filepath.Join(root, "example")
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(workspace, ".subyard-meta.json"), []byte(`{"schema":1,"projectId":"example","name":"Example"}`), 0o600)
	state := map[string]registration{}
	projects, err := projectInventory(root, state)
	if err != nil || len(projects) != 1 {
		t.Fatalf("inventory: %v %v", projects, err)
	}
	socket := fakeCore(t,
		`"result":{"value":null,"version":0}`,
		`"error":{"data":{"kind":"conflict"}}`,
		`"result":{"value":{"spaces":[{"id":"other","tabs":[]}],"future":true},"version":1}`,
		`"result":{"version":2}`,
	)
	if err := syncLayout(socket, root, stateDir, "core", projects, state, false); err != nil {
		t.Fatal(err)
	}
	state, err = loadRegistrations(stateDir)
	if err != nil || state["example"].ID != spaceID("example") || state["example"].Creating != nil {
		t.Fatalf("state after conflict: %+v, %v", state, err)
	}
}
