package safefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestResolveFileClasses(t *testing.T) {
	root := testkit.TempDir(t)
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"dir", "real/nested"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "file"), []byte("x"), 0o600)
	testkit.WriteFile(t, filepath.Join(root, "existing.env"), []byte("keep"), 0o600)
	if err := os.Symlink(filepath.Join(root, "real", "nested"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	link := func(name, text string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.Symlink(text, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	type result struct{ path, err string }
	cases := []struct {
		name           string
		input          string
		strict, create result
	}{
		{"absent plain", filepath.Join(root, "plain.env"), result{path: filepath.Join(root, "plain.env")}, result{path: filepath.Join(root, "plain.env")}},
		{"regular plain", filepath.Join(root, "file"), result{path: filepath.Join(root, "file")}, result{path: filepath.Join(root, "file")}},
		{"present entry", link("present", "existing.env"), result{path: filepath.Join(base, "existing.env")}, result{path: filepath.Join(base, "existing.env")}},
		{"absent entry", link("absent", "dir/new.env"), result{path: filepath.Join(base, "dir", "new.env")}, result{path: filepath.Join(base, "dir", "new.env")}},
		{"dot-dot after alias", link("alias-up", "alias/../x.env"), result{path: filepath.Join(base, "real", "x.env")}, result{path: filepath.Join(base, "real", "x.env")}},
		{"missing dot-dot", link("missing-up", "missing/../existing.env"), result{err: "open " + filepath.Join(root, "missing-up") + ": no such file or directory"}, result{err: "open " + filepath.Join(root, "missing-up") + ": no such file or directory"}},
		{"absolute missing parent", link("abs-new", filepath.Join(root, "new", "config")), result{err: "open " + filepath.Join(root, "abs-new") + ": no such file or directory"}, result{path: filepath.Join(root, "new", "config")}},
		{"relative missing parent", link("rel-new", "new/config"), result{err: "open " + filepath.Join(root, "rel-new") + ": no such file or directory"}, result{path: filepath.Join(base, "new", "config")}},
		{"through a file", link("file-x", "file/x"), result{err: "open " + filepath.Join(root, "file-x") + ": not a directory"}, result{err: "open " + filepath.Join(root, "file-x") + ": not a directory"}},
		{"file dot", link("file-dot", "file/."), result{err: "open " + filepath.Join(root, "file-dot") + ": not a directory"}, result{err: "open " + filepath.Join(root, "file-dot") + ": not a directory"}},
		{"dot", link("dot", "."), result{path: base}, result{path: base}},
		{"dot-dot", link("dotdot", ".."), result{path: filepath.Dir(base)}, result{path: filepath.Dir(base)}},
		{"dir slash", link("dir-slash", "dir/"), result{path: filepath.Join(base, "dir")}, result{path: filepath.Join(base, "dir")}},
		{"dir dot", link("dir-dot", "dir/."), result{path: filepath.Join(base, "dir")}, result{path: filepath.Join(base, "dir")}},
		{"plain directory", link("dir-name", "dir"), result{path: filepath.Join(base, "dir")}, result{path: filepath.Join(base, "dir")}},
	}
	loop := link("loop", "loop")
	cases = append(cases, struct {
		name           string
		input          string
		strict, create result
	}{"loop", loop, result{err: "readlink " + loop + ": too many levels of symbolic links"}, result{err: "readlink " + loop + ": too many levels of symbolic links"}})
	for _, test := range cases {
		for mode, want := range map[string]result{"strict": test.strict, "create": test.create} {
			resolve := ResolveFile
			if mode == "create" {
				resolve = ResolveFileForCreate
			}
			path, err := resolve(test.input)
			got := result{path: path}
			if err != nil {
				got = result{err: err.Error()}
			}
			if got != want {
				t.Errorf("%s/%s: got %+v, want %+v", test.name, mode, got, want)
			}
		}
	}
	if content, _ := os.ReadFile(filepath.Join(root, "existing.env")); string(content) != "keep" {
		t.Fatalf("unrelated file changed: %q", content)
	}
}

func TestResolveFileFollowsLongRelativeChains(t *testing.T) {
	root := testkit.TempDir(t)
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.Repeat("./", 1100)
	if err := os.Symlink(prefix+"b", filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(prefix+"target", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	for _, resolve := range []func(string) (string, error){ResolveFile, ResolveFileForCreate} {
		if path, err := resolve(filepath.Join(root, "a")); err != nil || path != filepath.Join(base, "target") {
			t.Fatalf("long chain = %q, %v", path, err)
		}
	}
}

func TestResolveFileDoesNotPublishThroughCleanedText(t *testing.T) {
	root := testkit.TempDir(t)
	existing := filepath.Join(root, "existing.env")
	testkit.WriteFile(t, existing, []byte("keep"), 0o600)
	link := filepath.Join(root, "link.env")
	if err := os.Symlink("missing/../existing.env", link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("new"), Options{}); err == nil {
		t.Fatal("published through a symlink")
	}
	if content, _ := os.ReadFile(existing); string(content) != "keep" {
		t.Fatalf("unrelated file changed: %q", content)
	}
}
