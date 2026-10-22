package reconcileruntime

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
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMaterializedProjectedSourceAcceptsOnlyApprovedPublication(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, source := materializedConfigFixture(t, root)
	future := []byte("approved prospective instructions\n")
	files := &config.ProspectiveFiles{Root: root, ReadPreparedInput: func(path string) (config.PersistentFileSnapshot, bool, error) {
		if path == source {
			return config.PersistentFileSnapshot{Exists: true, Content: future}, true, nil
		}
		return config.PersistentFileSnapshot{}, false, nil
	}, ReadSnapshot: func(path string) (config.PersistentFileSnapshot, error) {
		return config.ReadPersistentFileSnapshot(root, path)
	}}
	runtime.SourceReader = files.ReadAsset
	runtime.PreparedSources = files
	prepared, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	testkit.WriteFile(t, source, future, 0o600)
	runtime.SourceReader = nil
	rebound, err := runtime.BindInstalledRuntimeContext(context.Background(), p.Recipe())
	if err != nil {
		t.Fatal("approved publication lost original durable recipe", err)
	}
	defer rebound.Close()
	current, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := runtime.PrepareMaterializedConfigs(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	defer actual.Close()
	if actual.MaterializedSourceBinding() != p.MaterializedSourceBinding() {
		t.Fatal("own publication changed immutable source")
	}
	testkit.WriteFile(t, source, []byte("foreign publication\n"), 0o600)
	if _, err := runtime.BindInstalledRuntimeContext(context.Background(), p.Recipe()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("unapproved publication admitted", err)
	}
	if len(runtime.Incus.(*testkit.Incus).ExecCalls) != 0 {
		t.Fatal("publication guards made guest effects")
	}
}

type materializedGuardCut struct {
	*testkit.Incus
	source string
	cut    bool
}

func TestInstalledStageCapacityCountsParentsAndMarker(t *testing.T) {
	p := &PreparedInstalledRuntime{files: map[string][]byte{}}
	for index := 0; index < 8190; index++ {
		p.files[fmt.Sprintf("data/%04d", index)] = []byte("x")
	}
	members, _, err := p.stageUsage()
	if err != nil || members != 8192 {
		t.Fatalf("exact stage capacity: members=%d err=%v", members, err)
	}
	p.files["data/8190"] = []byte("x")
	if _, _, err := p.stageUsage(); err == nil {
		t.Fatal("8191 files plus directory and marker admitted")
	}
	if p.directory != "" {
		t.Fatal("readonly stage capacity check created a tree")
	}
}

func (e *materializedGuardCut) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if !e.cut {
		e.cut = true
		if err := os.WriteFile(e.source, []byte("unapproved after guard\n"), 0o600); err != nil {
			return ports.InstanceExecResult{}, err
		}
	}
	return e.Incus.Exec(ctx, project, name, request)
}

func materializedConfigFixture(t *testing.T, root string) (Runtime, string) {
	t.Helper()
	source := filepath.Join(root, "approved.instructions")
	incus := runningIncus(3)
	incus.Reconcile.Instance.Config = map[string]string{"volatile.uuid": "original-instance"}
	runtime := Runtime{RepositoryRoot: root, Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + source}, Incus: incus, Executor: incus,
		Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001}}
	bindInstructionTestPackage(t, &runtime, root, "codex")
	return runtime, source
}

