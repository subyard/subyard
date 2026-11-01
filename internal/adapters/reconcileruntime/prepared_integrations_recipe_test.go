package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerroute"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMaterializedIntegrationExecutionUsesClosedCachedDefinitionsAndAssets(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, _ := materializedIntegrationFixture(t, root, true)
	hook := filepath.Join(root, "config/profiles/synthetic/guest.sh")
	if err := os.MkdirAll(filepath.Dir(hook), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, hook, []byte("# approved guest environment\n"), 0755)
	runtime.Environment = append(runtime.Environment, "SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS="+hook)
	metadata := []byte(`{"schema_version":1,"lifecycle":{"handler":"runtime.sh","source_files":["payload.txt"]}}`)
	testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/integration.json"), metadata, 0o600)
	runtime.Integrations = nil
	runtime.Profiles = nil
	p := prepareMaterializedIntegrationFixture(t, runtime)
	defer p.Close()
	testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/integration.json"), []byte(`{"schema_version":1,"lifecycle":{"handler":"unapproved.sh"}}`), 0o600)
	execution, err := runtime.materializedIntegrationExecution(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(execution.Integrations) != 1 || execution.Integrations[0].Lifecycle == nil || execution.Integrations[0].Lifecycle.Handler != "runtime.sh" {
		t.Fatal("execution recaptured changed definition")
	}
	asset := config.MaterializedAsset{Source: filepath.Join(p.directory, "config/agents/sample/payload.txt")}
	if body, err := execution.SourceReader(asset); err != nil || string(body) != "approved dependency\n" {
		t.Fatal("cached asset unavailable", err)
	}
	foreign := filepath.Join(root, "unapproved-asset")
	testkit.WriteFile(t, foreign, []byte("unapproved"), 0o600)
	if _, err := execution.SourceReader(config.MaterializedAsset{Source: foreign}); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("execution read an uncaptured live asset", err)
	}
	helper, err := os.ReadFile("../../../scripts/lib/engine-context.sh")
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{execution.environmentValue("SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS"), hook} {
		command := exec.Command("bash", "--noprofile", "--norc", "-peu", "-s", "--", filepath.Join(p.directory, "config/profiles"), path)
		command.Env = execution.Environment
		command.Stdin = strings.NewReader(string(helper) + "\nincus(){ cat; }\nsubyard_apply_profile_guest_environment_hooks \"$1\" yard project dev \"$2\"\n")
		output, err := command.CombinedOutput()
		if i == 0 && (err != nil || string(output) != "# approved guest environment\n") {
			t.Fatal("retained guest hook rejected by the SSH component boundary", err)
		}
		if i == 1 && err == nil {
			t.Fatal("live hook escaped the retained profile root")
		}
	}
}

func TestMaterializedIntegrationNativeScopeUsesOwnPinnedRoot(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, _ := materializedIntegrationFixture(t, root, true)
	for _, relative := range []string{"config/agents/sample/bin/native", "go.mod", ".build/agents/sample/bin/native"} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte("approved"), 0o700)
	}
	fd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	runtime.RepositoryRoot = "/proc/self/fd/" + strconv.Itoa(int(fd.Fd()))
	runtime.LogicalRepositoryRoot = root
	runtime.Integrations[0].Root = filepath.Join(runtime.RepositoryRoot, "config/agents/sample")
	runtime.Integrations[0].Native = []profile.Native{{Path: "bin/native"}}
	for _, fallback := range []bool{false, true} {
		if fallback {
			if err := os.Remove(filepath.Join(root, "config/agents/sample/bin/native")); err != nil {
				t.Fatal(err)
			}
		}
		paths, err := runtime.materializedIntegrationPaths()
		if err != nil {
			t.Fatal("own checked root alias", err)
		}
		want := "config/agents/sample/bin/native"
		if fallback {
			want = ".build/agents/sample/bin/native"
		}
		if !slices.Contains(paths, want) {
			t.Fatal("selected native asset omitted")
		}
	}
	runtime.Integrations[0].Root = "/proc/1/fd/" + strconv.Itoa(int(fd.Fd())) + "/config/agents/sample"
	if _, err := runtime.materializedIntegrationPaths(); err == nil {
		t.Fatal("foreign process root admitted")
	}
}

type materializedIntegrationCut struct {
	*retryableIntegrationExecutor
	cut func() error
}

