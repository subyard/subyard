package reconcileruntime

import (
	"bytes"
	"context"
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
	manifest := []byte(`{"schema_version":2,"artifacts":[{"id":"config","kind":"config","source":"config.jsonc","destination":".fixture/config.jsonc","format":"raw"},{"id":"rules","kind":"config","source":"rules","destination":".fixture/repo.rules","format":"raw"}]}`)
	declaration := filepath.Join(packageRoot, "integration.json")
	raw := []byte("// JSONC comments survive\n{\"permission\":\"allow\",}\n")
	source := filepath.Join(packageRoot, "config.jsonc")
	testkit.WriteFile(t, declaration, manifest, 0644)
	testkit.WriteFile(t, source, raw, 0644)
	testkit.WriteFile(t, filepath.Join(packageRoot, "rules"), []byte("shipped rules\n"), 0644)
	rules := filepath.Join(testkit.TempDir(t), "repo.rules")
	imported := []byte("approved prospective rules\n")
	definitions, err := profile.LoadIntegrations(root)
	if err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(0)
	runtime := Runtime{RepositoryRoot: root, Integrations: definitions, Incus: incus, Executor: incus, Yard: domain.Context{DevUser: "dev", DevUID: 1001, IncusProject: "fixture", YardInstanceName: "yard"}, Environment: []string{"CODING_TOOL_INTEGRATIONS=fixture-tool", "AGENT_fixture-tool_RULES=" + rules}, SourceReader: func(asset config.MaterializedAsset) ([]byte, error) {
		if asset.Source == rules {
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
	if err := overlapping.RefreshConfigs(context.Background(), nil); err == nil {
		t.Fatal("overlapping files authorized refresh")
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("overlapping files reached guest")
	}
	overlapping.Environment = append(append([]string{}, runtime.Environment...), "INTEGRATION_HOST_LINKS=.fixture/config.jsonc:/mnt/host/sessions")
	if err := overlapping.RefreshConfigs(context.Background(), nil); err == nil {
		t.Fatal("managed link overlap authorized refresh")
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("managed link overlap reached guest")
	}

	prepared, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prepared.payloads["/home/dev/.fixture/config.jsonc"], raw) || !bytes.Equal(prepared.payloads["/home/dev/.fixture/repo.rules"], imported) {
		t.Fatal("raw config or prospective rules were replaced")
	}
	// The ordinary reader cannot see the rules yet; planning must use capture.
	if _, err := os.Stat(rules); !os.IsNotExist(err) {
		t.Fatal("prospective source unexpectedly exists")
	}
}
