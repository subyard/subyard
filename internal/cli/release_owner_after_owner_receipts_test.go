package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Run the existing inventory/materializer on this fixture's filesystem. Only
// fixed guest paths/identity are remapped; the approved stdin payload is retained.
type receiptCLIExecutor struct{ root, cut string }

func (e *receiptCLIExecutor) Exec(ctx context.Context, _, _ string, r ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	args := append([]string(nil), r.Command...)
	home := filepath.Join(e.root, "guest/home/dev")
	paths := strings.NewReplacer("/home/dev", home, "/usr/local/libexec/subyard/projects-changed", filepath.Join(e.root, "guest/usr/local/libexec/subyard/projects-changed"), "/etc/subyard/agent-project-hooks", filepath.Join(e.root, "guest/etc/subyard/agent-project-hooks"), "/srv/workspaces", filepath.Join(e.root, "guest/workspaces"))
	for i := range args {
		args[i] = paths.Replace(strings.NewReplacer("/var/lib/subyard/integrations", filepath.Join(e.root, "guest/inventory"), "/var/lib/subyard/config-materialization", filepath.Join(e.root, "guest/materialization"), "OWNER = int('0')", fmt.Sprintf("OWNER = int('%d')", os.Getuid()), "STATE_UID = 0", fmt.Sprintf("STATE_UID = %d", os.Getuid())).Replace(args[i]))
	}
	input := r.Stdin
	if len(args) > 3 && args[0] == "bash" && args[3] == "--observe" {
		input = []byte(paths.Replace(string(input)))
	}
	if len(args) > 3 && args[0] == "python3" && strings.Contains(args[3], "class OwnershipConflict") {
		var value map[string]any
		if err := json.Unmarshal(input, &value); err != nil {
			return ports.InstanceExecResult{}, err
		}
		value["home"] = home
		entries, _ := value["entries"].([]any)
		for _, v := range entries {
			row := v.(map[string]any)
			if p, ok := row["path"].(string); ok {
				row["path"] = paths.Replace(p)
			}
		}
		if legacy, ok := value["legacy"].([]any); ok {
			for i, p := range legacy {
				legacy[i] = paths.Replace(p.(string))
			}
		}
		if states, ok := value["commit_states"].(map[string]any); ok {
			mapped := map[string]any{}
			for path, row := range states {
				mapped[paths.Replace(path)] = row
			}
			value["commit_states"] = mapped
		}
		input, _ = json.Marshal(value)
	}
	if e.cut != "" && len(args) > 6 && args[0] == "python3" && args[4] == "apply" {
		hook := fmt.Sprintf(`
_real_atomic_write = atomic_write
def _fixture_publish(path, *args, **kwargs):
    _real_atomic_write(path, *args, **kwargs)
    stage = "destination" if path == sys.argv[3] else ("baseline" if path.endswith(".json") else "receipt")
    if stage == %q:
        raise MaterializationError("fixture cut after real publication")
atomic_write = _fixture_publish
`, e.cut)
		args[3] = strings.Replace(args[3], "\ntry:\n    main()", hook+"\ntry:\n    main()", 1)
	}
	if len(r.Command) == 5 && r.Command[0] == "sh" && r.Command[2] == `[ -x "$1" ] && exec timeout --kill-after=5s 120s "$1"` && r.Command[4] == "/usr/local/libexec/subyard/projects-changed" {
		// Execute the real fixture-published dispatcher with its fixed guest
		// paths remapped, as for the existing readonly dispatcher observation.
		program, err := os.ReadFile(paths.Replace(r.Command[4]))
		if err != nil {
			return ports.InstanceExecResult{}, err
		}
		args = []string{"timeout", "--kill-after=5s", "120s", "bash", "-s", "--"}
		input = []byte(paths.Replace(string(program)))
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	environment := map[string]string{"PATH": os.Getenv("PATH"), "HOME": home}
	for key, value := range r.Environment {
		environment[key] = paths.Replace(value)
	}
	command.Env = environmentList(environment, nil)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if len(args) > 4 && args[0] == "python3" && args[4] == "observe" && strings.Contains(args[3], "class OwnershipConflict") {
		reverse := strings.NewReplacer(home, "/home/dev", filepath.Join(e.root, "guest/usr/local/libexec/subyard/projects-changed"), "/usr/local/libexec/subyard/projects-changed", filepath.Join(e.root, "guest/etc/subyard/agent-project-hooks"), "/etc/subyard/agent-project-hooks")
		result.Stdout = []byte(reverse.Replace(string(result.Stdout)))
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
	}
	return result, err
}

type receiptCLIRecord struct {
	Context            releaseowner.Context
	Environment        []string
	Original, Resolved releaseowner.Target
	Source, Derived    []byte
	Format             string
	Established        bool
	Originals          []originalCLIRecipe
}

func receiptCLIIdentity(t *testing.T, path string) [2]int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return [2]int64{int64(info.Sys().(*syscall.Stat_t).Ino), info.ModTime().UnixNano()}
}
func TestAfterOwnerFilesCLIReceiptCutsAcrossFreshProcess(t *testing.T) {
	afterOwnerFilesCLIReceiptCuts(t, true, true)
}

