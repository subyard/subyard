package testvmsruntime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRecipeBundleIsDeterministicAndExcludesLocalData(t *testing.T) {
	root := t.TempDir()
	for _, name := range recipeFiles {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("trusted recipe\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "operator-private.env"), []byte("must never enter image"), 0600); err != nil {
		t.Fatal(err)
	}
	first, manifest, err := recipeBundle(root)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := recipeBundle(root)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("nondeterministic archive: %v", err)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gzipReader)
	allowed := map[string]bool{"manifest.sha256": true}
	for _, name := range recipeFiles {
		allowed[name] = true
	}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !allowed[header.Name] || header.Typeflag != tar.TypeReg || header.Uid != 0 || header.Gid != 0 {
			t.Fatalf("untrusted entry: %+v", header)
		}
		delete(allowed, header.Name)
	}
	if len(allowed) != 0 || bytes.Contains(manifest, []byte("private")) {
		t.Fatal("manifest mismatch")
	}
	path := filepath.Join(root, recipeFiles[0])
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "operator-private.env"), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recipeBundle(root); err == nil {
		t.Fatal("accepted symlink into local data")
	}
}
