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

func TestResourceContextEnvironmentIsExplicitAndProtected(t *testing.T) {
	base := "HANDLER=missing.sh\nTITLE=Synthetic\nACTION=up up yard-change reversible\nACTION=down down yard-change reversible\n"
	for _, names := range []string{"FIXTURE_RUN FIXTURE_DATA", ""} {
		spec, err := ParseResource("one", "synthetic", strings.NewReader(base+"CONTEXT_ENV="+names+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		definition := spec.Definition()
		if len(definition.ContextEnvironment) != len(strings.Fields(names)) {
			t.Fatal("context declaration lost")
		}
		if len(definition.ContextEnvironment) != 0 {
			definition.ContextEnvironment[0] = "MUTATED"
			if spec.Definition().ContextEnvironment[0] != "FIXTURE_RUN" {
				t.Fatal("context declaration aliases registry")
			}
		}
	}
	for _, names := range []string{"FIXTURE FIXTURE", "bad-name", "1FIXTURE", "PATH", "HOME", "IFS", "BASH_ENV",
		"LD_PRELOAD", "PYTHONPATH", "SSH_AUTH_SOCK", "SUBYARD_RESOURCE_SESSION_TRANSPORT", "SUBYARD_CONFIG_HOME",
		"SUBYARD_ENGINE_CONTEXT", "INCUS_PROJECT", "YARD_INSTANCE_NAME", strings.Repeat("A", 65),
		"A B C D E F G H I J K L M N O P Q"} {
		if _, err := ParseResource("one", "synthetic", strings.NewReader(base+"CONTEXT_ENV="+names+"\n")); err == nil {
			t.Errorf("unsafe context declaration accepted: %q", names)
		}
	}
}
