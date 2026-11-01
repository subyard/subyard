package cli

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
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
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
	for _, name := range names {
		state.physicalPaths = append(state.physicalPaths, "/home/dev/."+name+"/rules")
	}
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
	mu            sync.Mutex
	files         map[string][]byte
	committed     bool
	writes        int
	physicalPaths []string
	physicalUID   uint32
}

func (s *nativeMaterializedState) step(call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
	s.mu.Lock()
	defer s.mu.Unlock()
	command := call.Command
	if len(command) > 4 && command[0] == "python3" && command[4] == "configs-observe" {
		paths := s.physicalPaths
		if paths == nil {
			paths = []string{"/home/dev/.sample/rules"}
		}
		rows := []map[string]any{}
		for _, path := range paths {
			payload, present := s.files[path]
			digest, uid, mode := "", uint32(0), uint32(0)
			if present {
				digest = fmt.Sprintf("%x", sha256.Sum256(payload))
				uid = s.physicalUID
				mode = 0644
			}
			rows = append(rows, map[string]any{"exists": present, "digest": digest, "uid": uid, "gid": uid, "mode": mode})
		}
		raw, _ := json.Marshal(rows)
		return testkit.IncusServerExecStep{Stdout: raw}
	}
	if len(command) > 4 && command[0] == "python3" && command[4] == "configs-write" {
		return testkit.IncusServerExecStep{AfterInput: func(received testkit.IncusServerExecCall) int {
			var value struct{ UID uint32 }
			if json.Unmarshal(received.Stdin, &value) != nil {
				return 1
			}
			_, err := (afterOwnerConfigTestState{s}).Exec(context.Background(), received.Project, received.Name, ports.InstanceExecRequest{Command: received.Command, Stdin: received.Stdin})
			s.mu.Lock()
			s.physicalUID = value.UID
			s.mu.Unlock()
			if err != nil {
				return 1
			}
			return 0
		}}
	}
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
	if len(request.Command) > 4 && request.Command[0] == "python3" && (request.Command[4] == "configs-observe" || request.Command[4] == "configs-write") {
		return (afterOwnerConfigTestState{s}).Exec(context.Background(), project, name, request)
	}
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
	// The shared identity capture preserves both original materialized recipes.
	probe := &nativeActivationInstanceProbe{Incus: incus, first: os.ErrNotExist, later: os.ErrNotExist}
	c.cli.options.Incus = nativeActivationAbsenceProbe{probe, true}
	loaded, err := c.loadActivation("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		bound, err := c.bindGuestTarget(t.Context(), loaded, target)
		target.InstanceUUID = "absent"
		if err != nil || bound != target || probe.calls != 0 {
			t.Fatal("cold identity capture changed an original materialized recipe", target.Class, err)
		}
	}
}

