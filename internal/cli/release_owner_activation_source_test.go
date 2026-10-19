package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

// The Rust libtest child exercises the real private helper, durable opaque
// packing, and a second helper. This fixture makes no guest writes or Ready
// claim; all host paths and guest destinations fit ordinary Linux path limits.
func TestNativeLargeMaterializedRecipeRustRoundtrip(t *testing.T) {
	binary, helper := os.Getenv("SUBYARD_RUST_LARGE_RECIPE_TEST_BINARY"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER")
	if binary == "" || helper == "" {
		t.Skip("paired actual Rust large recipe transport")
	}
	c, _ := nativeActivationSourceFixture(t)
	nativeMaterializedSourceFiles(t, c.context.RepositoryRoot)
	root := c.context.RepositoryRoot
	if err := os.MkdirAll(filepath.Join(c.context.ConfigHome, "migrations"), 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config/release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/release-transition.json"), registry, 0o600)
	const count = 1200
	parent := root
	for index := 0; index < 15; index++ {
		parent = filepath.Join(parent, strings.Repeat("p", 190)+fmt.Sprint(index))
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	source, small := filepath.Join(parent, "rules"), filepath.Join(parent, "small")
	payload := bytes.Repeat([]byte("x"), (32<<20)-(count-1))
	testkit.WriteFile(t, source, payload, 0o600)
	testkit.WriteFile(t, small, []byte("x"), 0o600)
	names := make([]string, count)
	var settings strings.Builder
	for index := range count {
		names[index] = fmt.Sprintf("a%04d", index)
	}
	settings.WriteString("CODING_TOOL_INTEGRATIONS='" + strings.Join(names, " ") + "'\n")
	for index, name := range names {
		path := small
		if index == 0 {
			path = source
		}
		fmt.Fprintf(&settings, "AGENT_%s_COMMAND=%s\nAGENT_%s_RULES='%s'\nAGENT_%s_RULES_DEST=.%s/rules\n", name, name, name, path, name, name)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/agents.env"), []byte(settings.String()), 0o600)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetExtensions("projects", "instances", "instance_all_projects", "container_exec_user_group_cwd", "storage", "network")
	server.SetProject("subyard", map[string]any{"name": "subyard", "config": map[string]string{}})
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "status": "Running", "config": map[string]string{"volatile.uuid": "synthetic-instance-uuid", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.name": "default"}, "expanded_config": map[string]string{"volatile.uuid": "synthetic-instance-uuid", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.name": "default"}})
	state := &nativeMaterializedState{files: map[string][]byte{}}
	server.SetExecResponder(state.step)
	path := filepath.Join(testkit.TempDir(t), "large-recipe.json")
	environment := append(append([]string{}, c.cli.options.Environment...), "PATH=/usr/local/bin:/usr/bin:/bin", "SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_RUST_LARGE_RECIPE_FIXTURE="+path, "SUBYARD_RUST_NATIVE_HELPER="+helper)
	repo, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c.context.Root, err = releaseowner.IdentityOf(c.root)
	if err != nil {
		t.Fatal(err)
	}
	c.context.Repository, err = releaseowner.IdentityOf(repo)
	if err != nil {
		t.Fatal(err)
	}
	target, length := originalMaterializedHelperTarget(t, helper, environment, c.context, c.root, repo)
	if target.PreparedRecipe == "" || length <= 48<<20 {
		t.Fatal("actual helper did not cross former canonical ceiling")
	}
	descriptor, err := json.Marshal(struct {
		Context releaseowner.Context `json:"context"`
		Target  releaseowner.Target  `json:"target"`
		Length  int                  `json:"expectedLength"`
		Hash    string               `json:"expectedHash"`
	}{c.context, target, length, target.PreparedRecipe})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, descriptor, 0o600)
	t.Logf("actual canonical bytes=%d sha256=%s", length, target.PreparedRecipe)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--exact", "installed_recipe::tests::actual_large_native_recipe_roundtrip", "--nocapture")
	command.Env = environment
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual Rust recipe transport failed: %v\n%s", err, output)
	}
	t.Log(string(output))
	if state.writes != 0 || state.committed {
		t.Fatal("large transport performed guest mutations")
	}
}

type nativeMaterializedState struct {
	mu        sync.Mutex
	files     map[string][]byte
	committed bool
	writes    int
}

