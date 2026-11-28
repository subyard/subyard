package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func desired(checkouts map[string]checkout, gone ...string) inventory {
	result := inventory{Checkouts: checkouts, Gone: make(map[string]bool)}
	if result.Checkouts == nil {
		result.Checkouts = make(map[string]checkout)
	}
	for _, key := range gone {
		result.Gone[key] = true
	}
	return result
}

func TestClosedSpaceRequiresRestore(t *testing.T) {
	const id = "example"
	for _, key := range []string{id, id + "/src/private"} {
		checkouts := map[string]checkout{key: {Name: "example", Folder: "/srv/workspaces/" + key}}
		state := map[string]registration{key: {ID: spaceID(key)}}
		for _, restore := range []bool{false, true} {
			rows, changed, err := planSpaces(nil, desired(checkouts), state, nil, restore)
			if err != nil || changed != restore || (len(rows) != 0) != restore {
				t.Fatalf("%s restore=%v: rows %v, changed %v, error %v", key, restore, rows, changed, err)
			}
		}
	}
}

func TestRenameAndRemovalPreserveLayout(t *testing.T) {
	const id = "example"
	before := checkout{Name: "before", Folder: "/srv/workspaces/example/src"}
	for _, test := range []struct {
		name, tabs   string
		live, remove bool
	}{
		{"empty", `[]`, false, true},
		{"layout", `[{"id":"tab","future":{"x":1}}]`, false, false},
		{"live", `[]`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := checkoutSpace(id, before)
			row["tabs"] = json.RawMessage(test.tabs)
			row["future"] = json.RawMessage(`{"revision":1}`)
			value := jsonValue(map[string]any{"spaces": []any{row}})
			_, spaces, err := loadLayout(value)
			if err != nil {
				t.Fatal(err)
			}
			state := map[string]registration{}
			after := map[string]checkout{id: {Name: "after", Folder: before.Folder}}
			rows, changed, err := planSpaces(spaces, desired(after), state, nil, false)
			if err != nil || !changed || len(rows) != 1 || string(rows[0]["name"]) != `"after"` ||
				string(rows[0]["tabs"]) != test.tabs || string(rows[0]["future"]) != `{"revision":1}` {
				t.Fatalf("rename: %v %v %v", rows, changed, err)
			}
			_, spaces, err = loadLayout(jsonValue(map[string]any{"spaces": rows}))
			if err != nil {
				t.Fatal(err)
			}
			rows, changed, err = planSpaces(spaces, desired(nil, id), state, map[string]bool{spaceID(id): test.live}, false)
			if err != nil || changed != test.remove || (len(rows) == 0) != test.remove || (state[id].Pending == "") != test.remove {
				t.Fatalf("retire: %v %v %v, pending %q", rows, changed, err, state[id].Pending)
			}
			if test.remove {
				rows, changed, err = planSpaces(nil, desired(after), state, nil, false)
				if err != nil || !changed || len(rows) != 1 {
					t.Fatalf("re-added project requires restore: %v %v %v", rows, changed, err)
				}
			}
		})
	}
}

