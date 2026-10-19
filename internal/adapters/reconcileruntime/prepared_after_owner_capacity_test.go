package reconcileruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestAfterOwnerHeaderPreservesOriginalFileAndContentCapacity(t *testing.T) {
	for _, mode := range []string{"8192-files", "64MiB-content"} {
		t.Run(mode, func(t *testing.T) {
			p := &PreparedInstalledRuntime{owner: "materialized-integrations", files: map[string][]byte{}}
			if mode == "8192-files" {
				for i := 0; i < 8192; i++ {
					p.files[fmt.Sprintf("prepared/materialized-bytes/%064x", i)] = []byte{byte(i)}
				}
			} else {
				p.files["prepared/materialized-bytes/a"] = bytes.Repeat([]byte("a"), 32<<20)
				p.files["prepared/materialized-bytes/b"] = bytes.Repeat([]byte("b"), 32<<20)
			}
			if err := checkMaterializedRecipeSize(p); err != nil {
				t.Fatal("original capacity fixture", err)
			}
			if bytes.Contains(p.Recipe(), []byte(`"afterOwner":`)) {
				t.Fatal("ordinary omitted header changed")
			}
			originalCount := len(p.files)
			conditional, err := p.reserveAfterOwnerMetadata([]byte(`{"version":1,"phase":"template","metadataBytes":1048576,"descriptorBytes":1048576}`))
			if err != nil {
				t.Fatal("conditional lowered existing capacity", err)
			}
			if len(conditional.files) != originalCount {
				t.Fatal("metadata consumed a content slot")
			}
			resolved, err := conditional.WithAfterOwnerMetadata(make([]byte, 1<<20))
			if err != nil {
				t.Fatal("reserved physical header cannot fit", err)
			}
			var recipe installedRecipe
			if json.Unmarshal(resolved.Recipe(), &recipe) != nil || len(recipe.AfterOwner) != 1<<20 || len(recipe.Files) != originalCount {
				t.Fatal("header/file capacity changed")
			}
		})
	}
}