func (s *nativeMaterializedState) step(call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
	s.mu.Lock()
	defer s.mu.Unlock()
	command := call.Command
	if len(command) == 3 && command[0] == "sha256sum" {
		payload, exists := s.files[command[2]]
		if !exists {
			return testkit.IncusServerExecStep{ExitCode: 1}
		}
		return testkit.IncusServerExecStep{Stdout: []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(payload), command[2]))}
	}
	if len(command) >= 5 && command[0] == "python3" && strings.Contains(command[3], "Version 1 records observed ownership only") {
		switch command[4] {
		case "observe":
			raw, _ := json.Marshal(map[string]any{"fingerprint": strings.Repeat("a", 64), "changed": !s.committed, "initial": false, "facts": []any{}})
			return testkit.IncusServerExecStep{Stdout: raw}
		case "commit":
			s.committed = true
		}
		return testkit.IncusServerExecStep{}
	}
	if len(command) == 4 && command[0] == "bash" && command[3] == "--observe" {
		return testkit.IncusServerExecStep{Stdout: []byte(`{"projects":"` + strings.Repeat("b", 64) + `","wiring":"` + strings.Repeat("c", 64) + `","hooks":[],"roots":[],"facts":{},"resolved":{}}`)}
	}
	if len(command) >= 7 && command[0] == "sh" && strings.Contains(command[3], "mktemp") {
		return testkit.IncusServerExecStep{AfterInput: func(received testkit.IncusServerExecCall) int {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.files[command[5]] = append([]byte(nil), received.Stdin...)
			s.writes++
			return 0
		}}
	}
	if strings.Contains(strings.Join(command, " "), "mode=$1") {
		return testkit.IncusServerExecStep{Stdout: []byte("changed=0\n")}
	}
	return testkit.IncusServerExecStep{}
}

func (s *nativeMaterializedState) Exec(_ context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	call := testkit.IncusServerExecCall{Project: project, Name: name, Command: request.Command, Environment: request.Environment, Stdin: request.Stdin}
	step := s.step(call)
	exit := step.ExitCode
	if step.AfterInput != nil {
		exit = step.AfterInput(call)
	}
	return ports.InstanceExecResult{Stdout: step.Stdout, Stderr: step.Stderr, ExitCode: exit}, nil
}

func nativeMaterializedSourceFiles(t *testing.T, root string) {
	t.Helper()
	for path, content := range map[string]string{
		"config/agents.env":                 "CODING_TOOL_INTEGRATIONS=sample\nAGENT_sample_COMMAND=sample\nAGENT_sample_RULES='" + filepath.Join(root, "config/agents/sample/rules") + "'\nAGENT_sample_RULES_DEST=.sample/rules\n",
		"config/agents/sample/rules":        "approved original materialized rules\n",
		"config/projects-changed.sh":        "#!/bin/sh\nexit 0\n",
		"scripts/reconcile-integrations.sh": "#!/bin/sh\nprintf approved >\"$SUBYARD_HOME/integration-effect\"\nprintf x >>\"$SUBYARD_HOME/integration-count\"\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, full, []byte(content), 0o755)
	}
}

