package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"errors"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

// Services and command availability are synthetic Incus observations here. Every
// inventory/config/hook filesystem request runs its real existing program.
// This fixture makes no claim about opaque native command quiescence.
type integrationReceiptCLIExecutor struct {
	receiptCLIExecutor
	cutB       bool
	trace      *testing.T
	busyCommit bool
	core       bool
	coreStale  bool
}

func (e *integrationReceiptCLIExecutor) Exec(ctx context.Context, project, name string, r ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := strings.Join(r.Command, "\x00")
	if e.core {
		if strings.Contains(command, "ccusage-current") {
			return ports.InstanceExecResult{Stdout: []byte("ccusage-current\n")}, nil
		}
		if strings.Contains(command, "helper=/usr/local/bin/subyard-preview") {
			if e.coreStale {
				return ports.InstanceExecResult{ExitCode: 1}, errors.New("fixture core stale")
			}
			return ports.InstanceExecResult{}, nil
		}
		if strings.Contains(command, "for path in /usr/local/bin/subyard-preview") {
			return ports.InstanceExecResult{}, nil
		}
	}
	if strings.Contains(command, "mode=$1") && strings.Contains(command, "changed=0") {
		return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
	}
	if len(r.Command) > 2 && r.Command[0] == "sh" && r.Command[2] == `command -v "$1" >/dev/null` {
		return ports.InstanceExecResult{}, nil
	}
	if len(r.Command) > 4 && r.Command[0] == "python3" && (r.Command[4] == "apply" || r.Command[4] == "configs-write") && strings.Contains(r.Command[3], "class OwnershipConflict") {
		source := r.Command[3]
		count := fmt.Sprintf("\n_fixture_count_atomic = atomic\ndef _fixture_count(path,*args,**kwargs):\n    _fixture_count_atomic(path,*args,**kwargs)\n    if path.startswith('/home/dev/.sample/'):\n        with open(%q,'a') as events:\n            events.write(os.path.basename(path)+'\\n')\natomic = _fixture_count\n", filepath.Join(e.root, "integration-publications"))
		source = strings.Replace(source, "\ntry:\n    locked_main()", count+"\ntry:\n    locked_main()", 1)
		if e.cutB {
			hook := fmt.Sprintf("\n_fixture_atomic = atomic\ndef _fixture_cut(path,*args,**kwargs):\n    if path == %q:\n        raise ValueError('fixture full-set B destination cut')\n    _fixture_atomic(path,*args,**kwargs)\natomic = _fixture_cut\n", "/home/dev/.sample/b.rules")
			source = strings.Replace(source, "\ntry:\n    locked_main()", hook+"\ntry:\n    locked_main()", 1)
		}
		r.Command = append([]string(nil), r.Command...)
		r.Command[3] = source
	}
	if strings.Contains(command, `[ -x "$dispatcher" ]`) {
		r.Command = append([]string(nil), r.Command...)
		if r.Command[len(r.Command)-1] == "0:0" {
			r.Command[len(r.Command)-1] = fmt.Sprintf("%d:%d", os.Getuid(), os.Getuid())
		}
	}

	if e.busyCommit && len(r.Command) > 4 && r.Command[0] == "python3" && r.Command[4] == "commit" {
		destination := filepath.Join(e.root, "guest/home/dev/.sample/a.json")
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte("dev\x00"+destination)))
		lock, err := os.OpenFile(filepath.Join(e.root, "guest/materialization", identity+".lock"), os.O_RDONLY, 0)
		if err != nil {
			return ports.InstanceExecResult{}, err
		}
		defer lock.Close()
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return ports.InstanceExecResult{}, err
		}
		defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	}
	result, err := e.receiptCLIExecutor.Exec(ctx, project, name, r)
	if err != nil && e.trace != nil {
		e.trace.Log("fixture native refusal", r.Command[0], r.Command[len(r.Command)-1], err, string(result.Stderr))
	}
	return result, err
}