func (e *materializedIntegrationCut) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if e.cut != nil && len(request.Command) > 5 && request.Command[5] == "apply" && strings.Contains(strings.Join(request.Command, " "), "mode=$1") {
		cut := e.cut
		e.cut = nil
		if err := cut(); err != nil {
			return ports.InstanceExecResult{}, err
		}
	}
	return e.retryableIntegrationExecutor.Exec(ctx, project, name, request)
}

func materializedIntegrationFixture(t *testing.T, root string, create bool) (Runtime, *retryableIntegrationExecutor) {
	t.Helper()
	if create {
		files := map[string]string{
			"config/projects-changed.sh":            "#!/bin/sh\nexit 0\n",
			"scripts/reconcile-integrations.sh":     "#!/bin/sh\ncat \"$AGENT_sample_PROVISION\" >\"$RESULT\"\nprintf installed >\"$HOOK_STATE\"\n",
			"config/agents/sample/integration.json": `{"schema_version":1}`,
			"config/agents/sample/runtime.sh": `#!/bin/sh
case "$1" in
observe) cat "$LIFECYCLE_STATE";;
apply) printf approved-lifecycle >"$LIFECYCLE_RESULT"; cp "$LIFECYCLE_NEXT" "$LIFECYCLE_STATE"; cat "$LIFECYCLE_STATE";;
*) exit 92;;
esac
`,
			"config/agents/sample/payload.txt": "approved dependency\n",
			"provision.sh":                     "#!/bin/sh\n# approved original provision\n",
			"lifecycle-state":                  `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `"}`,
			"lifecycle-next":                   `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `"}`,
		}
		for path, content := range files {
			full := filepath.Join(root, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, full, []byte(content), 0o755)
		}
	}
	executor := &retryableIntegrationExecutor{pending: true}
	executor.observeHooks = func() hookObservation {
		observation := hookObservation{Projects: testRuntimeOldDigest, Wiring: testRuntimeNewDigest}
		if _, err := os.Stat(filepath.Join(root, "hook-state")); err == nil {
			observation.Hooks = []string{"sample-projects"}
			observation.Facts = map[string]string{"sample-projects": testRuntimeNewDigest}
			observation.Resolved = map[string]string{"sample-projects": "/usr/local/bin/sample-projects"}
		}
		return observation
	}
	incus := runningIncus(0)
	incus.Reconcile.Instance.Config = map[string]string{"volatile.uuid": "original-instance"}
	return Runtime{RepositoryRoot: root, Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{{Name: "sample", Root: filepath.Join(root, "config/agents/sample"), Lifecycle: &profile.IntegrationLifecycle{Handler: "runtime.sh", SourceFiles: []string{"payload.txt"}}}}, Incus: incus, Executor: executor,
		Yard: domain.Context{IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}, Environment: []string{"PATH=/usr/bin:/bin", "SUBYARD_OPERATION_ID=materialized-fixture", "CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_COMMAND=sample", "AGENT_sample_PROJECTS_CHANGED=sample-projects", "AGENT_sample_PROVISION=" + filepath.Join(root, "provision.sh"), "RESULT=" + filepath.Join(root, "result"), "HOOK_STATE=" + filepath.Join(root, "hook-state"), "LIFECYCLE_STATE=" + filepath.Join(root, "lifecycle-state"), "LIFECYCLE_NEXT=" + filepath.Join(root, "lifecycle-next"), "LIFECYCLE_RESULT=" + filepath.Join(root, "lifecycle-result")}}, executor
}