// Nested checkout spaces go with their layout once absence is proven and no
// terminal is live; anything else keeps the space. The project space keeps its
// layout as before.
func TestCheckoutRemovalMatrix(t *testing.T) {
	const id, nested = "example", "example/src/private"
	project := checkout{Name: "Example", Folder: "/srv/workspaces/example/src"}
	child := checkout{Name: "Example/private", Folder: "/srv/workspaces/example/src/private"}
	layout, live := "user layout", "live terminals"
	for _, test := range []struct {
		name      string
		inventory inventory
		live      bool
		pending   map[string]string
	}{
		{"present", desired(map[string]checkout{id: project, nested: child}), false, map[string]string{id: "", nested: ""}},
		{"absent", desired(map[string]checkout{id: project}, nested), false, map[string]string{id: ""}},
		{"absent with live terminal", desired(map[string]checkout{id: project}, nested), true, map[string]string{id: "", nested: live}},
		{"lost .git or unproven", desired(map[string]checkout{id: project}), false, map[string]string{id: "", nested: ""}},
		{"project gone", desired(nil, id, nested), false, map[string]string{id: layout}},
		{"project gone with live terminals", desired(nil, id, nested), true, map[string]string{id: layout, nested: live}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var rows []any
			for key, value := range map[string]checkout{id: project, nested: child} {
				row := checkoutSpace(key, value)
				row["tabs"] = json.RawMessage(`[{"id":"tab"}]`)
				rows = append(rows, row)
			}
			_, spaces, err := loadLayout(jsonValue(map[string]any{"spaces": rows}))
			if err != nil {
				t.Fatal(err)
			}
			state := map[string]registration{id: {ID: spaceID(id)}, nested: {ID: spaceID(nested)}}
			terminals := map[string]bool{spaceID(nested): test.live, spaceID(id): test.live}
			planned, _, err := planSpaces(spaces, test.inventory, state, terminals, false)
			if err != nil {
				t.Fatal(err)
			}
			kept := make(map[string]string)
			for _, row := range planned {
				var owner ownership
				if json.Unmarshal(row["subyard"], &owner) != nil || string(row["tabs"]) != `[{"id":"tab"}]` {
					t.Fatalf("kept space lost its ownership or layout: %v", row)
				}
				kept[owner.key()] = strings.TrimPrefix(state[owner.key()].Pending, "space has ")
				kept[owner.key()] = strings.TrimPrefix(kept[owner.key()], "space contains ")
			}
			if !maps.Equal(kept, test.pending) || len(state) != len(test.pending) {
				t.Fatalf("kept %v with state %v, want %v", kept, state, test.pending)
			}
		})
	}
}

func TestSyncRetriesCASConflict(t *testing.T) {
	useLibrary(t)
	root, stateDir := projectFixture(t), testkit.TempDir(t)
	state := map[string]registration{}
	const empty = `"result":{"value":null,"version":0}`
	const conflict = `"error":{"data":{"kind":"conflict"}}`
	socket := fakeCore(t, empty, conflict, empty, conflict, empty, conflict, empty, conflict)
	if err := syncLayout(socket, root, stateDir, state, false); err == nil || !strings.Contains(err.Error(), "retry sync") {
		t.Fatalf("conflict exhaustion: %v", err)
	}
	state, err := loadRegistrations(stateDir)
	if err != nil || len(state) != 0 {
		t.Fatalf("failed CAS recorded a creation: %+v, %v", state, err)
	}
	socket = fakeCore(t, `"result":{"value":{"spaces":[{"id":"other","tabs":[]}],"future":true},"version":1}`, `"result":{"version":2}`)
	if err := syncLayout(socket, root, stateDir, state, false); err != nil {
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
	useLibrary(t)
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
	useLibrary(t)
	t.Setenv("HOME", testkit.TempDir(t))
	root := projectFixture(t)
	testkit.WriteFile(t, filepath.Join(root, "example", ".subyard-meta.json"), []byte(`invalid`), 0o600)
	err := syncProjects(filepath.Join(root, "absent.sock"), root, false, false)
	if err == nil || err.Error() != "Core is stopped. Run yard herdr up first.; checkout discovery: Invalid project metadata: example" {
		t.Fatalf("independent refusals: %v", err)
	}
	// With Core running, unreadable state still reports the inventory refusal.
	herdr := &fakeHerdr{}
	socket := herdr.serve(t)
	dir, _ := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "projects.json"), []byte(`invalid`), 0o600)
	err = syncProjects(socket, root, false, false)
	if err == nil || err.Error() != "checkout discovery: Invalid project metadata: example; invalid Herdr registration state" {
		t.Fatalf("independent refusals with Core running: %v", err)
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