type integrationReceiptCLIRecord struct {
	Context     releaseowner.Context
	Environment []string
	Originals   []originalCLIRecipe
	Derived     []byte
	Target      releaseowner.Target
	Format      string
}

func TestCurrentMaterializedFilesThenOriginalIntegrations(t *testing.T) {
	afterOwnerIntegrationsCLICut(t, "current", false)
}

func TestAfterOwnerIntegrationsCLIFullSetCutFreshOriginalRestore(t *testing.T) {
	afterOwnerIntegrationsCLICut(t, "conf", false)
}
func TestAfterOwnerIntegrationsCLIStructuredCutFreshOriginalRestore(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		t.Run(format, func(t *testing.T) { afterOwnerIntegrationsCLICut(t, format, false) })
	}
}
func TestAfterOwnerIntegrationsCLIBusyStructuredCommitFreshOriginalRestore(t *testing.T) {
	afterOwnerIntegrationsCLICut(t, "json", true)
}

type endpointCLIRecord struct {
	Context     releaseowner.Context
	Environment []string
	Instance    ports.InstanceInfo
	Originals   []originalCLIRecipe
}

func TestCurrentIntegrationEndpointOriginalAuthorityFreshRestore(t *testing.T) {
	if root := os.Getenv("SUBYARD_ENDPOINT_ORIGINAL_CHILD"); root != "" {
		var record endpointCLIRecord
		raw, err := os.ReadFile(filepath.Join(root, "endpoint-originals.json"))
		if err != nil || json.Unmarshal(raw, &record) != nil || len(record.Originals) != 2 {
			t.Fatal("original child record", err)
		}
		incus := lifecycleIncus()
		incus.Instances["subyard/yard"] = record.Instance
		incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: record.Instance}
		executor := &integrationReceiptCLIExecutor{receiptCLIExecutor: receiptCLIExecutor{root: root}, core: true}
		program, err := New(Options{RepositoryRoot: record.Context.RepositoryRoot, Environment: record.Environment, Incus: incus, Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		fd, err := os.Open(record.Context.ConfigHome)
		if err != nil {
			t.Fatal(err)
		}
		defer fd.Close()
		fresh := &releaseOwnerComponents{cli: program.releaseActivationOperation(), root: fd, context: record.Context, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, activation: map[string]releaseActivationPrepared{}, installedBound: map[string]bool{}}
		defer fresh.Close()
		if _, _, err = fresh.CaptureInventory(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, original := range record.Originals {
			if _, err = fresh.CaptureActivation(context.Background(), original.Target.Owner, true); err != nil {
				t.Fatal(err)
			}
			if err = fresh.BindInstalledRuntimeRecipeForTarget(context.Background(), original.Target.PreparedRecipe, original.Raw, original.Target); err != nil {
				t.Fatal("fresh process original bind", err)
			}
		}
		original := record.Originals[1]
		final, err := fresh.CaptureActivation(context.Background(), original.Target.Owner, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range final {
			if target.Class == original.Target.Class {
				if target.Source != original.Target.Source || target.Desired != original.Target.Desired || target.PreparedRecipe != original.Target.PreparedRecipe || target.State != "current" {
					t.Fatal("fresh process changed original target")
				}
				retained, err := fresh.InstalledRuntimeRecipe(target.PreparedRecipe)
				if err != nil || string(retained) != string(original.Raw) {
					t.Fatal("fresh process changed original capsule", err)
				}
				return
			}
		}
		t.Fatal("fresh process lost integration target")
	}
	for _, scenario := range []string{"valid", "unchanged-no-core", "missing-core", "missing-core-recipe", "wrong-source", "wrong-desired", "foreign-config", "uuid", "wrong-endpoint", "local-mismatch", "stale-core"} {
		t.Run(scenario, func(t *testing.T) { afterOwnerIntegrationsCLICut(t, "endpoint:"+scenario, false) })
	}
}

func afterOwnerIntegrationsCLICut(t *testing.T, format string, busy bool) {
	endpoint := strings.HasPrefix(format, "endpoint:")
	scenario := strings.TrimPrefix(format, "endpoint:")
	current := format == "current" || endpoint
	if current {
		format = "conf"
	}
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_CLI_INTEGRATION_PARTIAL_CHILD"); root != "" {
		raw, err := os.ReadFile(filepath.Join(root, "integration-partial.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record integrationReceiptCLIRecord
		if json.Unmarshal(raw, &record) != nil {
			t.Fatal("original record")
		}
		incus := lifecycleIncus()
		instance := ports.InstanceInfo{Name: record.Target.Instance, Project: record.Target.Project, Status: "Running", Config: map[string]string{"volatile.uuid": record.Target.InstanceUUID, "user.subyard.owner_create": strings.Repeat("1", 32)}}
		incus.Instances[instance.Project+"/"+instance.Name] = instance
		incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
		executor := &integrationReceiptCLIExecutor{receiptCLIExecutor: receiptCLIExecutor{root: root}, trace: t}
		c := framedOriginalCLIRecipes(t, record.Context, incus, executor, record.Originals)
		completed := bindAfterOwnerFilesResults(t, c, record.Originals[1].Target)
		if err := c.BindAfterOwnerCompletion(completed); err != nil {
			t.Fatal(err)
		}
		if err := c.BindInstalledRuntimeRecipeForTarget(ctx, record.Target.PreparedRecipe, record.Derived, record.Target); err != nil {
			t.Fatal("checked derived bind", err)
		}
		a := filepath.Join(root, "guest/home/dev/.sample/a."+record.Format)
		before := receiptCLIIdentity(t, a)
		if _, err := c.CaptureActivation(ctx, "materialized-config", true); err != nil {
			t.Fatal("fresh upper capture", err)
		}
		if err := c.ApplyActivation(ctx, record.Target); err != nil {
			t.Fatal("same original integration apply", err)
		}
		if before != receiptCLIIdentity(t, a) {
			t.Fatal("full-set retry rewrote A")
		}
		b, err := os.ReadFile(filepath.Join(root, "guest/home/dev/.sample/b.rules"))
		if err != nil || string(b) != "approved B\n" {
			t.Fatal("remaining B", err)
		}
		events, err := os.ReadFile(filepath.Join(root, "integration-publications"))
		if err != nil || strings.Count(string(events), "b.rules\n") != 1 {
			t.Fatal("remaining B was not published exactly once", err, string(events))
		}
		values, err := c.CaptureActivation(ctx, "materialized-config", true)
		if err != nil {
			t.Fatal("final owning observation", err)
		}
		for _, v := range values {
			if v.Class == "materialized-integrations" && v.State != "current" {
				t.Fatal("full set did not commit", v.State)
			}
		}
		got, err := c.InstalledRuntimeRecipe(record.Target.PreparedRecipe)
		if err != nil || string(got) != string(record.Derived) {
			t.Fatal("original recipe replaced", err)
		}
		return
	}
	c, _ := nativeActivationSourceFixture(t)
	root := c.context.RepositoryRoot
	nativeMaterializedSourceFiles(t, root)
	if endpoint {
		bin := testkit.TempDir(t)
		for _, name := range []string{"tailscale", "ip"} {
			testkit.WriteFile(t, filepath.Join(bin, name), []byte("#!/bin/sh\nexit 1\n"), 0755)
		}
		t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
		paths := []string{"scripts/lib-power.sh", "scripts/refresh-installed-guest-runtime.sh", "scripts/prepare-preview-route.sh", "config/preview/subyard-preview", "config/agents/ccusage/provision.sh"}
		entries, err := os.ReadDir(filepath.Join(repositoryRoot(t), "scripts/lib"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				paths = append(paths, "scripts/lib/"+entry.Name())
			}
		}
		for _, relative := range paths {
			payload, err := os.ReadFile(filepath.Join(repositoryRoot(t), relative))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, relative), payload, 0755)
		}
	}
	hook, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config/projects-changed.sh"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), hook, 0644)
	settings := []byte("CODING_TOOL_INTEGRATIONS=sample\nAGENT_sample_COMMAND=sample\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/agents/sample/CONFIG\"\nAGENT_sample_CONFIG_DEST=.sample/a." + format + "\nAGENT_sample_RULES=\"$SUBYARD_CONFIG_DIR/agents/sample/RULES\"\nAGENT_sample_RULES_DEST=.sample/b.rules\n")
	if endpoint {
		settings = append(settings, []byte("CCUSAGE_PROVISION=\"$SUBYARD_CONFIG_DIR/agents/ccusage/provision.sh\"\n")...)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/agents.env"), settings, 0755)
	payload := []byte("approved A\n")
	if format == "json" {
		payload = []byte(`{"managed":"desired"}`)
	}
	if format == "toml" {
		payload = []byte("managed = 'desired'\n")
	}
	testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/CONFIG"), payload, 0755)
	testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/RULES"), []byte("approved B\n"), 0755)
	if err := os.MkdirAll(filepath.Join(root, "guest/home/dev"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.context.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	incus := c.cli.options.Incus.(*testkit.Incus)
	delete(incus.Instances, "subyard/yard")
	incus.Reconcile = ports.ReconcileState{}
	executor := &integrationReceiptCLIExecutor{receiptCLIExecutor: receiptCLIExecutor{root: root}, trace: t, core: endpoint}
	c.cli.options.Executor = executor
	c.cli.env["PATH"] = os.Getenv("PATH")
	c.cli.env["SUBYARD_CLI_INTEGRATION_PARTIAL_CHILD"] = root
	c.cli.options.Environment = environmentList(c.cli.env, nil)
	establishCLIHookInventory(t, executor, root)
	if current {
		instance := ports.InstanceInfo{Name: "yard", Project: "subyard", Status: "Running", Config: map[string]string{"volatile.uuid": "original-current-uuid"}}
		if endpoint {
			instance.Config["user.subyard.preview_sha256"] = strings.Repeat("b", 64)
			instance.Config[previewroute.EndpointKey] = strings.Repeat("a", 64)
			instance.LocalConfig = map[string]string{"user.subyard.preview_sha256": strings.Repeat("b", 64), previewroute.EndpointKey: strings.Repeat("a", 64)}
		}
		incus.Instances["subyard/yard"] = instance
		incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
		path := filepath.Join(root, "guest/inventory/inventory.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var inventory map[string]any
		if json.Unmarshal(raw, &inventory) != nil {
			t.Fatal("prior inventory")
		}
		for _, name := range []string{"a.conf", "b.rules"} {
			destination := filepath.Join(root, "guest/home/dev/.sample", name)
			if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, destination, []byte("old\n"), 0644)
			if err := os.Chown(destination, -1, os.Getuid()); err != nil {
				t.Fatal(err)
			}
			inventory["entries"] = append(inventory["entries"].([]any), map[string]any{"id": "sample", "kind": "file", "path": destination, "digest": fmt.Sprintf("%x", sha256.Sum256([]byte("old\n")))})
		}
		raw, _ = json.Marshal(inventory)
		testkit.WriteFile(t, path, raw, 0600)
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
	values, err := c.CaptureActivation(ctx, "materialized-config", false)
	if current {
		if err != nil || len(values) != 2 {
			t.Fatal("original current targets", err, len(values))
		}
		var files, integration releaseowner.Target
		for _, target := range values {
			if target.Class == "materialized-files" {
				files = target
			} else {
				integration = target
			}
		}
		raw, err := c.InstalledRuntimeRecipe(integration.PreparedRecipe)
		if err != nil {
			t.Fatal(err)
		}
		fileRecipe, err := c.InstalledRuntimeRecipe(files.PreparedRecipe)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.BindInstalledRuntimeRecipeForTarget(ctx, files.PreparedRecipe, fileRecipe, files); err != nil {
			t.Fatal(err)
		}
		if err := c.ApplyActivation(ctx, files); err != nil {
			t.Fatal("files first", err)
		}
		fresh, err := c.CaptureActivation(ctx, "materialized-config", true)
		if err != nil {
			t.Fatal("post-files capture", err)
		}
		var observed releaseowner.Target
		for _, target := range fresh {
			if target.Class == integration.Class {
				observed = target
			}
		}
		if observed.State != "stale" || observed.Actual == integration.Actual {
			t.Fatal("remaining integration work or actual config progress not exercised")
		}
		aPath, bPath := filepath.Join(root, "guest/home/dev/.sample/a.conf"), filepath.Join(root, "guest/home/dev/.sample/b.rules")
		aBefore, bBefore := receiptCLIIdentity(t, aPath), receiptCLIIdentity(t, bPath)
		inventoryPath := filepath.Join(root, "guest/inventory/inventory.json")
		inventoryBefore, err := os.ReadFile(inventoryPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"before", "source", "uuid"} {
			foreign := integration
			switch field {
			case "before":
				foreign.Before = strings.Repeat("0", 64)
			case "source":
				foreign.Source = strings.Repeat("0", 64)
			case "uuid":
				foreign.InstanceUUID = "foreign"
			}
			if c.BindInstalledRuntimeRecipeForTarget(ctx, foreign.PreparedRecipe, raw, foreign) == nil {
				t.Fatal("foreign original admitted", field)
			}
		}
		inventoryAfter, err := os.ReadFile(inventoryPath)
		if err != nil || string(inventoryAfter) != string(inventoryBefore) || aBefore != receiptCLIIdentity(t, aPath) || bBefore != receiptCLIIdentity(t, bPath) {
			t.Fatal("refused binding changed protected files")
		}
		if err := c.BindInstalledRuntimeRecipeForTarget(ctx, integration.PreparedRecipe, raw, integration); err != nil {
			t.Fatal("original integration progress bind", err)
		}
		if err := c.ApplyActivation(ctx, integration); err != nil {
			t.Fatal("remaining integration work", err)
		}
		if got, err := c.InstalledRuntimeRecipe(integration.PreparedRecipe); err != nil || string(got) != string(raw) {
			t.Fatal("original integration recipe replaced")
		}
		if endpoint {
			cores, err := c.CaptureActivation(ctx, "core-guest-runtime", true)
			if err != nil || len(cores) != 1 {
				t.Fatal("original core capture", err, len(cores))
			}
			core := cores[0]
			coreRaw, err := c.InstalledRuntimeRecipe(core.PreparedRecipe)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "unchanged-no-core" {
				instance := incus.Instances["subyard/yard"]
				digest := fmt.Sprintf("%x", sha256.Sum256(previewroute.Metadata("127.0.0.1", "")))
				instance.Config[previewroute.EndpointKey], instance.LocalConfig[previewroute.EndpointKey] = digest, digest
				switch scenario {
				case "foreign-config":
					instance.Config["user.foreign.fixture"] = "foreign"
				case "uuid":
					instance.Config["volatile.uuid"] = "foreign-uuid"
				case "wrong-endpoint":
					instance.Config[previewroute.EndpointKey], instance.LocalConfig[previewroute.EndpointKey] = strings.Repeat("c", 64), strings.Repeat("c", 64)
				case "local-mismatch":
					instance.LocalConfig[previewroute.EndpointKey] = strings.Repeat("c", 64)
				case "stale-core":
					executor.coreStale = true
				case "wrong-source":
					core.Source = strings.Repeat("0", 64)
				case "wrong-desired":
					core.Desired = strings.Repeat("0", 64)
				}
				incus.Instances["subyard/yard"] = instance
				incus.Reconcile.Instance = instance
			}
			fresh := &releaseOwnerComponents{cli: c.cli, root: c.root, context: c.context, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, activation: map[string]releaseActivationPrepared{}, installedBound: map[string]bool{}}
			defer fresh.Close()
			if _, _, err = fresh.CaptureInventory(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = fresh.CaptureActivation(ctx, core.Owner, true); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing-core" && scenario != "unchanged-no-core" {
				err = fresh.BindInstalledRuntimeRecipeForTarget(ctx, core.PreparedRecipe, coreRaw, core)
				if scenario == "uuid" || scenario == "wrong-source" || scenario == "wrong-desired" {
					if err == nil {
						t.Fatal("foreign core target admitted")
					}
					return
				}
				if err != nil {
					t.Fatal("restore original core", err)
				}
				if scenario == "missing-core-recipe" {
					delete(fresh.installedRecipes, core.PreparedRecipe)
				}
			}
			if _, err = fresh.CaptureActivation(ctx, integration.Owner, true); err != nil {
				t.Fatal(err)
			}
			err = fresh.BindInstalledRuntimeRecipeForTarget(ctx, integration.PreparedRecipe, raw, integration)
			if scenario != "valid" && scenario != "unchanged-no-core" {
				if err == nil {
					t.Fatal("foreign endpoint projection admitted")
				}
				return
			}
			if err != nil {
				t.Fatal("restore original integration", err)
			}
			final, err := fresh.CaptureActivation(ctx, integration.Owner, true)
			if err != nil {
				t.Fatal("fresh original comparison", err)
			}
			var got releaseowner.Target
			for _, target := range final {
				if target.Class == integration.Class {
					got = target
				}
			}
			if got.Source != integration.Source || got.Desired != integration.Desired || got.PreparedRecipe != integration.PreparedRecipe || got.State != "current" {
				t.Fatal("original integration identity or convergence changed")
			}
			retained, err := fresh.InstalledRuntimeRecipe(integration.PreparedRecipe)
			if err != nil || string(retained) != string(raw) {
				t.Fatal("original capsule replaced", err)
			}

			if scenario == "valid" {
				record := endpointCLIRecord{c.context, c.cli.options.Environment, incus.Instances["subyard/yard"], []originalCLIRecipe{{core, coreRaw}, {integration, raw}}}
				encoded, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "endpoint-originals.json"), encoded, 0600)
				childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				command := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestCurrentIntegrationEndpointOriginalAuthorityFreshRestore$", "-test.v")
				command.Env = append(os.Environ(), "SUBYARD_ENDPOINT_ORIGINAL_CHILD="+root)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("fresh process original restore: %v\n%s", err, output)
				}
			}
		}
		return
	}
	if err != nil || len(values) != 1 {
		t.Fatal("original source capture", err, len(values))
	}
	files, err := c.prepareAfterOwnerFiles(ctx, c.activation[activationKey(values[0])])
	if err != nil {
		t.Fatal(err)
	}
	c.activation[activationKey(files.value)] = files
	c.ownerChildren = &preparedOwnerChildren{inits: map[string]preparedOwnerInitChild{}}
	integration, err := c.prepareAfterOwnerIntegration(ctx, files.configTarget, files.platform.(reconcileruntime.Runtime), c.activationSource())
	if err != nil {
		t.Fatal(err)
	}
	c.activation[activationKey(integration.value)] = integration
	originals := []originalCLIRecipe{{files.value, files.nativeInstalled.Recipe()}, {integration.value, integration.nativeInstalled.Recipe()}}
	for _, o := range originals {
		if err := c.BindInstalledRuntimeRecipeForTarget(ctx, o.Target.PreparedRecipe, o.Raw, o.Target); err != nil {
			t.Fatal(err)
		}
	}
	completed := bindAfterOwnerFilesResults(t, c, integration.value)
	instance := ports.InstanceInfo{Name: integration.value.Instance, Project: integration.value.Project, Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
	incus.Instances[instance.Project+"/"+instance.Name] = instance
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
	target, hash, err := c.ResolveAfterOwner(ctx, integration.value, completed)
	if err != nil {
		t.Fatal("first original physical resolve", err)
	}
	derived, err := c.InstalledRuntimeRecipe(hash)
	if err != nil {
		t.Fatal(err)
	}
	record := integrationReceiptCLIRecord{Context: c.context, Environment: c.cli.options.Environment, Originals: originals, Derived: derived, Target: target, Format: format}
	raw, _ := json.Marshal(record)
	testkit.WriteFile(t, filepath.Join(root, "integration-partial.json"), raw, 0600)
	if format == "conf" {
		executor.cutB = true
	} else if busy {
		executor.busyCommit = true
	} else {
		executor.cut = "destination"
	}
	if err := c.ApplyActivation(ctx, target); err == nil {
		t.Fatal("full-set destination cut missing")
	}
	if a, err := os.ReadFile(filepath.Join(root, "guest/home/dev/.sample/a."+format)); err != nil || (format == "conf" && string(a) != string(payload) || format != "conf" && !strings.Contains(string(a), "desired")) {
		t.Fatal("A was not really published", err)
	}
	if _, err := os.Stat(filepath.Join(root, "guest/home/dev/.sample/b.rules")); format == "conf" && !os.IsNotExist(err) {
		t.Fatal("B published before cut", err)
	}
	inventory, err := os.ReadFile(filepath.Join(root, "guest/inventory/inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct{ Entries []map[string]any }
	if json.Unmarshal(inventory, &receipt) != nil || len(receipt.Entries) != 4 {
		t.Fatal("full approved pending set", len(receipt.Entries))
	}
	for _, row := range receipt.Entries {
		if row["pending_digest"] == nil {
			t.Fatal("full-set pending entry missing")
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestAfterOwnerIntegrationsCLIFullSetCutFreshOriginalRestore$", "-test.v")
	child.Env = append(record.Environment, "PATH="+os.Getenv("PATH"), "SUBYARD_CLI_INTEGRATION_PARTIAL_CHILD="+root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh original helper: %v\n%s", err, output)
	}
}

// Establish the prior ordinary core-only selection before the original physical
// capture. Completed init owns this dispatcher; this fixture does not project
// arbitrary hook wiring through an aggregate hash.
func establishCLIHookInventory(t *testing.T, executor *integrationReceiptCLIExecutor, root string) {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(repositoryRoot(t), "internal/adapters/reconcileruntime/integration_inventory.py"))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := os.ReadFile(filepath.Join(root, "config/projects-changed.sh"))
	if err != nil {
		t.Fatal(err)
	}
	entries := []map[string]any{}
	for _, file := range []struct {
		path    string
		mode    int
		payload []byte
	}{{"/usr/local/libexec/subyard/projects-changed", 0755, dispatcher}, {"/etc/subyard/agent-project-hooks", 0644, []byte("\n")}} {
		entries = append(entries, map[string]any{"id": "_projects", "kind": "file", "path": file.path, "mode": file.mode, "digest": fmt.Sprintf("%x", sha256.Sum256(file.payload)), "content": base64.StdEncoding.EncodeToString(file.payload)})
	}
	input, _ := json.Marshal(map[string]any{"home": "/home/dev", "uid": 1000, "entries": entries})
	program := strings.NewReplacer("@STATE_ROOT@", "/var/lib/subyard/integrations", "@STATE_UID@", "0").Replace(string(source))
	for _, mode := range []string{"apply", "commit"} {
		result, err := executor.Exec(context.Background(), "", "", ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", program, mode}, Stdin: input})
		if err != nil || result.ExitCode != 0 {
			t.Fatal("ordinary prior core inventory", mode, err, string(result.Stderr))
		}
	}
}
