package resource

import (
	"strings"
	"testing"
)

func TestBuildRegistryChecksCollisionsWithoutHandlers(t *testing.T) {
	descriptor := "HANDLER=missing.sh\nTITLE=Synthetic\nACTION=up up yard-change reversible\nACTION=down down yard-change reversible\n"
	one, err := ParseResource("one", "synthetic", strings.NewReader(descriptor))
	if err != nil {
		t.Fatal(err)
	}
	two, err := ParseResource("two", "other", strings.NewReader("COMMAND=synthetic\n"+descriptor))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := BuildRegistry([]ResourceSpec{one})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.Lookup("synthetic")
	if !ok || definition.HandlerPath() != "" {
		t.Fatal("semantic registry created handler binding")
	}
	definition.Verbs[0] = "changed"
	if registry.Definitions()[0].Verbs[0] != "up" {
		t.Fatal("registry aliases consumer metadata")
	}
	if _, err := BuildRegistry([]ResourceSpec{one, two}); err == nil {
		t.Fatal("command collision accepted")
	}
	if _, err := BuildRegistry([]ResourceSpec{{}}); err == nil {
		t.Fatal("zero resource accepted")
	}
	if _, err := ParseResource("one", "synthetic", strings.NewReader(strings.ReplaceAll(descriptor, "missing.sh", "../escape"))); err == nil {
		t.Fatal("escaping handler accepted by parser")
	}
}
