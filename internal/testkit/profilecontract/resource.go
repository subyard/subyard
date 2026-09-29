package profilecontract

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

type ResourceActionExpectation struct {
	Resource string
	LocalID  string
	Verb     string
	Effect   domain.ActionEffect
	Impacts  []domain.ActionImpact
	Recovery domain.RecoveryClass
}

// CheckResourceProfileContract checks one profile's declared action surface and
// the generic handler contract applied to its shipped resources.
func CheckResourceProfileContract(
	t testing.TB,
	root, profile string,
	expected []ResourceActionExpectation,
	wantVerbs map[string][]string,
	legacyDescriptors []string,
	forbiddenHandlerText map[string][]string,
	undeclared []ResourceActionExpectation,
	aliases []ResourceActionExpectation,
) {
	t.Helper()
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	owned := make(map[string]resource.Definition)
	for _, definition := range registry.Definitions() {
		if definition.Profile != profile {
			continue
		}
		owned[definition.Name] = definition
		if _, ok := registry.Lookup(definition.Command); !ok {
			t.Fatalf("resource command is not indexed: %s", definition.Command)
		}
		if byName, ok := registry.Lookup(definition.Name); !ok || byName.HandlerPath() != definition.HandlerPath() {
			t.Fatalf("resource name and command differ: %s", definition.Name)
		}
		content, err := os.ReadFile(definition.HandlerPath())
		if err != nil {
			t.Fatal(err)
		}
		source := string(content)
		if !strings.Contains(source, "subyard_require_engine_context") ||
			strings.Contains(source, "subyard_context_load") || strings.Contains(source, "lib/config.sh") {
			t.Errorf("resource handler does not consume only prepared context: %s", definition.HandlerPath())
		}
	}
	if len(owned) == 0 {
		t.Fatalf("profile %q has no shipped resources", profile)
	}
	definitions := make(map[domain.ActionID]domain.ActionDefinition)
	for _, definition := range registry.ActionDefinitions() {
		id := string(definition.Action)
		if strings.HasPrefix(id, "resource."+profile+".") {
			if _, duplicate := definitions[definition.Action]; duplicate {
				t.Fatalf("duplicate shipped action definition %q", definition.Action)
			}
			definitions[definition.Action] = definition
		}
	}
	if len(definitions) != len(expected) {
		t.Fatalf("%s action definitions = %d, want %d", profile, len(definitions), len(expected))
	}
	for _, action := range expected {
		if _, ok := owned[action.Resource]; !ok {
			t.Errorf("expected resource %q is not owned by %s", action.Resource, profile)
			continue
		}
		qualified, ok := registry.LookupAction(action.Resource, action.Verb, action.LocalID)
		if !ok {
			t.Errorf("missing %s action %s for verb %s", action.Resource, action.LocalID, action.Verb)
			continue
		}
		definition, ok := definitions[qualified]
		if !ok {
			t.Errorf("lookup %q has no %s domain definition", qualified, profile)
			continue
		}
		if definition.Effect != action.Effect || definition.Recovery != action.Recovery ||
			!reflect.DeepEqual(definition.Impacts, action.Impacts) {
			t.Errorf("%q classification = effect %q impacts %#v recovery %q", qualified, definition.Effect, definition.Impacts, definition.Recovery)
		}
	}
	for name, verbs := range wantVerbs {
		definition, ok := owned[name]
		if !ok || !slices.Equal(definition.Verbs, verbs) {
			t.Errorf("%s verbs = %#v, want %#v", name, definition.Verbs, verbs)
		}
	}
	for _, action := range undeclared {
		if _, ok := registry.LookupAction(action.Resource, action.Verb, action.LocalID); ok {
			t.Errorf("undeclared %s action %s for verb %s remains reachable", action.Resource, action.LocalID, action.Verb)
		}
	}
	for _, action := range aliases {
		definition, ok := registry.LookupAction(action.Resource, action.Verb, action.LocalID)
		if !ok {
			t.Errorf("%s action %s for verb %s is not reachable through its command alias", action.Resource, action.LocalID, action.Verb)
			continue
		}
		if !strings.HasPrefix(string(definition), "resource."+profile+".") {
			t.Errorf("alias %q resolved outside profile %s: %q", action.Resource, profile, definition)
		}
	}
	for _, descriptor := range legacyDescriptors {
		content, err := os.ReadFile(filepath.Join(root, descriptor))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "VERBS=") {
			t.Errorf("legacy VERBS remains in %s", descriptor)
		}
	}
	for relativePath, forbidden := range forbiddenHandlerText {
		content, err := os.ReadFile(filepath.Join(root, relativePath))
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range forbidden {
			if strings.Contains(string(content), text) {
				t.Errorf("undeclared handler alias %q remains reachable in %s", text, relativePath)
			}
		}
	}
}
