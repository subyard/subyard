package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestClosedSpaceRequiresRestore(t *testing.T) {
	const id = "example"
	projects := map[string]project{id: {Name: "example", Folder: "/srv/workspaces/example/src"}}
	state := map[string]registration{id: {ID: spaceID(id)}}
	for _, restore := range []bool{false, true} {
		rows, changed, err := planSpaces(nil, projects, state, nil, restore)
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
			rows, changed, err := planSpaces(spaces, projects, state, nil, false)
			if err != nil || !changed || len(rows) != 1 || string(rows[0]["name"]) != `"after"` || string(rows[0]["tabs"]) != test.tabs {
				t.Fatalf("rename: %v %v %v", rows, changed, err)
			}
			_, spaces, err = loadLayout(jsonValue(map[string]any{"spaces": rows}))
			if err != nil {
				t.Fatal(err)
			}
			rows, changed, err = planSpaces(spaces, nil, state, map[string]bool{spaceID(id): test.live}, false)
			if err != nil || changed != test.remove || (len(rows) == 0) != test.remove || (state[id].Pending == "") != test.remove {
				t.Fatalf("retire: %v %v %v, pending %q", rows, changed, err, state[id].Pending)
			}
			if test.remove {
				rows, changed, err = planSpaces(nil, projects, state, nil, false)
				if err != nil || !changed || len(rows) != 1 {
					t.Fatalf("re-added project requires restore: %v %v %v", rows, changed, err)
				}
			}
		})
	}
}

func TestSyncRetriesCASConflict(t *testing.T) {
	root, stateDir := projectFixture(t), testkit.TempDir(t)
	state := map[string]registration{}
	projects, err := projectInventory(root, state)
	if err != nil || len(projects) != 1 {
		t.Fatalf("inventory: %v %v", projects, err)
	}
	const empty = `"result":{"value":null,"version":0}`
	const conflict = `"error":{"data":{"kind":"conflict"}}`
	socket := fakeCore(t, empty, conflict, empty, conflict, empty, conflict, empty, conflict)
	if err := syncLayout(socket, root, stateDir, projects, state, false); err == nil || !strings.Contains(err.Error(), "retry sync") {
		t.Fatalf("conflict exhaustion: %v", err)
	}
	state, err = loadRegistrations(stateDir)
	if err != nil || len(state) != 0 {
		t.Fatalf("failed CAS recorded a creation: %+v, %v", state, err)
	}
	socket = fakeCore(t, `"result":{"value":{"spaces":[{"id":"other","tabs":[]}],"future":true},"version":1}`, `"result":{"version":2}`)
	if err := syncLayout(socket, root, stateDir, projects, state, false); err != nil {
		t.Fatal(err)
	}
	state, err = loadRegistrations(stateDir)
	if err != nil || state["example"].ID != spaceID("example") {
		t.Fatalf("state after retry: %+v, %v", state, err)
	}
}

func projectFixture(t *testing.T) string {
	t.Helper()
	root := testkit.TempDir(t)
	workspace := filepath.Join(root, "example")
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(workspace, ".subyard-meta.json"), []byte(`{"schema":1,"projectId":"example","name":"Example"}`), 0o600)
	return root
}

func TestRegistrationReadinessDetectsMissedHook(t *testing.T) {
	t.Setenv("HOME", testkit.TempDir(t))
	root := projectFixture(t)
	socket := fakeCore(t, `"result":{"value":null,"version":0}`)
	if err := registrationReady(socket, root); err == nil || !strings.Contains(err.Error(), "needs reconciliation") {
		t.Fatalf("missing registration appeared ready: %v", err)
	}
	dir, _ := stateDir()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("readiness wrote state: %v", err)
	}
}

func TestSyncReportsIndependentRefusals(t *testing.T) {
	t.Setenv("HOME", testkit.TempDir(t))
	root := projectFixture(t)
	testkit.WriteFile(t, filepath.Join(root, "example", ".subyard-meta.json"), []byte(`invalid`), 0o600)
	err := syncProjects(filepath.Join(root, "absent.sock"), root, false, false)
	if err == nil || !strings.Contains(err.Error(), "Core is stopped") || !strings.Contains(err.Error(), "invalid project metadata") {
		t.Fatalf("independent refusals: %v", err)
	}
}

func TestProjectLockUsesExactPermissions(t *testing.T) {
	dir := testkit.TempDir(t)
	old := syscall.Umask(0o200)
	defer syscall.Umask(old)
	for range 2 {
		lock, err := openProjectLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		info, err := lock.Stat()
		lock.Release()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("project lock permissions: %v %v", info, err)
		}
	}
}
