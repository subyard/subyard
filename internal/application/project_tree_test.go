package application

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProjectArchiveAcceptsLegacyRegularTypeflag(t *testing.T) {
	payload := validProjectArchive(t, "legacy content")
	expected, err := ProjectArchiveTreeDigest(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	// Write the legacy NUL directly: tar.Writer normalizes it to TypeReg.
	payload[156] = '\x00'
	for index := 148; index < 156; index++ {
		payload[index] = ' '
	}
	var checksum int
	for _, value := range payload[:512] {
		checksum += int(value)
	}
	copy(payload[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	header, err := tar.NewReader(bytes.NewReader(payload)).Next()
	if err != nil || header.Typeflag != tar.TypeReg {
		t.Fatalf("legacy typeflag was not normalized: header=%#v err=%v", header, err)
	}
	actual, err := ProjectArchiveTreeDigest(bytes.NewReader(payload))
	if err != nil || actual != expected {
		t.Fatalf("legacy archive digest=%s want=%s err=%v", actual, expected, err)
	}
	manifest, err := readProjectArchiveManifest(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "content.txt"), []byte("legacy content"), 0o600)
	if err := manifest.verify(context.Background(), projectProcessExecutor{}, domain.Context{}, root); err != nil {
		t.Fatalf("verify legacy archive: %v", err)
	}
}

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
