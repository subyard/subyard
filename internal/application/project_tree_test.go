package application

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProjectTreeReadOnlyObservationMatchesArchiveAndDetectsChanges(t *testing.T) {
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "file with newline\n.txt"), []byte("approved source"), 0o600)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, ".git", "unrelated"), []byte("excluded"), 0o600)
	if err := os.Symlink("file with newline\n.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	writer := tar.NewWriter(&payload)
	for _, header := range []*tar.Header{
		{Name: "file with newline\n.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len("approved source"))},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "file with newline\n.txt", Mode: 0o777},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte("approved source")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	expected, err := ProjectArchiveTreeDigest(bytes.NewReader(payload.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	observed, err := ObserveProjectTree(context.Background(), projectProcessExecutor{}, domain.Context{}, root)
	if err != nil || observed != expected {
		t.Fatalf("manifest comparison=%t err=%v", observed == expected, err)
	}
	testkit.WriteFile(t, filepath.Join(root, "file with newline\n.txt"), []byte("later edit"), 0o600)
	changed, err := ObserveProjectTree(context.Background(), projectProcessExecutor{}, domain.Context{}, root)
	if err != nil || changed == observed {
		t.Fatalf("change detected=%t err=%v", changed != observed, err)
	}
}