func TestNativeColdMaterializedCaptureRequiresPositiveAbsenceProof(t *testing.T) {
	for _, proof := range []string{"cold", "false-proof", "no-proof"} {
		t.Run(proof, func(t *testing.T) {
			c, _ := nativeActivationSourceFixture(t)
			if err := os.MkdirAll(c.context.DataHome, 0700); err != nil {
				t.Fatal(err)
			}
			nativeMaterializedSourceFiles(t, c.context.RepositoryRoot)
			probe := &nativeActivationInstanceProbe{Incus: c.cli.options.Incus.(*testkit.Incus), first: os.ErrNotExist, later: os.ErrNotExist}
			c.cli.options.Incus = probe
			if proof != "no-proof" {
				c.cli.options.Incus = nativeActivationAbsenceProbe{probe, proof == "cold"}
			}
			c.cli.options.Executor = &nativeMaterializedState{files: map[string][]byte{}}
			if _, _, err := c.CaptureInventory(t.Context()); err != nil {
				t.Fatal(err)
			}
			targets, err := c.CaptureActivation(t.Context(), "materialized-config", false)
			if proof != "cold" {
				if !errors.Is(err, os.ErrNotExist) || probe.calls != 2 {
					t.Fatalf("unproved identity absence accepted: error=%v calls=%d", err, probe.calls)
				}
				return
			}
			// The existing config assessment reads once; identity capture must
			// reuse the independent positive proof without another query.
			if err != nil || len(targets) != 1 || probe.calls != 1 {
				t.Fatalf("cold materialized capture: targets=%d error=%v calls=%d", len(targets), err, probe.calls)
			}
			if target := targets[0]; target.Class != "materialized-files" || target.InstanceUUID != "absent" || target.Source == "" || target.PreparedRecipe != "" {
				t.Fatal("cold materialized capture changed its source or absence contract")
			}
		})
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

type nativeActivationInstanceProbe struct {
	*testkit.Incus
	calls int
	first error
	later error
}

func (p *nativeActivationInstanceProbe) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	p.calls++
	err := p.later
	if p.calls == 1 {
		err = p.first
	}
	if err != nil {
		return ports.InstanceInfo{}, err
	}
	return p.Incus.Instance(ctx, project, name)
}

type nativeActivationAbsenceProbe struct {
	*nativeActivationInstanceProbe
	absent bool
}

func (p nativeActivationAbsenceProbe) LocalInstallationAbsent() bool { return p.absent }

func TestNativeActivationUUIDUsesPositiveLocalAbsenceProof(t *testing.T) {
	for _, owner := range []string{"core-guest-runtime", "synthetic-runtime"} {
		for _, name := range []string{"cold", "false-proof", "no-proof", "missing-instance", "present-instance", "empty-uuid"} {
			t.Run(owner+"/"+name, func(t *testing.T) {
				c, _ := nativeActivationSourceFixture(t)
				profile := filepath.Join(c.context.RepositoryRoot, "config/profiles/synthetic")
				if err := os.MkdirAll(profile, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(profile, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":"runtime.sh"}}`), 0644)
				testkit.WriteFile(t, filepath.Join(profile, "runtime.sh"), []byte("#!/bin/sh\nexit 97\n"), 0755)
				catalog, err := config.LoadCatalog(c.context.RepositoryRoot)
				if err != nil {
					t.Fatal(err)
				}
				c.cli.catalog = catalog
				c.catalog = &c.cli.catalog
				probe := &nativeActivationInstanceProbe{Incus: c.cli.options.Incus.(*testkit.Incus), first: ports.ErrInstanceNotFound, later: os.ErrNotExist}
				switch name {
				case "cold", "false-proof":
					if name == "cold" {
						probe.first = os.ErrNotExist
					}
					c.cli.options.Incus = nativeActivationAbsenceProbe{probe, name == "cold"}
				case "missing-instance":
					probe.later = ports.ErrInstanceNotFound
					c.cli.options.Incus = probe
				case "present-instance", "empty-uuid":
					probe.first, probe.later = nil, nil
					if name == "empty-uuid" {
						instance := probe.Instances["subyard/yard"]
						instance.Config["volatile.uuid"] = ""
						probe.Instances["subyard/yard"] = instance
					}
					c.cli.options.Incus = probe
				default:
					c.cli.options.Incus = probe
				}
				if _, _, err := c.CaptureInventory(t.Context()); err != nil {
					t.Fatal(err)
				}
				values, err := c.CaptureActivation(t.Context(), owner, false)
				if name == "empty-uuid" {
					if err == nil || !strings.Contains(err.Error(), "UUID") || probe.calls != 2 {
						t.Fatal("empty native UUID was accepted", err, probe.calls)
					}
					return
				}
				if name == "false-proof" || name == "no-proof" {
					if !errors.Is(err, os.ErrNotExist) || probe.calls != 2 {
						t.Fatalf("unproved UUID absence accepted: error=%v calls=%d", err, probe.calls)
					}
					return
				}
				if err != nil || len(values) != 1 || values[0].Source == "" {
					t.Fatalf("runtime target capture: error=%v targets=%d calls=%d", err, len(values), probe.calls)
				}
				uuid, calls := "absent", 2
				if name == "cold" {
					calls = 0
				}
				if name == "present-instance" {
					uuid = "synthetic-instance-uuid"
				}
				if values[0].InstanceUUID != uuid || probe.calls != calls {
					t.Fatal("runtime capture changed UUID authority or repeated cold discovery")
				}
			})
		}
	}
}

func TestNativeInventoryRequiresYardRegistration(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	for _, relative := range []string{"yards/canonical/config.env", "yards/flat.env", ".sync/settings/yards/cached/config.env", "yards/retired/retained.dat"} {
		path := filepath.Join(home, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte("SSH_PORT=2290\n"), 0600)
	}
	names, _, err := c.CaptureInventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := c.CaptureYard(t.Context(), name, true); err != nil {
			t.Fatalf("discovered yard %s cannot resolve: %v", name, err)
		}
	}
	if strings.Join(names, ",") != "cached,canonical,default,flat" {
		t.Fatal("inventory does not match registered yards", names)
	}
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
		owner, err := c.CaptureOwner(ctx, nil)
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

func TestNativeActivationUsesCurrentEffectiveEnvironment(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_NATIVE_ENVIRONMENT") == "" {
		child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNativeActivationUsesCurrentEffectiveEnvironment$", "-test.v")
		child.Env = append(os.Environ(), "SUBYARD_TEST_NATIVE_ENVIRONMENT=1")
		child.Dir = testkit.TempDir(t)
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("native factory environment: %v\n%s", err, output)
		}
		return
	}
	fixture, _ := nativeActivationSourceFixture(t)
	root := fixture.context.RepositoryRoot
	fixture.context.PhysicalRepositoryRoot = root
	retired := filepath.Join(root, "config/retired-integrations/example")
	if err := os.MkdirAll(retired, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(retired, "integration.json"), []byte(`{"schema_version":1,"retired":true,"retired_settings":["EXAMPLE_RETIRED_PORT"],"lifecycle":{"handler":"retire.py"}}`), 0600)
	testkit.WriteFile(t, filepath.Join(retired, "retire.py"), []byte("#!/usr/bin/env python3\npass\n"), 0700)
	base := append(append([]string(nil), fixture.cli.options.Environment...), "EXTERNAL=original", "LC_ALL=C.UTF-8")
	old := append(append([]string(nil), base...), "AGENT_example_CHECK=old-check", "AGENT_example_COMMAND=old-command", "AGENT_example_HEALTH=old-health", "AGENT_example_PROVISION=/old/provision", "EXAMPLE_RETIRED_PORT=1234", "SSH_CODE_HOST=old-derived", "SUBYARD_CONFIG_SECRETS_DIR=/old/secrets")
	capture := func(environment []string, inherited ...string) (reconcileruntime.Runtime, string) {
		t.Helper()
		// Use the actual native factory: its ingress must have the same effective
		// environment whether called through a predecessor or a fresh launcher.
		for key, value := range environmentMap(environment) {
			t.Setenv(key, value)
		}
		for _, key := range []string{"AGENT_example_CHECK", "AGENT_example_COMMAND", "AGENT_example_HEALTH", "AGENT_example_PROVISION", "EXAMPLE_RETIRED_PORT", "SSH_CODE_HOST", "SUBYARD_CONFIG_SECRETS_DIR"} {
			if _, present := environmentMap(environment)[key]; !present {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
		}
		original := fixture.context
		original.InheritedSettingIDs = inherited
		for _, name := range inherited {
			value, present := environmentMap(environment)[name]
			var raw []byte
			if present {
				raw = []byte(value)
			}
			original.CommandOverrides = append(original.CommandOverrides, releaseowner.CommandOverride{Name: name, Present: present, Value: raw})
		}
		if len(inherited) != 0 {
			t.Setenv("SSH_PORT", "2255")
		}
		actual, err := newReleaseOwnerComponents(context.Background(), original, fixture.root)
		if err != nil {
			t.Fatal(err)
		}
		c := actual.(*releaseOwnerComponents)
		defer c.Close()
		loaded, err := c.load("default")
		if err != nil {
			t.Fatal(err)
		}
		runtime := c.platform(loaded, nil).(reconcileruntime.Runtime)
		token, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
		if err != nil {
			t.Fatal(err)
		}
		return runtime, token
	}
	before, original := capture(old)
	fresh, current := capture(base)
	if original != current || !bytes.Equal([]byte(strings.Join(before.Environment, "\x00")), []byte(strings.Join(fresh.Environment, "\x00"))) {
		t.Fatal("predecessor exports changed actual native execution environment or source")
	}
	if environmentMap(fresh.Environment)["EXTERNAL"] != "original" || environmentMap(fresh.Environment)["LC_ALL"] != "C.UTF-8" {
		t.Fatal("bootstrap inputs were lost")
	}
	_, changed := capture(append(base, "EXTERNAL=changed"))
	if changed == current {
		t.Fatal("actual execution environment drift did not change source")
	}
	override, changed := capture(append(base, "SSH_PORT=2241"), "SSH_PORT")
	if changed == current || environmentMap(override.Environment)["SSH_PORT"] != "2241" {
		t.Fatal("explicit current setting override was lost")
	}
	empty, _ := capture(append(base, "ENVIRONMENT_PROFILES="), "ENVIRONMENT_PROFILES")
	if value, present := environmentMap(empty.Environment)["ENVIRONMENT_PROFILES"]; !present || value != "" {
		t.Fatal("explicit empty command input was lost")
	}
	for _, invalid := range []releaseowner.Context{
		{InheritedSettingIDs: []string{"SSH_PORT"}},
		{InheritedSettingIDs: []string{"SSH_PORT"}, CommandOverrides: []releaseowner.CommandOverride{{Name: "SSH_PORT", Present: true}}},
		{InheritedSettingIDs: []string{"SSH_PORT"}, CommandOverrides: []releaseowner.CommandOverride{{Name: "SSH_PORT", Value: []byte{}}}},
		{InheritedSettingIDs: []string{"SSH_PORT", "SSH_PORT"}, CommandOverrides: []releaseowner.CommandOverride{{Name: "SSH_PORT"}, {Name: "SSH_PORT"}}},
		{InheritedSettingIDs: []string{"UNKNOWN_SETTING"}, CommandOverrides: []releaseowner.CommandOverride{{Name: "UNKNOWN_SETTING"}}},
		{InheritedSettingIDs: []string{"SUBYARD_CONFIG_HOME"}, CommandOverrides: []releaseowner.CommandOverride{{Name: "SUBYARD_CONFIG_HOME", Present: true, Value: []byte("/tmp/foreign")}}},
	} {
		original := fixture.context
		original.InheritedSettingIDs = invalid.InheritedSettingIDs
		original.CommandOverrides = invalid.CommandOverrides
		actual, err := newReleaseOwnerComponents(context.Background(), original, fixture.root)
		if err == nil {
			actual.Close()
			t.Fatal("invalid inherited command context accepted")
		}
	}
	absent := fixture.context
	absent.InheritedSettingIDs = []string{"SSH_PORT"}
	absent.CommandOverrides = []releaseowner.CommandOverride{{Name: "SSH_PORT"}}
	actual, err := newReleaseOwnerComponents(context.Background(), absent, fixture.root)
	if err != nil {
		t.Fatal("explicit absence refused", err)
	}
	if _, present := actual.(*releaseOwnerComponents).cli.env["SSH_PORT"]; present {
		t.Fatal("explicit absence adopted ambient command setting")
	}
	actual.Close()

	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("changed source\n"), 0600)
	_, changed = capture(base)
	if changed == current {
		t.Fatal("source modification did not change native binding")
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
