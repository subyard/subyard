package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// useLibrary runs the shared checkout discovery library from this checkout.
func useLibrary(t *testing.T) {
	t.Helper()
	library, err := filepath.Abs("../../../../../scripts/lib/checkout-discovery/checkouts.py")
	if err != nil {
		t.Fatal(err)
	}
	old := discoveryProgram
	discoveryProgram = []string{"python3", "-B", "-E", "-s", library}
	t.Cleanup(func() { discoveryProgram = old })
}

// fakeDiscovery answers every discovery call with report and records its input.
func fakeDiscovery(t *testing.T, report string) string {
	t.Helper()
	dir := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(dir, "report"), []byte(report), 0o600)
	testkit.WriteFile(t, filepath.Join(dir, "discovery"), []byte("#!/bin/sh\ncat > \"$0.input\"\ncat \"${0%/*}/report\"\n"), 0o700)
	old := discoveryProgram
	discoveryProgram = []string{filepath.Join(dir, "discovery")}
	t.Cleanup(func() { discoveryProgram = old })
	return filepath.Join(dir, "discovery.input")
}

func TestScanCheckoutsRefusalsAndProvenAbsence(t *testing.T) {
	workspaces := testkit.TempDir(t)
	for _, name := range []string{"kept", "lost"} {
		if err := os.MkdirAll(filepath.Join(workspaces, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(workspaces, "file"), nil, 0o600)
	w := func(path string) string { return filepath.Join(workspaces, path) }
	project := fmt.Sprintf(`{"projectId":"kept","name":"Kept","root":%q,"roots":[{"path":%q,"kind":"folder"},{"path":%q,"kind":"git"},{"path":%q,"kind":"git"}]}`,
		w("kept/src"), w("kept/src"), w("kept/src/a/b"), w("kept/wt-x"))
	report := fmt.Sprintf(`{"projects":[%s,{"projectId":"bare","name":"Bare","root":%q,"roots":[]}],"absent":[%q,%q],"errors":[]}`,
		project, w("bare/src"), w("gone"), w("kept/src/old"))
	input := fakeDiscovery(t, report)
	known := []string{"bare", "file", "gone", "gone/wt-y", "kept/src/old", "kept/wt-x", "lost"}
	result, err := scanCheckouts(workspaces, "/state", known)
	want := `invalid project metadata for "lost"; project "bare" has no canonical checkout directory; workspace "file" is no longer a directory`
	if err != nil || result.refusal() == nil || result.refusal().Error() != want {
		t.Fatalf("refusals: %v, %v", err, result.refusal())
	}
	if fmt.Sprint(result.Kept) != "map[bare:true file:true lost:true]" || result.Partial || !result.retains("lost/wt-z") || result.retains("kept/wt-z") {
		t.Fatalf("kept: %v", result.Kept)
	}
	if !result.Gone["gone"] || !result.Gone["kept/src/old"] || result.Gone["gone/wt-y"] || len(result.Gone) != 2 {
		t.Fatalf("gone: %v", result.Gone)
	}
	checkouts := map[string]checkout{
		"kept": {"Kept", w("kept/src")}, "kept/src/a/b": {"Kept/a/b", w("kept/src/a/b")}, "kept/wt-x": {"Kept/wt-x", w("kept/wt-x")},
	}
	if fmt.Sprint(result.Checkouts) != fmt.Sprint(checkouts) {
		t.Fatalf("checkouts: %v", result.Checkouts)
	}
	data, err := os.ReadFile(input)
	if err != nil || !strings.Contains(string(data), w("gone/wt-y")) || !strings.Contains(string(data), w("bare")) {
		t.Fatalf("known folders: %s, %v", data, err)
	}

	// Scan errors keep present checkouts but prove no absence and forget no record.
	fakeDiscovery(t, fmt.Sprintf(`{"projects":[{"projectId":"kept","name":"Kept","root":%q,"roots":[{"path":%q,"kind":"folder"},{"path":%q,"kind":"unknown"},{"path":%q,"kind":"git"}]}],"errors":["b failed","a failed"],"absent":[%q]}`,
		w("kept/src"), w("kept/src"), w("kept/wt-stale"), w("kept/wt-x"), w("gone")))
	result, err = scanCheckouts(workspaces, "/state", known)
	if err != nil || result.refusal() == nil || result.refusal().Error() != "checkout discovery: a failed; checkout discovery: b failed" {
		t.Fatalf("scan errors: %v, %v", err, result.refusal())
	}
	if !result.Partial || len(result.Gone) != 0 || len(result.Checkouts) != 2 || result.Checkouts["kept/wt-x"].Folder != w("kept/wt-x") || !result.retains("kept/wt-stale") {
		t.Fatalf("partial scan: %+v", result)
	}
	invalidRoot := func(path, kind string) string {
		return fmt.Sprintf(`{"projects":[{"projectId":"kept","root":%q,"roots":[{"path":%q,"kind":"folder"},{"path":%q,"kind":%q}]}]}`,
			w("kept/src"), w("kept/src"), path, kind)
	}
	for _, invalid := range []string{`not json`, `{}`} {
		fakeDiscovery(t, invalid)
		if _, err := scanCheckouts(workspaces, "/state", nil); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("invalid report %s: %v", invalid, err)
		}
	}
	for _, invalid := range []string{
		invalidRoot(workspaces+"/kept/src/../x", "git"), invalidRoot(w("kept/a/b"), "git"),
		invalidRoot(w("kept/src/a"), "folder"), invalidRoot(w("kept/src/a"), "unknown"), invalidRoot(w("other/src/a"), "git"),
	} {
		fakeDiscovery(t, invalid)
		result, err := scanCheckouts(workspaces, "/state", nil)
		if err != nil || result.refusal() == nil || !strings.Contains(result.refusal().Error(), "invalid root") || !result.Kept["kept"] {
			t.Fatalf("invalid root %s: %v, %+v", invalid, err, result)
		}
	}
}

func TestRegistrationStateStaysReadableByEarlierReleases(t *testing.T) {
	dir := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(dir, "projects.json"), []byte(fmt.Sprintf(`{"example":{"id":%q,"pending":"space has live terminals"}}`, spaceID("example"))), 0o600)
	state, err := loadRegistrations(dir)
	if err != nil || state["example"].Pending != "space has live terminals" || len(state) != 1 {
		t.Fatalf("earlier state: %v %v", state, err)
	}
	state = map[string]registration{
		"example/src/private": {ID: spaceID("example/src/private")}, "other": {ID: spaceID("other")},
		"other/wt-x": {ID: spaceID("other/wt-x"), Pending: "space has live terminals"},
	}
	if err := saveRegistrations(dir, state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The earlier release reads every project record with its own identity rule.
	var earlier map[string]struct{ ID, Pending string }
	if json.Unmarshal(data, &earlier) != nil || len(earlier) != 2 || earlier["example"].ID != spaceID("example") || earlier["other"].ID != spaceID("other") {
		t.Fatalf("earlier reader: %s", data)
	}
	loaded, err := loadRegistrations(dir)
	if err != nil || fmt.Sprint(loaded) != fmt.Sprint(state) {
		t.Fatalf("round trip: %v, %v", loaded, err)
	}
	for _, invalid := range []string{
		fmt.Sprintf(`{"example":{"id":%q,"checkouts":{"src":{"id":%q}}}}`, spaceID("example"), spaceID("example/src")),
		fmt.Sprintf(`{"example":{"id":%q,"checkouts":{"../x":{"id":%q}}}}`, spaceID("example"), spaceID("example/../x")),
		fmt.Sprintf(`{"example":{"id":%q,"checkouts":{"wt-x":{"id":%q}}}}`, spaceID("example"), spaceID("example")),
	} {
		testkit.WriteFile(t, filepath.Join(dir, "projects.json"), []byte(invalid), 0o600)
		if _, err := loadRegistrations(dir); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

// fakeHerdr serves the Core RPCs that project sync uses over a stored document.
type fakeHerdr struct {
	mu      sync.Mutex
	value   json.RawMessage
	version uint64
	live    []string
}

func (herdr *fakeHerdr) serve(t *testing.T) string {
	t.Helper()
	path := filepath.Join(testkit.TempDir(t), "core.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	herdr.value = json.RawMessage("null")
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			herdr.answer(conn)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return path
}

func (herdr *fakeHerdr) answer(conn net.Conn) {
	defer conn.Close()
	var request struct {
		Method string
		Params struct {
			Value   json.RawMessage
			Version uint64
		}
	}
	if json.NewDecoder(conn).Decode(&request) != nil {
		return
	}
	herdr.mu.Lock()
	defer herdr.mu.Unlock()
	var result any
	switch request.Method {
	case "hello":
		result = map[string]int{"generation": generation}
	case "server.status":
		result = map[string]any{"core": map[string]string{"id": "fixture"}, "pid": 1, "version": "0.1.22"}
	case "doc.get":
		result = map[string]any{"value": herdr.value, "version": herdr.version}
	case "doc.set":
		if request.Params.Version != herdr.version {
			fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":1,"error":{"data":{"kind":"conflict"}}}`)
			return
		}
		herdr.value, herdr.version = request.Params.Value, herdr.version+1
		result = map[string]uint64{"version": herdr.version}
	case "events.subscribe":
		terminals := []map[string]any{}
		for index, id := range herdr.live {
			terminals = append(terminals, map[string]any{"address": fmt.Sprint("t", index), "state": "live", "labels": map[string]string{"herdr.space": id}})
		}
		result = map[string]any{"terminals": terminals}
	}
	encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	fmt.Fprintf(conn, "%s\n", encoded)
}

// spaces returns key -> name, tabs for every owned space in the document.
func (herdr *fakeHerdr) spaces(t *testing.T) map[string]string {
	t.Helper()
	herdr.mu.Lock()
	defer herdr.mu.Unlock()
	_, rows, err := loadLayout(herdr.value)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]string)
	for _, row := range rows {
		if row.owned() {
			result[row.Subyard.key()] = row.Name + " " + string(row.fields["tabs"])
		}
	}
	return result
}

func (herdr *fakeHerdr) setTabs(t *testing.T, key, tabs string) {
	t.Helper()
	herdr.mu.Lock()
	defer herdr.mu.Unlock()
	fields, rows, err := loadLayout(herdr.value)
	if err != nil {
		t.Fatal(err)
	}
	updated := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if row.ID == spaceID(key) {
			row.fields["tabs"] = json.RawMessage(tabs)
		}
		updated = append(updated, row.fields)
	}
	fields["spaces"] = jsonValue(updated)
	herdr.value, herdr.version = jsonValue(fields), herdr.version+1
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	command.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "GIT_") })
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestNestedCheckoutsAndWorktreesFollowTheirFolders(t *testing.T) {
	useLibrary(t)
	t.Setenv("HOME", testkit.TempDir(t))
	// Discovery fixtures live under /tmp, outside any managed workspace tree.
	workspaces, err := os.MkdirTemp("/tmp", "subyard-herdr-checkouts.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(workspaces) })
	workspaces, _ = filepath.EvalSymlinks(workspaces)
	project := filepath.Join(workspaces, "example")
	src := filepath.Join(project, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(project, ".subyard-meta.json"), []byte(`{"schema":1,"projectId":"example","name":"Example"}`), 0o600)
	git(t, src, "init", "-q")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "fixture")
	git(t, src, "worktree", "add", "-q", filepath.Join(project, "wt-feature"))
	git(t, src, "worktree", "add", "-q", filepath.Join(src, "linked"))
	for _, dir := range []string{filepath.Join(src, "private"), filepath.Join(project, "wt-feature", "private")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "init", "-q")
	}
	herdr := &fakeHerdr{}
	socket := herdr.serve(t)
	if err := discoverCheckouts(socket, workspaces); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"example": "Example []", "example/src/linked": "Example/linked []",
		"example/src/private": "Example/private []", "example/wt-feature": "Example/wt-feature []",
	}
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("spaces: %v", got)
	}
	if err := registrationReady(socket, workspaces); err != nil {
		t.Fatal(err)
	}

	// A removed worktree goes with its layout; a live nested checkout stays pending.
	herdr.setTabs(t, "example/wt-feature", `[{"id":"tab"}]`)
	git(t, src, "worktree", "remove", "--force", filepath.Join(project, "wt-feature"))
	if err := os.RemoveAll(filepath.Join(src, "private")); err != nil {
		t.Fatal(err)
	}
	// Losing .git is no proof of removal.
	if err := os.Rename(filepath.Join(src, "linked", ".git"), filepath.Join(src, "linked.git")); err != nil {
		t.Fatal(err)
	}
	herdr.live = []string{spaceID("example/src/private")}
	if err := syncProjects(socket, workspaces, false, true); err != nil {
		t.Fatal(err)
	}
	delete(want, "example/wt-feature")
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after removal: %v", got)
	}
	dir, _ := stateDir()
	state, err := loadRegistrations(dir)
	if err != nil || state["example/src/private"].Pending != "space has live terminals" || state["example/src/linked"].ID == "" {
		t.Fatalf("state: %v, %v", state, err)
	}
	herdr.live = nil
	if err := syncProjects(socket, workspaces, false, true); err != nil {
		t.Fatal(err)
	}
	delete(want, "example/src/private")
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after terminals closed: %v", got)
	}
	if err := registrationReady(socket, workspaces); err != nil {
		t.Fatal(err)
	}

	// An unverifiable sibling leaves the other checkouts reconciled and retires nothing.
	stale := filepath.Join(project, "wt-stale")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(stale, ".git"), []byte("gitdir: /nonexistent\n"), 0o644)
	git(t, src, "worktree", "add", "-q", filepath.Join(project, "wt-extra"))
	if err := os.RemoveAll(filepath.Join(src, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := discoverCheckouts(socket, workspaces); err == nil || !strings.Contains(err.Error(), "could not be verified: "+stale) {
		t.Fatalf("stale sibling: %v", err)
	}
	want["example/wt-extra"] = "Example/wt-extra []"
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("partial scan: %v", got)
	}
	if err := registrationReady(socket, workspaces); err == nil || !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("partial readiness: %v", err)
	}
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	if err := syncProjects(socket, workspaces, false, true); err != nil {
		t.Fatal(err)
	}
	delete(want, "example/src/linked")
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after the stale sibling went: %v", got)
	}

	// An unsafe discovery state refuses and changes nothing.
	if err := os.RemoveAll(filepath.Join(dir, "discovery")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workspaces, filepath.Join(dir, "discovery")); err != nil {
		t.Fatal(err)
	}
	if err := syncProjects(socket, workspaces, false, true); err == nil || !strings.Contains(err.Error(), "Discovery state") {
		t.Fatalf("unsafe state: %v", err)
	}
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after refusal: %v", got)
	}
	if err := os.Remove(filepath.Join(dir, "discovery")); err != nil {
		t.Fatal(err)
	}

	// A rollback that dropped checkout records still retires a removed checkout in one sync.
	git(t, src, "worktree", "add", "-q", filepath.Join(project, "wt-next"))
	if err := syncProjects(socket, workspaces, false, true); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "projects.json"), []byte(fmt.Sprintf(`{"example":{"id":%q}}`, spaceID("example"))), 0o600)
	git(t, src, "worktree", "remove", "--force", filepath.Join(project, "wt-next"))
	if err := syncProjects(socket, workspaces, false, true); err != nil {
		t.Fatal(err)
	}
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after rollback: %v", got)
	}
	if err := registrationReady(socket, workspaces); err != nil {
		t.Fatal(err)
	}

	// A missing src removes nothing; a removed project's spaces go with it.
	moved := filepath.Join(project, "src.moved")
	if err := os.Rename(src, moved); err != nil {
		t.Fatal(err)
	}
	if err := syncProjects(socket, workspaces, false, true); err == nil || !strings.Contains(err.Error(), "no canonical checkout directory") {
		t.Fatalf("missing src: %v", err)
	}
	if got := herdr.spaces(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after a missing src: %v", got)
	}
	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	if err := syncProjects(socket, workspaces, false, true); err != nil {
		t.Fatal(err)
	}
	if got := herdr.spaces(t); len(got) != 0 {
		t.Fatalf("after project removal: %v", got)
	}
}

