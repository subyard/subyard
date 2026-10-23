package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func preparedProfileFixture(t *testing.T) Runtime {
	t.Helper()
	runtime := preparedInstalledFixture(t)
	root := filepath.Join(runtime.RepositoryRoot, "config/profiles/synthetic")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"runtime.sh": "#!/bin/sh\nexit 0\n", "payload.txt": "approved profile payload\n", "payload<&\u2028.txt": "approved serialized path\n"} {
		testkit.WriteFile(t, filepath.Join(root, name), []byte(body), 0o755)
	}
	runtime.Profiles = []profile.Definition{{SchemaVersion: 1, Name: "synthetic", Root: root, Runtime: &profile.RuntimeHook{ActivationID: "synthetic-runtime", Handler: "runtime.sh"}, Native: []profile.Native{{Package: "cmd/worker", Path: "bin/worker"}}}}
	fallback := filepath.Join(runtime.RepositoryRoot, ".build/profiles/synthetic/bin/worker")
	if err := os.MkdirAll(filepath.Dir(fallback), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fallback, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/i": {Status: "Running", Config: map[string]string{"volatile.uuid": "fixture-uuid"}}}}
	runtime.Incus = incus
	runtime.Executor = incus
	runtime.Yard.IncusProject = "p"
	runtime.Yard.YardInstanceName = "i"
	return runtime
}

func TestPreparedProfileRecipeRetainsNativeLayoutAndRefusesChangedScope(t *testing.T) {
	runtime := preparedProfileFixture(t)
	p, err := runtime.PrepareProfileRuntimeSources(context.Background(), "synthetic-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	restored, err := runtime.BindInstalledRuntime(p.Recipe())
	if err != nil || restored.Binding() != p.Binding() {
		t.Fatal("original profile recipe failed to restore", err)
	}
	defer restored.Close()
	native := filepath.Join(runtime.RepositoryRoot, ".build/profiles/synthetic/bin/worker")
	testkit.WriteFile(t, native, []byte("#!/bin/sh\nexit 3\n"), 0o755)
	if _, err := runtime.BindInstalledRuntime(p.Recipe()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed fallback native source rebound", err)
	}
	if err := p.materialize(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(p.directory, ".build/profiles/synthetic/bin/worker"))
	if err != nil || string(body) != "#!/bin/sh\nexit 0\n" {
		t.Fatal("selected native layout recaptured", err)
	}
}

func TestPreparedProfileExecutionConsumesApprovedPayloadAfterGuard(t *testing.T) {
	runtime := preparedProfileFixture(t)
	home := runtime.Yard.Paths.DataHome
	root := runtime.Profiles[0].Root
	marker, swap := filepath.Join(home, "effect"), filepath.Join(home, "swap")
	old, new := testRuntimeOldDigest, testRuntimeNewDigest
	script := `#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
if [ "$1" = observe ]; then
 if [ -f "$SWAP" ]; then cp "$SWAP" "$ORIGINAL"; rm "$SWAP"; fi
elif [ "$1" = apply ]; then cat "$root/payload.txt" >"$EFFECT"; fi
if [ -f "$EFFECT" ]; then printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"; else printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW"; fi
`
	testkit.WriteFile(t, filepath.Join(root, "runtime.sh"), []byte(script), 0o755)
	runtime.Environment = append(runtime.Environment, "EFFECT="+marker, "SWAP="+swap, "ORIGINAL="+filepath.Join(root, "payload.txt"), "OLD="+old, "NEW="+new)
	p, err := runtime.PrepareProfileRuntimeSources(context.Background(), "synthetic-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	testkit.WriteFile(t, swap, []byte("unapproved source after guard\n"), 0o600)
	if err := runtime.ApplyPreparedProfileRuntimeSources(context.Background(), p, "prepared", old, new); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(marker)
	if err != nil || string(body) != "approved profile payload\n" {
		t.Fatal("profile payload recaptured", err)
	}
}

func TestPreparedRuntimeRecipeCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_INSTALLED_RECIPE_CORPUS")
	if destination == "" {
		t.Skip("optional owning canonical corpus")
	}
	type sample struct {
		Name string `json:"name"`
		Raw  []byte `json:"raw"`
		SHA  string `json:"sha256"`
	}
	samples := []sample{}
	for _, owner := range []string{"core-guest-runtime", "key-tools-runtime", "synthetic-runtime"} {
		runtime := preparedProfileFixture(t)
		source, err := filepath.Abs("../../..")
		if err != nil {
			t.Fatal(err)
		}
		for _, relative := range []string{"scripts/refresh-installed-guest-runtime.sh", "scripts/prepare-preview-route.sh", "config/preview/subyard-preview", "config/agents/ccusage/provision.sh"} {
			body, err := os.ReadFile(filepath.Join(source, relative))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(runtime.RepositoryRoot, relative)), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, relative), body, 0o755)
		}
		runtime.Environment = append(runtime.Environment, "ALLOWS_CODING_TOOLS=false")
		var p *PreparedInstalledRuntime
		if owner == "synthetic-runtime" {
			p, err = runtime.PrepareProfileRuntimeSources(context.Background(), owner)
		} else {
			p, err = runtime.PrepareInstalledRuntime(context.Background(), owner)
		}
		if err != nil {
			t.Fatal(err)
		}
		raw := p.Recipe()
		samples = append(samples, sample{owner, raw, p.Binding()})
		p.Close()
	}
	{
		root := testkit.TempDir(t)
		runtime, source := materializedConfigFixture(t, root)
		testkit.WriteFile(t, source, []byte("approved corpus config\n"), 0o600)
		prepared, err := runtime.PrepareConfigs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		p, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
		if err != nil {
			t.Fatal(err)
		}
		samples = append(samples, sample{"materialized-files", p.Recipe(), p.Binding()})
		p.Close()
	}
	{
		runtime, _ := materializedIntegrationFixture(t, testkit.TempDir(t), true)
		p := prepareMaterializedIntegrationFixture(t, runtime)
		samples = append(samples, sample{"materialized-integrations", p.Recipe(), p.Binding()})
		p.Close()
	}
	raw, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedProfileCapturesSharedSkillHelperForPreCoreGuestActivation(t *testing.T) {
	runtime := preparedProfileFixture(t)
	path := filepath.Join(runtime.RepositoryRoot, "config/skills/deliver.py")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("# captured shared delivery helper\n")
	testkit.WriteFile(t, path, original, 0o644)
	prepared, err := runtime.PrepareProfileRuntimeSources(context.Background(), "synthetic-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if string(prepared.files["config/skills/deliver.py"]) != string(original) {
		t.Fatal("profile recipe omitted pre-core skill delivery source")
	}
	testkit.WriteFile(t, path, []byte("# changed helper\n"), 0o644)
	if _, err := runtime.BindInstalledRuntime(prepared.Recipe()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed helper admitted: %v", err)
	}
	if err := prepared.materialize(); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(prepared.directory, "config/skills/deliver.py"))
	if err != nil || string(actual) != string(original) {
		t.Fatalf("helper recaptured after guard: %v", err)
	}
}
