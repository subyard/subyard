package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDeclaredArtifactsCaptureProspectiveSourcesAndRejectStalePackages(t *testing.T) {
	root := testkit.TempDir(t)
	packageRoot := filepath.Join(root, "config/agents/fixture-tool")
	if err := os.MkdirAll(packageRoot, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schema_version":2,"settings":[{"name":"FIXTURE_INSTRUCTIONS","type":"absolute-path","optional":true,"scopes":["host","yard","command"],"application":"yard-init"}],"artifacts":[{"id":"config","kind":"config","source":"config.jsonc","destination":".fixture/config.jsonc","format":"raw"},{"id":"instructions","kind":"instructions","source_setting":"FIXTURE_INSTRUCTIONS","destination":".fixture/AGENTS.md","format":"raw"}]}`)
	declaration := filepath.Join(packageRoot, "integration.json")
	raw := []byte("// JSONC comments survive\n{\"permission\":\"allow\",}\n")
	source := filepath.Join(packageRoot, "config.jsonc")
	testkit.WriteFile(t, declaration, manifest, 0644)
	testkit.WriteFile(t, source, raw, 0644)
	instructions := filepath.Join(testkit.TempDir(t), "AGENTS.md")
	imported := []byte("approved prospective instructions\n")
	definitions, err := profile.LoadIntegrations(root)
	if err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(0)
	runtime := Runtime{RepositoryRoot: root, Integrations: definitions, Incus: incus, Executor: incus, Yard: domain.Context{DevUser: "dev", DevUID: 1001, IncusProject: "fixture", YardInstanceName: "yard"}, Environment: []string{"CODING_TOOL_INTEGRATIONS=fixture-tool", "FIXTURE_INSTRUCTIONS=" + instructions}, SourceReader: func(asset config.MaterializedAsset) ([]byte, error) {
		if asset.Source == instructions {
			return imported, nil
		}
		return asset.ReadSource()
	}}
	unbound := runtime
	unbound.Integrations = []profile.IntegrationDefinition{definitions[0].Clone()}
	unbound.Integrations[0].Root = ""
	if _, err := unbound.PrepareConfigs(context.Background()); err == nil {
		t.Fatal("unbound semantic artifact declaration authorized preparation")
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("unbound artifact reached guest")
	}
	overlapping := runtime
	overlapping.Environment = append(append([]string{}, runtime.Environment...), "AGENT_fixture-tool_CONFIG_DEST=.fixture")
	if err := overlapping.RefreshConfigs(context.Background()); err == nil {
		t.Fatal("overlapping files authorized refresh")
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("overlapping files reached guest")
	}
	overlapping.Environment = append(append([]string{}, runtime.Environment...), "INTEGRATION_HOST_LINKS=.fixture/config.jsonc:/mnt/host/sessions")
	if err := overlapping.RefreshConfigs(context.Background()); err == nil {
		t.Fatal("managed link overlap authorized refresh")
	}
	if _, err := overlapping.PrepareIntegrations(context.Background(), IntegrationPlan{}); err == nil {
		t.Fatal("managed link overlap authorized integration planning")
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("managed link overlap reached guest")
	}

	prepared, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prepared.payloads["/home/dev/.fixture/config.jsonc"], raw) || !bytes.HasPrefix(prepared.payloads["/home/dev/.fixture/AGENTS.md"], imported) {
		t.Fatal("raw config or prospective instructions were replaced")
	}
	// The ordinary reader cannot see instructions yet; planning must use capture.
	if _, err := os.Stat(instructions); !os.IsNotExist(err) {
		t.Fatal("prospective source unexpectedly exists")
	}
	testkit.WriteFile(t, instructions, imported, 0600)
	if err := runtime.CheckPreparedConfigs(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	recipe, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.restoreMaterializedConfigs(context.Background(), recipe); err != nil {
		t.Fatal("captured declaration recipe did not restore", err)
	}
	for _, changed := range []string{"manifest", "default", "removed", "replacement"} {
		t.Run(changed, func(t *testing.T) {
			prepared, err := runtime.PrepareConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			recipe, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
			if err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "manifest":
				testkit.WriteFile(t, declaration, append(bytes.Clone(manifest), '\n'), 0644)
			case "default":
				testkit.WriteFile(t, source, []byte("changed"), 0644)
			case "replacement":
				testkit.WriteFile(t, declaration+".replacement", manifest, 0644)
				if err := os.Rename(declaration+".replacement", declaration); err != nil {
					t.Fatal(err)
				}
			case "removed":
				if err := os.Remove(declaration); err != nil {
					t.Fatal(err)
				}
			}
			before := len(incus.ExecCalls)
			if err := runtime.ApplyPreparedConfigs(context.Background(), prepared); err == nil {
				t.Fatal("changed package allowed prepared config mutation")
			}
			if len(incus.ExecCalls) != before {
				t.Fatal("stale package reached guest")
			}
			if _, err := runtime.restoreMaterializedConfigs(context.Background(), recipe); err == nil {
				t.Fatal("stale recipe restored")
			}
			testkit.WriteFile(t, declaration, manifest, 0644)
			testkit.WriteFile(t, source, raw, 0644)
		})
	}
	runtime.SourceReader = nil
	testkit.WriteFile(t, instructions, []byte("changed after approval"), 0600)
	if err := runtime.CheckPreparedConfigs(context.Background(), prepared); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("operator source drift accepted", err)
	}
}