func TestNativeMaterializedTargetsBindOriginalRecipes(t *testing.T) {
	c, _ := nativeActivationSourceFixture(t)
	if err := os.MkdirAll(c.context.DataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeMaterializedSourceFiles(t, c.context.RepositoryRoot)
	incus := c.cli.options.Incus.(*testkit.Incus)
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
	state := &nativeMaterializedState{files: map[string][]byte{}}
	c.cli.options.Executor = state
	if _, _, err := c.CaptureInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	targets, err := c.CaptureActivation(context.Background(), "materialized-config", false)
	if err != nil || len(targets) != 2 {
		t.Fatalf("materialized capture: targets=%d err=%v", len(targets), err)
	}
	for _, target := range targets {
		if target.PreparedRecipe == "" {
			t.Fatal("target lacks original payload recipe", target.Class)
		}
		raw, err := c.InstalledRuntimeRecipe(target.PreparedRecipe)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.BindInstalledRuntimeRecipe(context.Background(), target.PreparedRecipe, raw); err != nil {
			t.Fatal(err)
		}
		if destination := os.Getenv("SUBYARD_RUST_INSTALLED_RECIPE_CORPUS"); destination != "" {
			appendCapturedRecipeCorpus(t, destination, target, raw)
		}
		if err := c.ApplyActivation(context.Background(), target); err != nil {
			t.Fatal("original component dispatch", target.Class, err)
		}
	}
	if state.writes == 0 || !state.committed {
		t.Fatal("original config/integration recipes did not execute")
	}
	if body, err := os.ReadFile(filepath.Join(c.context.DataHome, "integration-count")); err != nil || string(body) != "x" {
		t.Fatal("integration recipe did not run exactly once", err)
	}
	fresh, err := c.CaptureActivation(context.Background(), "materialized-config", true)
	if err != nil || len(fresh) != len(targets) {
		t.Fatal("post-effect capture", err)
	}
	for index, target := range targets {
		if fresh[index].Source != target.Source || fresh[index].Desired != target.Desired || fresh[index].InstanceUUID != target.InstanceUUID || fresh[index].State != "current" {
			t.Fatalf("own progress changed immutable materialized source: class=%s sourceEqual=%t desiredEqual=%t state=%s", target.Class, fresh[index].Source == target.Source, fresh[index].Desired == target.Desired, fresh[index].State)
		}
	}
}

func TestNativeInactiveBrokerDoesNotLoadAbsentCanonicalRegistration(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	root := c.context.RepositoryRoot
	profile, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config/yards/profiles/test-vms.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(root, "config/yards/profiles"), filepath.Join(home, "yards"), filepath.Join(root, "tools")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "config/yards/profiles/test-vms.env"), profile, 0o600)
	testkit.WriteFile(t, filepath.Join(home, "yards/e2e-yard.env"), []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\nNESTED_E2E_VMS=0\n"), 0o600)
	incus := filepath.Join(root, "tools/incus")
	testkit.WriteFile(t, incus, []byte(`#!/bin/sh
case "$*" in
  'project list --format=json') printf '[{"name":"subyard-test-yard"}]\n';;
  'project get subyard-test-yard features.images') printf 'false\n';;
  '--version') printf '6.23.0\n';;
  list*) printf '[]\n';;
  *) exit 91;;
esac
`), 0o755)
	t.Setenv("PATH", filepath.Join(root, "tools")+":"+os.Getenv("PATH"))
	if _, _, err := c.CaptureInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	options, err := c.cli.releaseTransitionTestYardOptions(c.request())
	if err != nil {
		t.Fatal(err)
	}
	options.ProspectiveFiles = c.view()
	fact, yard, err := testyardmigration.PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil || fact != testyardmigration.BrokerRuntimeInactive || yard != "test-yard" {
		t.Fatalf("owning applicability: %s %s %v", fact, yard, err)
	}
	if _, err := c.load(yard); err == nil {
		t.Fatal("fixture unexpectedly has canonical registration")
	}
	values, err := c.CaptureActivation(context.Background(), "test-vm-broker", false)
	if err != nil || len(values) != 1 {
		t.Fatalf("inactive native observation: %v", err)
	}
	target := values[0]
	if target.Yard != yard || target.State != "current" || target.ScopeState != string(fact) || target.Actual != target.Desired || target.PreparedRecipe != "" || target.Source == "" {
		t.Fatal("native lost inactive owning fact", target)
	}
	prepared := c.activation["test-vm-broker:"+yard+":stage"]
	if prepared.platform != nil || prepared.broker != nil || prepared.nativeInstalled != nil {
		t.Fatal("inactive observation constructed execution handles")
	}
	owning := c.cli.brokerActivationWithFiles(c.request(), c.view())
	owning.platform = func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
		t.Fatal("owning inactive observation loaded platform")
		return nil, nil
	}
	observed, err := owning.Observe(context.Background(), c.context.Releases, releasetransition.ReleaseLinks{})
	if err != nil || !observed.Converged || observed.Actual != observed.Desired {
		t.Fatalf("owning observer: %+v %v", observed, err)
	}
}

func nativeActivationSourceFixture(t *testing.T) (*releaseOwnerComponents, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	home := filepath.Join(root, "state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	if instance.Config == nil {
		instance.Config = map[string]string{}
	}
	instance.Config["volatile.uuid"] = "synthetic-instance-uuid"
	incus.Instances["subyard/yard"] = instance
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	c := &releaseOwnerComponents{cli: program, root: fd, context: releaseowner.Context{Session: strings.Repeat("b", 64), Artifact: strings.Repeat("a", 64), Releases: releasetransition.ReleasePair{From: "old", Target: "new"}, Direction: releasetransition.DirectionActivateTarget, RepositoryRoot: root, ConfigHome: home, DataHome: filepath.Join(root, "data"), RuntimeRoot: filepath.Join(root, "runtime"), Target: "new", Yard: "default"}, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, prepared: map[string]releaseSettingsPrepared{}, activation: map[string]releaseActivationPrepared{}}
	t.Cleanup(func() { c.Close() })
	return c, home
}