// The whole CaptureActivation still visits legacy integration adoption before
// original-recipe binding when inventory is absent. Preserve that separate gap.
func TestAfterOwnerFilesCLIAbsentInventoryDestinationCutRemainsRefused(t *testing.T) {
	afterOwnerFilesCLIReceiptCuts(t, false, false)
}

func TestAfterOwnerFilesCLIAllOriginalsAbsentInventoryRestore(t *testing.T) {
	afterOwnerFilesCLIReceiptCuts(t, false, true)
}

func afterOwnerFilesCLIReceiptCuts(t *testing.T, established, allOriginals bool) {
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_CLI_RECEIPT_CHILD"); root != "" {
		raw, err := os.ReadFile(filepath.Join(root, "receipt-cli.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record receiptCLIRecord
		if json.Unmarshal(raw, &record) != nil {
			t.Fatal("invalid original CLI record")
		}
		incus := lifecycleIncus()
		instance := ports.InstanceInfo{Name: record.Resolved.Instance, Project: record.Resolved.Project, Status: "Running", Config: map[string]string{"volatile.uuid": record.Resolved.InstanceUUID, "user.subyard.owner_create": strings.Repeat("1", 32)}}
		incus.Instances[instance.Project+"/"+instance.Name] = instance
		incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
		var c *releaseOwnerComponents
		if len(record.Originals) > 0 {
			c = framedOriginalCLIRecipes(t, record.Context, incus, &receiptCLIExecutor{root: root}, record.Originals)
		} else {
			program, err := New(Options{RepositoryRoot: root, Environment: record.Environment, Incus: incus, Executor: &receiptCLIExecutor{root: root}})
			if err != nil {
				t.Fatal(err)
			}
			fd, err := os.Open(record.Context.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			c = &releaseOwnerComponents{cli: program, root: fd, context: record.Context, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, activation: map[string]releaseActivationPrepared{}, installedBound: map[string]bool{}}
			defer c.Close()
			if _, _, err := c.CaptureInventory(ctx); err != nil {
				t.Fatal(err)
			}
		}
		destination := filepath.Join(root, "guest/home/dev/.sample/a."+record.Format)
		originalIdentity := receiptCLIIdentity(t, destination)
		if _, err := c.CaptureActivation(ctx, record.Original.Owner, true); err != nil {
			if !record.Established && len(record.Originals) == 0 && strings.Contains(err.Error(), "integration structured configuration ownership conflict") {
				if originalIdentity != receiptCLIIdentity(t, destination) {
					t.Fatal("refused source capture rewrote destination")
				}
				return
			}
			t.Fatal("fresh source construction", err)
		}
		if !record.Established && len(record.Originals) == 0 {
			t.Fatal("absent-inventory full-capture gap unexpectedly disappeared")
		}
		if err := c.BindInstalledRuntimeRecipeForTarget(ctx, record.Original.PreparedRecipe, record.Source, record.Original); err != nil {
			t.Fatal(err)
		}
		completed := bindAfterOwnerFilesResults(t, c, record.Original)
		if err := c.BindAfterOwnerCompletion(completed); err != nil {
			t.Fatal(err)
		}
		if err := c.BindInstalledRuntimeRecipeForTarget(ctx, record.Resolved.PreparedRecipe, record.Derived, record.Resolved); err != nil {
			t.Fatal("original derived recipe", err)
		}
		before := receiptCLIIdentity(t, destination)
		if _, err := c.CaptureActivation(ctx, record.Original.Owner, true); err != nil {
			t.Fatal("fresh upper observation", err)
		}
		if err := c.ApplyActivation(ctx, record.Resolved); err != nil {
			t.Fatal("original CLI apply after receipt cut", err)
		}
		if before != receiptCLIIdentity(t, destination) {
			t.Fatal("CLI receipt repair rewrote destination")
		}
		derived, err := c.InstalledRuntimeRecipe(record.Resolved.PreparedRecipe)
		if err != nil || !bytes.Equal(record.Derived, derived) {
			t.Fatal("CLI replaced original recipe", err)
		}
		values, err := c.CaptureActivation(ctx, record.Original.Owner, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range values {
			if v.Class == "materialized-files" && v.State != "current" {
				t.Fatal("receipts did not converge")
			}
		}
		return
	}
	for _, format := range []string{"json", "toml"} {
		for _, cut := range []string{"destination", "baseline"} {
			if !established && cut != "destination" {
				continue
			}
			t.Run(format+"/"+cut, func(t *testing.T) {
				c, _ := nativeActivationSourceFixture(t)
				root := c.context.RepositoryRoot
				nativeMaterializedSourceFiles(t, root)
				hookSource, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "projects-changed.sh"))
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), hookSource, 0644)
				settings := filepath.Join(root, "config/agents.env")
				content, err := os.ReadFile(settings)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, []byte("AGENT_sample_RULES=''\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/agents/sample/CONFIG\"\nAGENT_sample_CONFIG_DEST=.sample/a."+format+"\n")...)
				testkit.WriteFile(t, settings, content, 0755)
				desired, original := []byte(`{"managed":"desired"}`), []byte(`{"managed":"original","runtime":"kept"}`)
				if format == "toml" {
					desired = []byte("managed = 'desired'\n")
					original = []byte("managed = 'original'\nruntime = 'kept'\n")
				}
				testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/CONFIG"), desired, 0755)
				destination := filepath.Join(root, "guest/home/dev/.sample/a."+format)
				if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, destination, original, 0644)
				if err := os.MkdirAll(c.context.DataHome, 0700); err != nil {
					t.Fatal(err)
				}
				incus := c.cli.options.Incus.(*testkit.Incus)
				delete(incus.Instances, "subyard/yard")
				incus.Reconcile = ports.ReconcileState{}
				executor := &receiptCLIExecutor{root: root}
				c.cli.options.Executor = executor
				if allOriginals {
					c.cli.env["PATH"] = os.Getenv("PATH")
					c.cli.env["SUBYARD_CLI_RECEIPT_CHILD"] = root
					c.cli.options.Environment = environmentList(c.cli.env, nil)
				}
				if established {
					program, err := os.ReadFile(filepath.Join(repositoryRoot(t), "internal/adapters/reconcileruntime/integration_inventory.py"))
					if err != nil {
						t.Fatal(err)
					}
					program = []byte(strings.NewReplacer("@STATE_ROOT@", "/var/lib/subyard/integrations", "@STATE_UID@", "0").Replace(string(program)))
					request := ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", string(program), "apply"}, Stdin: []byte(`{"home":"/home/dev","uid":1000,"entries":[]}`)}
					if result, err := executor.Exec(ctx, "", "", request); err != nil || result.ExitCode != 0 {
						t.Fatal("ordinary empty integration inventory setup", err, string(result.Stderr))
					}
				}
				// Match the fresh native factory after all fixture sources are staged.
				program, err := New(c.cli.options)
				if err != nil {
					t.Fatal(err)
				}
				c.cli = program.releaseActivationOperation()
				c.catalog = &c.cli.catalog
				if _, _, err := c.CaptureInventory(ctx); err != nil {
					t.Fatal(err)
				}
				targets, err := c.CaptureActivation(ctx, "materialized-config", false)
				if err != nil || len(targets) != 1 {
					t.Fatal("original source capture", targets, err)
				}
				prepared, err := c.prepareAfterOwnerFiles(ctx, c.activation[activationKey(targets[0])])
				if err != nil {
					t.Fatal(err)
				}
				originalTarget := prepared.value
				c.activation[activationKey(originalTarget)] = prepared
				source := prepared.nativeInstalled.Recipe()
				if err := c.BindInstalledRuntimeRecipeForTarget(ctx, originalTarget.PreparedRecipe, source, originalTarget); err != nil {
					t.Fatal(err)
				}
				originals := []originalCLIRecipe{}
				if allOriginals {
					c.ownerChildren = &preparedOwnerChildren{inits: map[string]preparedOwnerInitChild{}}
					sibling, err := c.prepareAfterOwnerIntegration(ctx, prepared.configTarget, prepared.platform.(reconcileruntime.Runtime), c.activationSource())
					if err != nil {
						t.Fatal("original integration sibling", err)
					}
					c.activation[activationKey(sibling.value)] = sibling
					siblingRaw := sibling.nativeInstalled.Recipe()
					originals = []originalCLIRecipe{{originalTarget, source}, {sibling.value, siblingRaw}}
				}
				completed := bindAfterOwnerFilesResults(t, c, originalTarget)
				instance := ports.InstanceInfo{Name: originalTarget.Instance, Project: originalTarget.Project, Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
				incus.Instances[instance.Project+"/"+instance.Name] = instance
				incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
				resolved, hash, err := c.ResolveAfterOwner(ctx, originalTarget, completed)
				if err != nil {
					t.Fatal(err)
				}
				derived, err := c.InstalledRuntimeRecipe(hash)
				if err != nil {
					t.Fatal(err)
				}
				record := receiptCLIRecord{Context: c.context, Environment: c.cli.options.Environment, Original: originalTarget, Resolved: resolved, Source: source, Derived: derived, Format: format, Established: established, Originals: originals}
				raw, _ := json.Marshal(record)
				testkit.WriteFile(t, filepath.Join(root, "receipt-cli.json"), raw, 0600)
				executor.cut = cut
				if err := c.ApplyActivation(ctx, resolved); err == nil {
					t.Fatal("full CLI publisher cut missing")
				}
				current, _ := os.ReadFile(destination)
				if !bytes.Contains(current, []byte("desired")) {
					t.Fatal("CLI cut preceded real destination publication")
				}
				child := exec.Command(os.Args[0], "-test.run=^"+strings.Split(t.Name(), "/")[0]+"$", "-test.v")
				child.Env = append(record.Environment, "PATH="+os.Getenv("PATH"), "SUBYARD_CLI_RECEIPT_CHILD="+root)
				if output, err := child.CombinedOutput(); err != nil {
					t.Fatalf("fresh CLI helper: %v\n%s", err, output)
				}
			})
		}
	}
}
