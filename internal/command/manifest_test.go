package command

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRepositoryManifest(t *testing.T) {
	root := repositoryRoot(t)
	file, err := os.Open(filepath.Join(root, "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	manifest, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.PublicNames()) < 20 {
		t.Fatalf("unexpected public command count: %d", len(manifest.PublicNames()))
	}
	setup, ok := manifest.Lookup("setup")
	if !ok || setup.Name != "init" || setup.Effect != EffectMutate || setup.Confirmation != ConfirmationDynamic {
		t.Fatalf("setup alias mismatch: %#v", setup)
	}
	list, ok := manifest.Lookup("list")
	if !ok || list.Effect != EffectRead || list.Confirmation != ConfirmationNever || list.Remote != RemoteLocal {
		t.Fatalf("list contract mismatch: %#v", list)
	}
	for _, definition := range manifest.Commands() {
		if definition.Confirmation == Confirmation("required") {
			t.Errorf("command %q uses legacy required confirmation", definition.Name)
		}
	}
}

func TestManifestRejectsDuplicatesAndTraversal(t *testing.T) {
	rows := []string{
		"one|same|one.sh||local|read|never|public|x|simple|one|first|--help|",
		"two|same|two.sh||local|read|never|public|x|simple|two|second|--help|",
	}
	if _, err := Parse(strings.NewReader(strings.Join(rows, "\n"))); err == nil {
		t.Fatal("duplicate alias was accepted")
	}
	if _, err := Parse(strings.NewReader("one||../escape.sh||local|read|never|public|x|simple|one|first|||\n")); err == nil {
		t.Fatal("handler traversal was accepted")
	}
	if _, err := Parse(strings.NewReader("one||one.sh||local|read|sometimes|public|x|simple|one|first||\n")); err == nil {
		t.Fatal("unknown confirmation policy was accepted")
	}
	if _, err := Parse(strings.NewReader("one||one.sh||local|mutate|required|hidden|x|simple|one|first||\n")); err == nil {
		t.Fatal("legacy required confirmation policy was accepted")
	}
}

func FuzzParseDoesNotPanic(fuzz *testing.F) {
	fuzz.Add("one||handler.sh||local|read|never|public|section|simple|one|summary||\n")
	fuzz.Add("malformed")
	fuzz.Fuzz(func(t *testing.T, value string) {
		_, _ = Parse(strings.NewReader(value))
	})
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
