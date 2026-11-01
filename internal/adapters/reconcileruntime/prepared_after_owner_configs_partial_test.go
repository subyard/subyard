package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Run the real inventory and materializer against one parent-owned filesystem;
// remap only known guest paths and the synthetic guest identity, never payloads.
type afterOwnerFileExecutor struct {
	root             string
	cut              bool
	cutSeen          bool
	mutate           string
	calls            int
	beforeStructured func()
	publicationCut   string
}

func (e *afterOwnerFileExecutor) Exec(ctx context.Context, _, _ string, r ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	e.calls++
	destination := ""
	for _, arg := range r.Command {
		if arg == "/home/dev/.sample/a.conf" || arg == "/home/dev/.sample/b.rules" {
			destination = arg
		}
	}
	write := r.Command[0] == "sh" && destination != ""
	if r.Command[0] == "python3" && len(r.Command) > 6 && r.Command[4] == "apply" {
		destination, write = r.Command[6], true
		if e.beforeStructured != nil {
			hook := e.beforeStructured
			e.beforeStructured = nil
			hook()
		}
	}
	if r.Command[0] == "python3" && len(r.Command) > 4 && r.Command[4] == "configs-write" {
		var request struct {
			Entries []integrationArtifact `json:"entries"`
		}
		if err := json.Unmarshal(r.Stdin, &request); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if len(request.Entries) != 1 {
			return ports.InstanceExecResult{}, errors.New("invalid fixture write scope")
		}
		destination = request.Entries[0].Path
		write = true
	}
	if write && e.cut && destination == "/home/dev/.sample/b.rules" {
		e.cutSeen = true
		return ports.InstanceExecResult{}, errors.New("cut before original B destination write")
	}
	if write && destination == "/home/dev/.sample/b.rules" && e.mutate != "" {
		path := filepath.Join(e.root, "home/.sample/b.rules")
		switch e.mutate {
		case "mutation-third-state":
			if err := os.WriteFile(path, []byte("foreign mutation\n"), 0644); err != nil {
				return ports.InstanceExecResult{}, err
			}
		case "mutation-mode":
			if err := os.Chmod(path, 0600); err != nil {
				return ports.InstanceExecResult{}, err
			}
		case "mutation-path":
			parent := filepath.Dir(path)
			if err := os.Rename(parent, parent+"-original"); err != nil {
				return ports.InstanceExecResult{}, err
			}
			if err := os.Symlink(parent+"-original", parent); err != nil {
				return ports.InstanceExecResult{}, err
			}
		}
		e.mutate = ""
	}
	args := append([]string(nil), r.Command...)
	for i := range args {
		args[i] = strings.NewReplacer("/home/dev", filepath.Join(e.root, "home"), "/var/lib/subyard/integrations", filepath.Join(e.root, "inventory"), "OWNER = int('0')", fmt.Sprintf("OWNER = int('%d')", os.Getuid()), "/var/lib/subyard/config-materialization", filepath.Join(e.root, "materialization"), "STATE_UID = 0", fmt.Sprintf("STATE_UID = %d", os.Getuid())).Replace(args[i])
	}
	input := r.Stdin
	if r.Command[0] == "python3" && strings.Contains(r.Command[3], "class OwnershipConflict") {
		var value map[string]any
		if err := json.Unmarshal(input, &value); err != nil {
			return ports.InstanceExecResult{}, err
		}
		value["home"] = filepath.Join(e.root, "home")
		entries, _ := value["entries"].([]any)
		for _, entry := range entries {
			row := entry.(map[string]any)
			if path, ok := row["path"].(string); ok {
				row["path"] = strings.ReplaceAll(path, "/home/dev", filepath.Join(e.root, "home"))
			}
		}
		if entries, ok := value["legacy"].([]any); ok {
			for i, entry := range entries {
				entries[i] = strings.ReplaceAll(entry.(string), "/home/dev", filepath.Join(e.root, "home"))
			}
		}
		input, _ = json.Marshal(value)
	}
	if e.publicationCut != "" && len(args) > 6 && args[0] == "python3" && args[4] == "apply" {
		hook := fmt.Sprintf(`
_real_atomic_write = atomic_write
def _fixture_publish(path, *args, **kwargs):
    _real_atomic_write(path, *args, **kwargs)
    stage = "destination" if path == sys.argv[3] else ("baseline" if path.endswith(".json") else "receipt")
    with open(%q, "a") as events:
        events.write(stage + "\n")
    if stage == %q:
        raise MaterializationError("fixture cut after real publication")
atomic_write = _fixture_publish
`, filepath.Join(e.root, "publication-events"), e.publicationCut)
		args[3] = strings.Replace(args[3], "\ntry:\n    main()", hook+"\ntry:\n    main()", 1)
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
	}
	if err == nil && write {
		file, openErr := os.OpenFile(filepath.Join(e.root, "writes"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			return result, openErr
		}
		_, writeErr := fmt.Fprintln(file, destination)
		err = errors.Join(writeErr, file.Close())
	}
	return result, err
}
func afterOwnerFilesFixture(t *testing.T, root string) (Runtime, *afterOwnerFileExecutor) {
	t.Helper()
	incus := runningIncus(3)
	incus.Reconcile.Instance.Config = map[string]string{"volatile.uuid": "original-instance"}
	e := &afterOwnerFileExecutor{root: root}
	return Runtime{RepositoryRoot: root, Environment: []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CONFIG=" + filepath.Join(root, "sample.CONFIG"), "AGENT_sample_CONFIG_DEST=.sample/a.conf", "AGENT_sample_RULES=" + filepath.Join(root, "sample.RULES"), "AGENT_sample_RULES_DEST=.sample/b.rules"}, Incus: incus, Executor: e, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}}, e
}

type afterOwnerFileIdentity struct {
	Inode    uint64
	Modified int64
}

func afterOwnerFileStat(t *testing.T, path string) afterOwnerFileIdentity {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return afterOwnerFileIdentity{info.Sys().(*syscall.Stat_t).Ino, info.ModTime().UnixNano()}
}

func TestAfterOwnerMaterializedFilesPartialStateAcrossFreshProcess(t *testing.T) {
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_AFTER_OWNER_PARTIAL_CHILD"); root != "" {
		runtime, executor := afterOwnerFilesFixture(t, root)
		scenario := os.Getenv("SUBYARD_AFTER_OWNER_PARTIAL_SCENARIO")
		raw, err := os.ReadFile(filepath.Join(root, "resolved.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		physical, err := os.ReadFile(filepath.Join(root, "physical.record"))
		if err != nil {
			t.Fatal(err)
		}
		before := afterOwnerFileStat(t, filepath.Join(root, "home/.sample/a.conf"))
		writesBefore, _ := os.ReadFile(filepath.Join(root, "writes"))
		switch scenario {
		case "third-state":
			testkit.WriteFile(t, filepath.Join(root, "home/.sample/b.rules"), []byte("foreign B\n"), 0644)
		case "mode":
			if err := os.Chmod(filepath.Join(root, "home/.sample/b.rules"), 0600); err != nil {
				t.Fatal(err)
			}
		case "source":
			testkit.WriteFile(t, filepath.Join(root, "sample.RULES"), []byte("foreign source\n"), 0644)
		case "uuid":
			runtime.Incus.(*testkit.Incus).Reconcile.Instance.Config["volatile.uuid"] = "replacement"
		case "selector":
			runtime.Environment = append(runtime.Environment, "AGENT_sample_RULES_DEST=.sample/other.rules")
		case "path":
			parent := filepath.Join(root, "home/.sample")
			if err := os.Rename(parent, parent+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(parent+"-original", parent); err != nil {
				t.Fatal(err)
			}
		case "mutation-third-state", "mutation-mode", "mutation-path":
			executor.mutate = scenario
		}
		p, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, raw, physical)
		if err == nil {
			defer p.Close()
			err = runtime.ApplyPreparedMaterializedConfigs(ctx, p)
		}
		if scenario != "partial" {
			if err == nil {
				t.Fatal("unapproved fresh state accepted", scenario)
			}
			writesAfter, _ := os.ReadFile(filepath.Join(root, "writes"))
			if !bytes.Equal(writesBefore, writesAfter) {
				t.Fatal("refusal wrote selected destination", scenario)
			}
			if before != afterOwnerFileStat(t, filepath.Join(root, "home/.sample/a.conf")) {
				t.Fatal("refusal rewrote A")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if before != afterOwnerFileStat(t, filepath.Join(root, "home/.sample/a.conf")) {
			t.Fatal("fresh helper rewrote already-desired A")
		}
		if !bytes.Equal(p.Recipe(), raw) {
			t.Fatal("fresh helper replaced original recipe")
		}
		saved, _ := os.ReadFile(filepath.Join(root, "physical.record"))
		if !bytes.Equal(saved, physical) {
			t.Fatal("physical record changed")
		}
		return
	}
	if os.Getenv("SUBYARD_AFTER_OWNER_PARTIAL_SCENARIO") == "" {
		for _, scenario := range []string{"partial", "third-state", "source", "uuid", "selector", "mode", "path", "mutation-third-state", "mutation-mode", "mutation-path"} {
			t.Run(scenario, func(t *testing.T) {
				t.Setenv("SUBYARD_AFTER_OWNER_PARTIAL_SCENARIO", scenario)
				TestAfterOwnerMaterializedFilesPartialStateAcrossFreshProcess(t)
			})
		}
		return
	}
	root := testkit.TempDir(t)
	runtime, e := afterOwnerFilesFixture(t, root)
	if err := os.MkdirAll(filepath.Join(root, "home/.sample"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, content string }{{"sample.CONFIG", "approved A\n"}, {"sample.RULES", "approved B\n"}, {"home/.sample/a.conf", "original A\n"}, {"home/.sample/b.rules", "original B\n"}} {
		testkit.WriteFile(t, filepath.Join(root, item.name), []byte(item.content), 0644)
	}
	inventory := configInventoryFixture{t: t, home: filepath.Join(root, "home"), state: filepath.Join(root, "inventory")}
	entries := []map[string]any{}
	for _, name := range []string{"a.conf", "b.rules"} {
		letter := "A"
		if name == "b.rules" {
			letter = "B"
		}
		entries = append(entries, map[string]any{"id": "sample", "kind": "file", "path": filepath.Join(root, "home/.sample", name), "digest": fmt.Sprintf("%x", sha256.Sum256([]byte("original "+letter+"\n")))})
	}
	inventory.save(entries, []string{})
	source, err := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":8192,"descriptorBytes":8192}`))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if e.calls != 0 {
		t.Fatal("preconsent source capture read guest")
	}
	resolved, physical, err := runtime.ResolveAfterOwnerMaterializedConfigs(ctx, source.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.Close()
	derived, err := resolved.WithAfterOwnerMetadata([]byte(`{"version":1,"phase":"resolved"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	testkit.WriteFile(t, filepath.Join(root, "resolved.recipe"), derived.Recipe(), 0600)
	testkit.WriteFile(t, filepath.Join(root, "physical.record"), physical, 0600)
	e.cut = true
	if err := runtime.ApplyPreparedMaterializedConfigs(ctx, derived); err == nil || !e.cutSeen {
		t.Fatal("destination cut did not interrupt B", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "home/.sample/a.conf")); string(got) != "approved A\n" {
		t.Fatal("A did not reach desired")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "home/.sample/b.rules")); string(got) != "original B\n" {
		t.Fatal("B changed before cut")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestAfterOwnerMaterializedFilesPartialStateAcrossFreshProcess$", "-test.v")
	child.Env = []string{"PATH=" + os.Getenv("PATH"), "SUBYARD_AFTER_OWNER_PARTIAL_CHILD=" + root, "SUBYARD_AFTER_OWNER_PARTIAL_SCENARIO=" + os.Getenv("SUBYARD_AFTER_OWNER_PARTIAL_SCENARIO")}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, output)
	}
	if os.Getenv("SUBYARD_AFTER_OWNER_PARTIAL_SCENARIO") != "partial" {
		return
	}
	writes, err := os.ReadFile(filepath.Join(root, "writes"))
	if err != nil {
		t.Fatal(err)
	}
	if string(writes) != "/home/dev/.sample/a.conf\n/home/dev/.sample/b.rules\n" {
		t.Fatalf("writes repeated or expanded: %s", writes)
	}
	for _, entry := range inventory.read()["entries"].([]any) {
		if entry.(map[string]any)["config_pending_digest"] != nil {
			t.Fatal("inventory commit did not finish")
		}
	}
}

func TestAfterOwnerMaterializedFilesStoppedRecordCannotAuthorizeRunning(t *testing.T) {
	ctx := context.Background()
	root := testkit.TempDir(t)
	runtime, e := afterOwnerFilesFixture(t, root)
	testkit.WriteFile(t, filepath.Join(root, "sample.CONFIG"), []byte("approved A\n"), 0600)
	testkit.WriteFile(t, filepath.Join(root, "sample.RULES"), []byte("approved B\n"), 0600)
	incus := runtime.Incus.(*testkit.Incus)
	incus.Reconcile.Instance.Status = "Stopped"
	original, err := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":8192,"descriptorBytes":8192}`))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	p, physical, err := runtime.ResolveAfterOwnerMaterializedConfigs(ctx, original.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var record afterOwnerConfigPhysical
	if json.Unmarshal(physical, &record) != nil || !record.Stopped || len(record.Files) != 0 {
		t.Fatal("ambiguous stopped physical record")
	}
	if err := runtime.CheckAfterOwnerMaterializedConfigs(ctx, p); err != nil {
		t.Fatal("unchanged stopped record", err)
	}
	if err := runtime.ApplyPreparedMaterializedConfigs(ctx, p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("stopped record permitted writes", err)
	}
	if e.calls != 0 {
		t.Fatal("stopped resolution read or wrote guest")
	}
	incus.Reconcile.Instance.Status = "Running"
	fresh, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, original.Recipe(), physical)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := runtime.CheckAfterOwnerMaterializedConfigs(ctx, fresh); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("running guest gained late baseline", err)
	}
	if err := runtime.ApplyPreparedMaterializedConfigs(ctx, fresh); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("running guest gained writes", err)
	}
	if e.calls != 0 {
		t.Fatal("running refusal read or wrote guest")
	}
}

func TestAfterOwnerMaterializedFilesAbsentInventoryRemainsUnowned(t *testing.T) {
	ctx := context.Background()
	root := testkit.TempDir(t)
	runtime, e := afterOwnerFilesFixture(t, root)
	if err := os.Mkdir(filepath.Join(root, "home"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "sample.CONFIG"), []byte("approved A\n"), 0600)
	testkit.WriteFile(t, filepath.Join(root, "sample.RULES"), []byte("approved B\n"), 0600)
	original, err := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":8192,"descriptorBytes":8192}`))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if e.calls != 0 {
		t.Fatal("source capture read guest")
	}
	p, _, err := runtime.ResolveAfterOwnerMaterializedConfigs(ctx, original.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := os.Stat(filepath.Join(root, "inventory")); !os.IsNotExist(err) {
		t.Fatal("readonly observation created inventory")
	}
	for range 2 {
		if err := runtime.ApplyPreparedMaterializedConfigs(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	writes, _ := os.ReadFile(filepath.Join(root, "writes"))
	if string(writes) != "/home/dev/.sample/a.conf\n/home/dev/.sample/b.rules\n" {
		t.Fatal("initial publication repeated", string(writes))
	}
	for _, name := range []string{"a.conf", "b.rules"} {
		info, err := os.Stat(filepath.Join(root, "home/.sample", name))
		if err != nil || info.Mode().Perm() != 0644 {
			t.Fatal("desired file mode", err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "inventory")); !os.IsNotExist(err) {
		t.Fatal("config-only publication adopted inventory")
	}
}

func TestAfterOwnerMaterializedFilesEmptyObservation(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, e := afterOwnerFilesFixture(t, root)
	states, err := runtime.afterOwnerConfigRequest(context.Background(), "configs-observe", nil, nil)
	if err != nil || len(states) != 0 || e.calls != 1 {
		t.Fatalf("empty readonly observation: %v, %v, calls %d", states, err, e.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "inventory")); !os.IsNotExist(err) {
		t.Fatal("empty observation created inventory")
	}
}
