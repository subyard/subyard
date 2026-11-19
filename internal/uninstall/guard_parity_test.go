package uninstall

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// While the released helper remains in the tree, Go and Python capture the
// same facts, digests and normalized tree records byte for byte.
func TestGoGuardMatchesReleasedPythonHelper(t *testing.T) {
	helper, err := filepath.Abs("../../scripts/lib/teardown-plan.py")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(testkit.TempDir(t), "owned")
	if err := os.MkdirAll(filepath.Join(root, "nested <&>  "), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plain", "tab\tand\x01control", "back\bform\fquote\"slash\\", "nested <&>  /é \U0001f600", "retained/data"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, name), []byte(name), 0600)
	}
	if err := os.Symlink("../target <>& \x7f", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	python := func(mode string, input any) []byte {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("python3", "-B", helper, mode)
		command.Stdin = bytes.NewReader(body)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		return output
	}
	var released ports.TeardownArtifact
	if err := json.Unmarshal(python("capture-artifact", ports.TeardownArtifact{Path: root}), &released); err != nil {
		t.Fatal(err)
	}
	current, err := CaptureArtifact(root)
	if err != nil {
		t.Fatal(err)
	}
	if released.Binding == "" || !reflect.DeepEqual(released, current) {
		t.Fatalf("artifact capture differs:\nreleased=%+v\ncurrent=%+v", released, current)
	}
	for _, tree := range []Tree{
		{Path: root, Entries: []Entry{}},
		{Path: root, Entries: []Entry{}, Exclusions: []string{filepath.Join(root, "retained")}},
		{Path: root, Entries: []Entry{}, Scoped: true, OwnedPaths: []string{"nested <&>  ", "plain"}},
	} {
		var expected Tree
		if err := json.Unmarshal(python("capture-tree", tree), &expected); err != nil {
			t.Fatal(err)
		}
		captured, err := tree.capture()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(expected, captured) {
			t.Fatalf("tree capture differs:\nreleased=%+v\ncurrent=%+v", expected, captured)
		}
	}
}