func TestNativeActivationSourceIgnoresUnrelatedEarlierComponentReads(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	if _, _, err := c.CaptureInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	// An earlier migration may inspect a source file that this activation does
	// not consume. Its presence must not change a later component's recipe.
	testkit.WriteFile(t, filepath.Join(home, "unrelated-migration.env"), []byte("SYNTHETIC=1\n"), 0o600)
	if _, err := c.read("unrelated-migration.env"); err != nil {
		t.Fatal(err)
	}
	first, err := c.CaptureActivation(context.Background(), "host-power", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CaptureActivation(context.Background(), "host-power", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) || len(first) == 0 {
		t.Fatal("missing component targets", first, second)
	}
	for index := range first {
		if first[index].Source != second[index].Source {
			t.Fatal("unrelated earlier read changed component source", first[index].Source, second[index].Source)
		}
	}
}

func TestNativeRepeatedFullComponentCaptureHasSameFacts(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	if err := os.MkdirAll(filepath.Join(home, ".sync/settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0o600)
	capture := func() string {
		t.Helper()
		ctx := context.Background()
		names, owners, err := c.CaptureInventory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ingress, err := c.CaptureIngress(ctx, nil)
		if err != nil {
			t.Fatal("ingress", err)
		}
		var raw, resolved []releaseowner.YardSettings
		for _, yard := range names {
			value, err := c.CaptureYard(ctx, yard, false)
			if err != nil {
				t.Fatal("raw yard", err)
			}
			raw = append(raw, value)
		}
		owner, err := c.CaptureOwner(ctx)
		if err != nil {
			t.Fatal("owner", err)
		}
		for _, yard := range names {
			value, err := c.CaptureYard(ctx, yard, true)
			if err != nil {
				t.Fatal("resolved yard", err)
			}
			resolved = append(resolved, value)
		}
		port := "2240"
		settings, err := c.PrepareSettings(ctx, "default", []releaseowner.Edit{{Setting: "SSH_PORT", Value: &port}})
		if err != nil {
			t.Fatal("settings", err)
		}
		targets := map[string][]releaseowner.Target{}
		for _, id := range append(owners, "materialized-config", "core-guest-runtime", "key-tools-runtime", "test-vm-broker", "test-yard-route-consumers", "host-power") {
			values, err := c.CaptureActivation(ctx, id, false)
			if err != nil {
				t.Fatalf("activation %s: %v", id, err)
			}
			targets[id] = values
		}
		body, err := json.Marshal(struct {
			Ingress       releaseowner.IngressFacts
			Raw, Resolved []releaseowner.YardSettings
			Owner         releaseowner.OwnerBefore
			Settings      releaseowner.PreparedSettings
			Targets       map[string][]releaseowner.Target
		}{ingress, raw, resolved, owner, settings, targets})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	first, second := capture(), capture()
	if first != second {
		t.Fatalf("same actual snapshot changed repeated component facts: %s != %s", nativeDigest([]byte(first)), nativeDigest([]byte(second)))
	}
	var approved struct {
		Targets map[string][]releaseowner.Target
	}
	if err := json.Unmarshal([]byte(first), &approved); err != nil {
		t.Fatal(err)
	}
	port := "2240"
	for _, prepared := range c.prepared {
		value := prepared.value
		recipe := releaseowner.SettingsRecipe{Yard: value.Yard, Target: value.Target, ExpectedExists: value.ExpectedExists, ExpectedFile: value.ExpectedFile, DesiredFile: value.DesiredFile, Source: value.Source, Inputs: value.Inputs, Edits: []releaseowner.Edit{{Setting: "SSH_PORT", Value: &port}}}
		if err := c.ApplySettings(context.Background(), recipe); err != nil {
			t.Fatal("authorized same-value settings CAS", err)
		}
	}
	for owner, targets := range approved.Targets {
		fresh, err := c.CaptureActivation(context.Background(), owner, true)
		if err != nil {
			t.Fatal(owner, err)
		}
		if len(fresh) != len(targets) {
			t.Fatal("authorized settings CAS changed component resource set", owner)
		}
		for index, target := range targets {
			if fresh[index].Source != target.Source || fresh[index].Desired != target.Desired || fresh[index].Project != target.Project || fresh[index].InstanceUUID != target.InstanceUUID {
				t.Fatalf("authorized same-value CAS changed approved component %s", owner)
			}
		}
	}
}

func TestNativeComponentLoadPreservesDistinctPinnedRoots(t *testing.T) {
	c, _ := nativeActivationSourceFixture(t)
	loaded, err := c.load("default")
	if err != nil {
		t.Fatal(err)
	}
	paths := loaded.Context.Paths
	if paths.DataHome != c.context.DataHome || paths.ConfigHome != c.context.ConfigHome || paths.OperatorHome != c.cli.env["SUBYARD_OPERATOR_HOME"] {
		t.Fatal("native component changed an owning root role")
	}
	if paths.DataHome == paths.ConfigHome || paths.OperatorHome == paths.ConfigHome || paths.DataHome == paths.OperatorHome {
		t.Fatal("fixture did not distinguish all root roles")
	}
}
