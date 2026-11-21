package releaseruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestFetchKeepsSourceErrorBeforeDestinationRefusal(t *testing.T) {
	root := testkit.TempDir(t)
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(cache, "asset")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{}
	parsed := options{baseURL: "file://" + source}
	err := runtime.fetch(context.Background(), parsed, "asset", destination)
	if !errors.Is(err, os.ErrNotExist) || err.Error() != "open "+filepath.Join(source, "asset")+": no such file or directory" {
		t.Fatalf("missing source with a directory destination = %v", err)
	}

	testkit.WriteFile(t, filepath.Join(source, "asset"), []byte("bundle"), 0o600)
	victim := filepath.Join(root, "victim")
	testkit.WriteFile(t, victim, []byte("keep"), 0o600)
	link := filepath.Join(cache, "linked")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := runtime.fetch(context.Background(), parsed, "asset", link); err == nil {
		t.Fatal("download replaced a symlinked cache entry")
	}
	if content, _ := os.ReadFile(victim); string(content) != "keep" {
		t.Fatalf("symlink target changed: %q", content)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cache symlink replaced: %v", err)
	}
	entries, _ := os.ReadDir(cache)
	if len(entries) != 2 {
		t.Fatalf("staging file left behind: %v", entries)
	}
}