func prepareMaterializedIntegrationFixture(t *testing.T, runtime Runtime) *PreparedInstalledRuntime {
	t.Helper()
	plan, err := runtime.IntegrationPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := runtime.PrepareIntegrations(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	p, err := runtime.PrepareMaterializedIntegrations(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMaterializedIntegrationFreshProcessOriginalRecipe(t *testing.T) {
	if root := os.Getenv("SUBYARD_INTEGRATION_RECIPE_CHILD"); root != "" {
		runtime, executor := materializedIntegrationFixture(t, root, false)
		raw, err := os.ReadFile(filepath.Join(root, "original.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if err := runtime.ApplyPreparedMaterializedIntegrations(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if executor.pending || executor.commits != 1 || executor.hookAttempts != 1 {
			t.Fatalf("original prepared integration did not close: pending=%t commits=%d hooks=%d", executor.pending, executor.commits, executor.hookAttempts)
		}
		payload, err := os.ReadFile(filepath.Join(root, "result"))
		if err != nil || !bytes.Contains(payload, []byte("approved original provision")) {
			t.Fatal("original provision missing", err)
		}
		payload, err = os.ReadFile(filepath.Join(root, "lifecycle-result"))
		if err != nil || string(payload) != "approved-lifecycle" {
			t.Fatal("original lifecycle missing", err)
		}
		return
	}
	root := testkit.TempDir(t)
	runtime, _ := materializedIntegrationFixture(t, root, true)
	p := prepareMaterializedIntegrationFixture(t, runtime)
	defer p.Close()
	testkit.WriteFile(t, filepath.Join(root, "original.recipe"), p.Recipe(), 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestMaterializedIntegrationFreshProcessOriginalRecipe$", "-test.v")
	command.Env = append(os.Environ(), "SUBYARD_INTEGRATION_RECIPE_CHILD="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, output)
	}
}

func TestRebindPreparedIntegrationRetainsOriginalAndChecksFreshSources(t *testing.T) {
	for _, change := range []string{"content", "identity"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime, _ := materializedIntegrationFixture(t, root, true)
			original := prepareMaterializedIntegrationFixture(t, runtime)
			defer original.Close()
			decoded, err := runtime.BindInstalledRuntimeContext(context.Background(), original.Recipe())
			if err != nil {
				t.Fatal(err)
			}
			defer decoded.Close()
			rebound, err := runtime.RebindPreparedInstalledRuntime(context.Background(), original)
			if err != nil {
				t.Fatal(err)
			}
			defer rebound.Close()
			if rebound.Binding() != decoded.Binding() || rebound.Binding() != original.Binding() || !bytes.Equal(rebound.Recipe(), decoded.Recipe()) || !bytes.Equal(rebound.Recipe(), original.Recipe()) {
				t.Fatal("rebound recipe differs from the original capsule")
			}
			for path, payload := range original.files {
				if len(payload) == 0 {
					continue
				}
				before := payload[0]
				rebound.files[path][0] ^= 1
				if payload[0] != before {
					t.Fatal("rebound recipe aliases original payload")
				}
				rebound.files[path][0] ^= 1
				break
			}
			path := filepath.Join(root, "provision.sh")
			if change == "content" {
				testkit.WriteFile(t, path, []byte("unapproved\n"), 0o755)
			} else {
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path+".new", payload, 0o755)
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := runtime.RebindPreparedInstalledRuntime(context.Background(), original); err == nil {
				t.Fatal("rebind accepted changed source")
			}
		})
	}
}

func TestRebindPreparedIntegrationPreservesJSONNormalization(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, _ := materializedIntegrationFixture(t, root, true)
	original := prepareMaterializedIntegrationFixture(t, runtime)
	defer original.Close()
	// These omitted empty slices decode as nil on the established wire path.
	original.native = []installedSourceGuard{}
	original.afterOwner = []byte{}
	decoded, err := runtime.BindInstalledRuntimeContext(context.Background(), original.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Close()
	rebound, err := runtime.RebindPreparedInstalledRuntime(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	defer rebound.Close()
	if rebound.Binding() != decoded.Binding() || !bytes.Equal(rebound.Recipe(), decoded.Recipe()) {
		t.Fatal("empty and nil fields changed the canonical recipe")
	}
	// Invalid UTF-8 is normalized by JSON decoding and must use the old path.
	original.files[string([]byte{0xff})] = []byte("untrusted")
	_, decodedErr := runtime.BindInstalledRuntimeContext(context.Background(), original.Recipe())
	_, reboundErr := runtime.RebindPreparedInstalledRuntime(context.Background(), original)
	if decodedErr == nil || reboundErr == nil || decodedErr.Error() != reboundErr.Error() {
		t.Fatal("invalid UTF-8 differed from strict decoding", decodedErr, reboundErr)
	}
}

func TestMaterializedIntegrationBeforeDispatchSourceAndScopeGuards(t *testing.T) {
	for _, change := range []string{"wrapper", "provision", "lifecycle", "identity", "uuid", "hook-scope", "metadata"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime, executor := materializedIntegrationFixture(t, root, true)
			p := prepareMaterializedIntegrationFixture(t, runtime)
			defer p.Close()
			before := len(executor.requests)
			switch change {
			case "wrapper":
				testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
			case "provision":
				testkit.WriteFile(t, filepath.Join(root, "provision.sh"), []byte("unapproved\n"), 0o755)
			case "lifecycle":
				testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/runtime.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
			case "identity":
				path := filepath.Join(root, "provision.sh")
				payload, _ := os.ReadFile(path)
				testkit.WriteFile(t, path+".new", payload, 0o755)
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			case "uuid":
				runtime.Incus.(*testkit.Incus).Reconcile.Instance.Config["volatile.uuid"] = "replacement-instance"
			case "hook-scope":
				executor.observeHooks = func() hookObservation {
					return hookObservation{Projects: testRuntimeNewDigest, Wiring: testRuntimeNewDigest, Hooks: []string{"foreign-hook"}}
				}
			case "metadata":
				original, _, err := decodeMaterializedMetadata(p)
				if err != nil {
					t.Fatal(err)
				}
				original.Integrations.Sources[0].Layout = "../../outside"
				if err := writeMaterializedMetadata(p, original); err != nil {
					t.Fatal(err)
				}
			}
			if err := runtime.ApplyPreparedMaterializedIntegrations(context.Background(), p); err == nil {
				t.Fatal("changed original scope dispatched")
			}
			if executor.commits != 0 || executor.hookAttempts != 0 {
				t.Fatal("refusal mutated integration")
			}
			for _, request := range executor.requests[before:] {
				if len(request.Command) > 5 && request.Command[5] == "apply" || len(request.Command) > 4 && request.Command[4] == "apply" {
					t.Fatal("before-dispatch refusal made an effect")
				}
			}
			if _, err := os.Stat(filepath.Join(root, "result")); !os.IsNotExist(err) {
				t.Fatal("changed source executed")
			}
		})
	}
}

func TestMaterializedIntegrationAfterGuardConsumesOriginalSources(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, executor := materializedIntegrationFixture(t, root, true)
	p := prepareMaterializedIntegrationFixture(t, runtime)
	defer p.Close()
	runtime.Executor = &materializedIntegrationCut{retryableIntegrationExecutor: executor, cut: func() error {
		for _, path := range []string{"scripts/reconcile-integrations.sh", "provision.sh", "config/agents/sample/runtime.sh"} {
			if err := os.WriteFile(filepath.Join(root, path), []byte("#!/bin/sh\nprintf unapproved >\"$RESULT\"\nexit 93\n"), 0o755); err != nil {
				return err
			}
		}
		return nil
	}}
	err := runtime.ApplyPreparedMaterializedIntegrations(context.Background(), p)
	// Original byte execution is independent of the final observation's honest
	// refusal of newly changed external source facts.
	if err == nil || !errors.Is(err, domain.ErrPlanStale) && !strings.Contains(err.Error(), "verify") {
		t.Fatalf("post-guard source drift outcome: %v", err)
	}
	payload, readErr := os.ReadFile(filepath.Join(root, "result"))
	if readErr != nil || !bytes.Contains(payload, []byte("approved original provision")) || bytes.Contains(payload, []byte("unapproved")) {
		t.Fatal("post-guard changed bytes executed", readErr)
	}
	payload, readErr = os.ReadFile(filepath.Join(root, "lifecycle-result"))
	if readErr != nil || string(payload) != "approved-lifecycle" {
		t.Fatal("post-guard changed lifecycle executed", readErr)
	}
}

func TestMaterializedIntegrationSourceExcludesOwnProgress(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, _ := materializedIntegrationFixture(t, root, true)
	p := prepareMaterializedIntegrationFixture(t, runtime)
	defer p.Close()
	original, _, err := decodeMaterializedMetadata(p)
	if err != nil {
		t.Fatal(err)
	}
	changed := original
	raw, _ := json.Marshal(original.Integrations)
	var copy materializedIntegrations
	if err := json.Unmarshal(raw, &copy); err != nil {
		t.Fatal(err)
	}
	changed.Integrations = &copy
	changed.Integrations.Plan.Plan.Changed = false
	changed.Integrations.Plan.Plan.Fingerprint = testRuntimeNewDigest
	changed.Integrations.Hooks.Completed = true
	if original.Integrations.sourceBinding(original.Configs) != changed.Integrations.sourceBinding(changed.Configs) {
		t.Fatal("own native progress entered immutable source")
	}
	if installedRuntimeDigest(original) == installedRuntimeDigest(changed) {
		t.Fatal("original baseline missing from durable recipe")
	}
}

func TestOriginalIntegrationCoreEndpointProjection(t *testing.T) {
	previewLoopbackFixture(t)
	for _, change := range []string{"endpoint-only", "combined-profile", "closed-core", "original-absence", "unchanged-no-core", "foreign-config", "uuid", "wrong-endpoint", "local-mismatch", "missing-core", "wrong-desired", "core-source", "stale-core"} {
		t.Run(change, func(t *testing.T) {
			root := preparedInstalledFixture(t).RepositoryRoot
			runtime, _ := materializedIntegrationFixture(t, root, true)
			runtime.Environment = append(runtime.Environment, "ALLOWS_CODING_TOOLS=false", "WEB_PREVIEW_HOST_PORT=32222")
			for _, relative := range []string{"config/preview/subyard-preview", "config/agents/ccusage/provision.sh", "scripts/refresh-installed-guest-runtime.sh", "scripts/prepare-preview-route.sh"} {
				path := filepath.Join(root, relative)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("fixed approved source\n"), 0755)
			}
			combined := change == "combined-profile" || change == "foreign-config"
			if combined {
				profileRuntimeFixtureAt(t, root, "#!/bin/sh\nprintf '{\"state\":\"current\",\"actual\":\""+testRuntimeNewDigest+"\",\"desired\":\""+testRuntimeNewDigest+"\"}\\n'\n")
				definitions, err := profile.Load(root)
				if err != nil {
					t.Fatal(err)
				}
				definitions[0].Dashboard = &profile.IntegrationDashboard{PortSetting: "SYNTHETIC_PORT", GuestPort: 3030, ContainerRoute: &profile.IntegrationRoute{MarkerKey: "user.synthetic.route", DeviceName: "synthetic-route"}}
				runtime.Profiles = definitions
				runtime.Environment = append(runtime.Environment, "ENVIRONMENT_PROFILES=synthetic", "SYNTHETIC_PORT=32223")
			}
			fake := runtime.Incus.(*testkit.Incus)
			fake.Reconcile.Instance.Config["user.subyard.preview_sha256"] = strings.Repeat("b", 64)
			fake.Reconcile.Instance.Config["user.subyard.preview_endpoint_sha256"] = strings.Repeat("a", 64)
			fake.Reconcile.Instance.LocalConfig = map[string]string{"user.subyard.preview_sha256": strings.Repeat("b", 64), "user.subyard.preview_endpoint_sha256": strings.Repeat("a", 64)}
			if combined {
				fake.Reconcile.Instance.Devices = map[string]map[string]string{"eth0": {"type": "nic", "network": "fixture"}}
				fake.Reconcile.Instance.LocalDevices = map[string]map[string]string{"eth0": {"type": "nic", "network": "fixture"}}
			}
			fake.Instances = map[string]ports.InstanceInfo{"test/yard": fake.Reconcile.Instance}
			if change == "original-absence" {
				delete(fake.Reconcile.Instance.Config, "user.subyard.preview_endpoint_sha256")
				delete(fake.Reconcile.Instance.LocalConfig, "user.subyard.preview_endpoint_sha256")
			}
			core, err := runtime.PrepareInstalledRuntime(context.Background(), "core-guest-runtime")
			if err != nil {
				t.Fatal(err)
			}
			defer core.Close()
			originalWitness, err := runtime.ObserveInstalledRuntime(context.Background(), "core-guest-runtime")
			if err != nil {
				t.Fatal(err)
			}
			original := prepareMaterializedIntegrationFixture(t, runtime)
			defer original.Close()
			expected, err := runtime.previewEndpointHash(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fake.Reconcile.Instance.Config["user.subyard.preview_endpoint_sha256"] = expected
			fake.Reconcile.Instance.LocalConfig["user.subyard.preview_endpoint_sha256"] = expected
			if combined {
				fake.Reconcile.Instance.Config["user.synthetic.route"] = "v1:32223"
				fake.Reconcile.Instance.LocalConfig["user.synthetic.route"] = "v1:32223"
				fake.Reconcile.Instance.Devices["synthetic-route"] = ownerroute.Device("127.0.0.1", "32223", 3030)
				fake.Reconcile.Instance.LocalDevices["synthetic-route"] = ownerroute.Device("127.0.0.1", "32223", 3030)
				fake.Instances["test/yard"] = fake.Reconcile.Instance
			}
			switch change {
			case "closed-core":
				core.Close()
			case "foreign-config":
				fake.Reconcile.Instance.Config["user.foreign.fixture"] = "foreign"
			case "uuid":
				fake.Reconcile.Instance.Config["volatile.uuid"] = "different-fixed-instance"
			case "wrong-endpoint":
				fake.Reconcile.Instance.Config["user.subyard.preview_endpoint_sha256"] = strings.Repeat("c", 64)
				fake.Reconcile.Instance.LocalConfig["user.subyard.preview_endpoint_sha256"] = strings.Repeat("c", 64)
			case "unchanged-no-core":
				fake.Reconcile.Instance.Config["user.subyard.preview_endpoint_sha256"] = strings.Repeat("a", 64)
				fake.Reconcile.Instance.LocalConfig["user.subyard.preview_endpoint_sha256"] = strings.Repeat("a", 64)
			case "local-mismatch":
				fake.Reconcile.Instance.LocalConfig["user.subyard.preview_endpoint_sha256"] = strings.Repeat("c", 64)
			}
			witness, err := runtime.ObserveInstalledRuntime(context.Background(), "core-guest-runtime")
			if err != nil || witness.State != ports.RuntimeStateCurrent || witness.Actual == "" || witness.Actual != witness.Desired {
				t.Fatal("native core witness unavailable", err)
			}
			retained, err := runtime.BindInstalledRuntimeContext(context.Background(), original.Recipe())
			if change == "uuid" {
				if err == nil {
					retained.Close()
					t.Fatal("foreign UUID admitted original capsule")
				}
				t.Log("foreign UUID refused original capsule")
				return
			}
			if err != nil {
				t.Fatal("original capsule unavailable", err)
			}
			defer retained.Close()
			fresh, err := runtime.IntegrationPlan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			desired := originalWitness.Desired
			switch change {
			case "missing-core", "unchanged-no-core":
				core = nil
			case "wrong-desired":
				desired = strings.Repeat("0", 64)
			case "core-source":
				core.source = strings.Repeat("0", 64)
			case "stale-core":
				runtime.Executor = endpointProjectionExecutor{runtime.Executor}
			}
			compared, err := runtime.ProjectIntegrationPlanAfterCoreRuntime(context.Background(), core, desired, retained.PreparedIntegrations().Plan(), fresh)
			if err == nil {
				compared, err = runtime.ProjectIntegrationPlanAfterProfileActivation(context.Background(), retained.PreparedIntegrations().Plan(), compared)
			}
			if err == nil {
				err = CheckIntegrationPlan(retained.PreparedIntegrations().Plan(), compared)
			}
			if err == nil && change == "combined-profile" {
				reversed, reverseErr := runtime.ProjectIntegrationPlanAfterProfileActivation(context.Background(), retained.PreparedIntegrations().Plan(), fresh)
				if reverseErr == nil {
					reversed, reverseErr = runtime.ProjectIntegrationPlanAfterCoreRuntime(context.Background(), core, desired, retained.PreparedIntegrations().Plan(), reversed)
				}
				if reverseErr != nil || reversed.Scope != compared.Scope || reversed.Fingerprint != compared.Fingerprint {
					t.Fatal("sibling projections did not share the same comparison", reverseErr)
				}
			}
			if !bytes.Equal(retained.Recipe(), original.Recipe()) {
				t.Fatal("original integration capsule changed")
			}
			actualInstance, _ := json.Marshal(compared.instance)
			physicalInstance, _ := json.Marshal(fresh.instance)
			if !bytes.Equal(actualInstance, physicalInstance) {
				t.Fatal("comparison changed the raw physical instance")
			}
			positive := change == "endpoint-only" || change == "combined-profile" || change == "closed-core" || change == "original-absence" || change == "unchanged-no-core"
			if positive && err != nil {
				t.Fatal("exact core endpoint publication invalidates original integration scope", err)
			}
			if !positive && err == nil {
				t.Fatal("foreign guard accepted")
			}
		})
	}
}

type endpointProjectionExecutor struct{ ports.InstanceExecutor }

func (e endpointProjectionExecutor) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if strings.Contains(strings.Join(request.Command, "\x00"), "helper=/usr/local/bin/subyard-preview") {
		return ports.InstanceExecResult{ExitCode: 1}, errors.New("fixture preview is stale")
	}
	return e.InstanceExecutor.Exec(ctx, project, name, request)
}