func TestMaterializedConfigsFreshProcessOriginalRecipe(t *testing.T) {
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_MATERIALIZED_RECIPE_CHILD"); root != "" {
		runtime, source := materializedConfigFixture(t, root)
		raw, err := os.ReadFile(filepath.Join(root, "original.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := runtime.BindInstalledRuntimeContext(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		incus := runtime.Incus.(*testkit.Incus)
		runtime.Executor = &materializedGuardCut{Incus: incus, source: source}
		if err := runtime.ApplyPreparedMaterializedConfigs(ctx, p); err != nil {
			t.Fatal(err)
		}
		if len(incus.ExecCalls) != 3 || !bytes.Contains(incus.ExecCalls[1].Request.Stdin, []byte("approved original\n")) || bytes.Contains(incus.ExecCalls[1].Request.Stdin, []byte("unapproved after guard")) {
			t.Fatal("fresh process did not execute the original retained config payload")
		}
		return
	}
	root := testkit.TempDir(t)
	runtime, source := materializedConfigFixture(t, root)
	testkit.WriteFile(t, source, []byte("approved original\n"), 0o600)
	prepared, err := runtime.PrepareConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := runtime.PrepareMaterializedConfigs(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	testkit.WriteFile(t, filepath.Join(root, "original.recipe"), p.Recipe(), 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestMaterializedConfigsFreshProcessOriginalRecipe$", "-test.v")
	command.Env = append(os.Environ(), "SUBYARD_MATERIALIZED_RECIPE_CHILD="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, output)
	}
}

func TestMaterializedConfigsRecipeRefusesDriftBeforeGuestEffect(t *testing.T) {
	for _, change := range []string{"bytes", "identity", "uuid", "destination", "payload", "scope"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime, source := materializedConfigFixture(t, root)
			testkit.WriteFile(t, source, []byte("approved original\n"), 0o600)
			prepared, err := runtime.PrepareConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			raw := p.Recipe()
			switch change {
			case "bytes":
				testkit.WriteFile(t, source, []byte("unapproved before dispatch\n"), 0o600)
			case "identity":
				testkit.WriteFile(t, source+".new", []byte("approved original\n"), 0o600)
				if err := os.Rename(source+".new", source); err != nil {
					t.Fatal(err)
				}
			case "uuid":
				runtime.Incus.(*testkit.Incus).Reconcile.Instance.Config["volatile.uuid"] = "replacement-instance"
			case "destination":
				runtime.Environment = append(runtime.Environment, "DEV_USER=other")
				runtime.Yard.DevUser = "other"
			case "payload", "scope":
				var r installedRecipe
				if err := json.Unmarshal(raw, &r); err != nil {
					t.Fatal(err)
				}
				if change == "payload" {
					r.Files[0].Content = []byte("unapproved payload")
				} else {
					r.Owner = "core-guest-runtime"
				}
				raw, _ = json.Marshal(r)
			}
			if rebound, err := runtime.BindInstalledRuntimeContext(context.Background(), raw); err == nil {
				rebound.Close()
				t.Fatal("drifted recipe rebound")
			}
			if len(runtime.Incus.(*testkit.Incus).ExecCalls) != 0 {
				t.Fatal("recipe refusal made a guest effect")
			}
			if change == "bytes" || change == "identity" || change == "uuid" {
				if err := runtime.ApplyPreparedMaterializedConfigs(context.Background(), p); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("drift executed: %v", err)
				}
			}
		})
	}
}

func TestMaterializedConfigsRecipePreservesNonUTF8Source(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, _ := materializedConfigFixture(t, root)
	source := filepath.Join(root, "source-"+string([]byte{0xff}))
	testkit.WriteFile(t, source, []byte("approved original\n"), 0o600)
	runtime.Environment[1] = "HOST_CODEX_AGENTS_MD=" + source
	prepared, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rebound, err := runtime.BindInstalledRuntimeContext(context.Background(), p.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer rebound.Close()
	if rebound.configs.files[0].source != source {
		t.Fatal("non-UTF8 source path was lossy")
	}
}

func TestMaterializedConfigsRecipePreservesPayloadAllowanceWithMetadata(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, source := materializedConfigFixture(t, root)
	payload := bytes.Repeat([]byte("x"), 32<<20)
	testkit.WriteFile(t, source, payload, 0o600)
	// The prior selector admits a long relative destination. This is a prepare
	// capacity check, not a claim that such a path is usable on a real guest.
	destination := strings.Repeat("long-metadata-", 160000) + "rules"
	runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_RULES=" + source, "AGENT_sample_RULES_DEST=" + destination}
	prepared, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var rawBytes, metadataBytes int
	for path, body := range p.files {
		rawBytes += len(body)
		if strings.HasPrefix(path, materializedMetadataPrefix) {
			metadataBytes += len(body)
		}
	}
	raw := p.Recipe()
	if rawBytes <= 32<<20 || rawBytes > 64<<20 || len(raw) > MaxInstalledRecipe {
		t.Fatal("payload/metadata capacity did not preserve the original allowance")
	}
	t.Logf("payload=%d metadata=%d raw=%d canonical=%d files=%d", len(payload), metadataBytes, rawBytes, len(raw), len(p.files))
	rebound, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer rebound.Close()
	if !bytes.Equal(rebound.configs.payloads["/home/dev/"+destination], payload) {
		t.Fatal("boundary payload changed on bind")
	}
	// Every retained file is charged before publication; an extra leaf cannot
	// silently escape the generic per-recipe raw capacity.
	p.files[materializedBlobPath(bytes.Repeat([]byte("y"), 32<<20))] = bytes.Repeat([]byte("y"), 32<<20)
	if err := checkMaterializedRecipeSize(p); err == nil {
		t.Fatal("raw capacity overflow admitted")
	}
	if len(runtime.Incus.(*testkit.Incus).ExecCalls) != 0 {
		t.Fatal("capacity admission made guest effects")
	}
}
