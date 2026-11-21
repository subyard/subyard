package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
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
	type result struct {
		path  string
		op    string
		errno unix.Errno
	}
	ok := func(path string) result { return result{path: path} }
	fail := func(op string, errno unix.Errno) result { return result{op: op, errno: errno} }
	both := func(want result) [2]result { return [2]result{want, want} }
	cases := []struct {
		name  string
		input string
		want  [2]result // strict, create
	}{
		{"absent plain", filepath.Join(root, "plain.env"), both(ok(filepath.Join(root, "plain.env")))},
		{"regular plain", filepath.Join(root, "file"), both(ok(filepath.Join(root, "file")))},
		{"present entry", link("present", "existing.env"), both(ok(filepath.Join(base, "existing.env")))},
		{"absent entry", link("absent", "dir/new.env"), both(ok(filepath.Join(base, "dir", "new.env")))},
		{"dot-dot after alias", link("alias-up", "alias/../x.env"), both(ok(filepath.Join(base, "real", "x.env")))},
		{"missing dot-dot", link("missing-up", "missing/../existing.env"), both(fail("open", unix.ENOENT))},
		{"missing slash", link("missing-slash", "missing/"), both(fail("open", unix.ENOENT))},
		{"absolute missing parent", link("abs-new", filepath.Join(root, "new", "config")), [2]result{fail("open", unix.ENOENT), ok(filepath.Join(root, "new", "config"))}},
		{"relative missing parent", link("rel-new", "new/config"), [2]result{fail("open", unix.ENOENT), ok(filepath.Join(base, "new", "config"))}},
		{"through a file", link("file-x", "file/x"), both(fail("open", unix.ENOTDIR))},
		{"file slash", link("file-slash", "file/"), both(fail("open", unix.ENOTDIR))},
		{"file dot", link("file-dot", "file/."), both(fail("open", unix.ENOTDIR))},
		{"dot", link("dot", "."), both(ok(base))},
		{"dot-dot", link("dotdot", ".."), both(ok(filepath.Dir(base)))},
		{"dir slash", link("dir-slash", "dir/"), both(ok(filepath.Join(base, "dir")))},
		{"dir dot", link("dir-dot", "dir/."), both(ok(filepath.Join(base, "dir")))},
		{"plain directory", link("dir-name", "dir"), both(ok(filepath.Join(base, "dir")))},
		{"loop", link("loop", "loop"), both(fail("readlink", unix.ELOOP))},
	}
	for _, test := range cases {
		for i, resolve := range []func(string) (string, error){ResolveFile, ResolveFileForCreate} {
			mode, want := [2]string{"strict", "create"}[i], test.want[i]
			path, err := resolve(test.input)
			if want.errno == 0 {
				if err != nil || path != want.path {
					t.Errorf("%s/%s: got %q, %v; want %q", test.name, mode, path, err, want.path)
				}
				continue
			}
			text := want.op + " " + test.input + ": " + want.errno.Error()
			if err == nil || !errors.Is(err, want.errno) || err.Error() != text {
				t.Errorf("%s/%s: got %q, %v; want error %q (%d)", test.name, mode, path, err, text, want.errno)
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
