package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"testing"
)

// PrepareAfterOwnerMaterializedConfigs captures only the owning file selector
// and source bytes. The future instance is deliberately not a physical guard.
func (runtime Runtime) PrepareAfterOwnerMaterializedConfigs(ctx context.Context, template []byte) (*PreparedInstalledRuntime, error) {
	p, err := runtime.PrepareAfterOwnerMaterializedConfigSources(ctx)
	if err != nil {
		return nil, err
	}
	return p.reserveAfterOwnerMetadata(template)
}

func TestAfterOwnerMaterializedFilesOriginalSourceGuards(t *testing.T) {
	for _, change := range []string{"uuid", "source", "selector", "ordinary-kind"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime, _ := afterOwnerFilesFixture(t, root)
			if err := os.Mkdir(filepath.Join(root, "home"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "sample.CONFIG"), []byte("approved A\n"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "sample.RULES"), []byte("approved B\n"), 0600)
			ctx := context.Background()
			original, err := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":8192,"descriptorBytes":8192}`))
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			resolved, physical, err := runtime.ResolveAfterOwnerMaterializedConfigs(ctx, original.Recipe())
			if err != nil {
				t.Fatal(err)
			}
			defer resolved.Close()
			raw := original.Recipe()
			switch change {
			case "uuid":
				runtime.Incus.(*testkit.Incus).Reconcile.Instance.Config["volatile.uuid"] = "foreign-instance"
			case "source":
				testkit.WriteFile(t, filepath.Join(root, "sample.CONFIG"), []byte("unapproved source\n"), 0600)
			case "selector":
				runtime.Environment = append(runtime.Environment, "AGENT_sample_CONFIG_DEST=.sample/other.conf")
			case "ordinary-kind":
				var other installedRecipe
				if json.Unmarshal(raw, &other) != nil {
					t.Fatal("recipe")
				}
				other.Owner = "materialized-integrations"
				raw, _ = json.Marshal(other)
			}
			if _, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, raw, physical); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("drift replaced original physical plan", err)
			}
			if _, err := os.Stat(filepath.Join(root, "writes")); !os.IsNotExist(err) {
				t.Fatal("refusal wrote guest files")
			}
		})
	}
}