// Readiness right after up must not race the timer, which adds nested checkouts
// once its scan caches them.
func TestReadinessLeavesNewNestedCheckoutsToTheTimer(t *testing.T) {
	t.Setenv("HOME", testkit.TempDir(t))
	workspaces := testkit.TempDir(t)
	src := filepath.Join(workspaces, "example", "src")
	fakeDiscovery(t, fmt.Sprintf(`{"projects":[{"projectId":"example","name":"Example","root":%q,"roots":[{"path":%q,"kind":"git"},{"path":%q,"kind":"git"}]}],"absent":[],"errors":[]}`,
		src, src, src+"/private"))
	herdr := &fakeHerdr{}
	socket := herdr.serve(t)
	herdr.value = jsonValue(map[string]any{"spaces": []any{checkoutSpace("example", checkout{"Example", src})}})
	dir, _ := stateDir()
	if err := saveRegistrations(dir, map[string]registration{"example": {ID: spaceID("example")}}); err != nil {
		t.Fatal(err)
	}
	if err := registrationReady(socket, workspaces); err != nil {
		t.Fatalf("unregistered nested checkout: %v", err)
	}
	if err := syncProjects(socket, workspaces, false, true); err != nil {
		t.Fatal(err)
	}
	if got := herdr.spaces(t); got["example/src/private"] != "Example/private []" {
		t.Fatalf("sync did not add the nested checkout: %v", got)
	}
	if err := registrationReady(socket, workspaces); err != nil {
		t.Fatal(err)
	}
	// A registered nested checkout still counts: a rename is drift.
	fakeDiscovery(t, fmt.Sprintf(`{"projects":[{"projectId":"example","name":"Renamed","root":%q,"roots":[{"path":%q,"kind":"git"},{"path":%q,"kind":"git"}]}],"absent":[],"errors":[]}`,
		src, src, src+"/private"))
	if err := registrationReady(socket, workspaces); err == nil {
		t.Fatal("renamed project appeared ready")
	}
}
