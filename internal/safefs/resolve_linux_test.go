package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestResolveFileDoesNotCleanDanglingLinkText(t *testing.T) {
	root := testkit.TempDir(t)
	existing := filepath.Join(root, "existing.env")
	testkit.WriteFile(t, existing, []byte("keep"), 0o600)
	link := filepath.Join(root, "link.env")
	if err := os.Symlink("missing/../existing.env", link); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveFile(link)
	if !errors.Is(err, unix.ENOENT) || err.Error() != "open "+link+": no such file or directory" {
		t.Fatalf("ResolveFile = %q, %v; want the open(2) error for the link", resolved, err)
	}
	if err := WriteFile(link, []byte("new"), Options{}); err == nil {
		t.Fatal("published through a symlink")
	}
	if content, _ := os.ReadFile(existing); string(content) != "keep" {
		t.Fatalf("unrelated file changed: %q", content)
	}
}

func TestResolveFileResolvesDotDotAfterSymlinkedDirectory(t *testing.T) {
	root := testkit.TempDir(t)
	real := filepath.Join(root, "real", "nested")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.env")
	// The kernel resolves "alias/.." to root/real, not lexically to root.
	if err := os.Symlink("alias/../target.env", link); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveFile(link)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Join(root, "real"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(canonical, "target.env"); resolved != want {
		t.Fatalf("ResolveFile = %q, want %q", resolved, want)
	}
	plain := filepath.Join(root, "plain.env")
	if resolved, err := ResolveFile(plain); err != nil || resolved != plain {
		t.Fatalf("absent plain path = %q, %v", resolved, err)
	}
}
