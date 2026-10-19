package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releaseowner"
)

func TestAfterOwnerRouteOriginalScopeFitsFinalRecipeFrame(t *testing.T) {
	// Native route inventory comes from registration filenames, whose Unix
	// component limit is 255 bytes. The existing 256 device ceiling includes
	// the canonical owner; all three consumer labels are fixed derivations.
	type consumer struct {
		Project  string `json:"project"`
		Instance string `json:"instance"`
		Yard     string `json:"yard"`
		Mounted  bool   `json:"mounted"`
	}
	state := struct {
		SchemaVersion int        `json:"schemaVersion"`
		Active        bool       `json:"active"`
		Consumers     []consumer `json:"consumers"`
	}{SchemaVersion: 1, Active: true}
	for i := 0; i < 255; i++ {
		yard := fmt.Sprintf("%03d", i) + strings.Repeat("x", 252)
		state.Consumers = append(state.Consumers, consumer{"subyard-" + yard, "yard-" + yard, yard, false})
	}
	before, _ := json.Marshal(state)
	capacity, err := releaseowner.DecodeAfterOwnerCapacity(afterOwnerTemplate("routes", string(before)))
	if err != nil {
		t.Fatal(err)
	}
	if capacity.MetadataBytes > 1<<20 || capacity.DescriptorBytes != capacity.MetadataBytes {
		t.Fatal("invalid original scope capacity")
	}
	hash := strings.Repeat("a", 64)
	target := releaseowner.Target{Owner: "test-yard-route-consumers", Class: "route-consumers", Yard: "test-yard", Before: string(before), InstanceUUID: "original-created-uuid", PreparedRecipe: hash, AfterOwner: "v1:routes:" + hash, Source: hash, Actual: hash, Desired: hash}
	request := releaseowner.Request{Op: "bind-installed-recipe", Session: hash, RecipeHash: hash, Total: 512 << 10, Content: base64.StdEncoding.EncodeToString(make([]byte, 512<<10)), Target: &target}
	raw, _ := json.Marshal(request)
	if len(raw) > 1<<20 {
		t.Fatalf("supported route target exceeds final frame: %d", len(raw))
	}
}
