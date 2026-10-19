package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOwnerIntegrationWriteRestoresOriginalCAS(t *testing.T) {
	root := testkit.TempDir(t)
	dir := filepath.Join(testkit.TempDir(t), "config")
	path := filepath.Join(root, "yards", "named", "config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("AGENTS=none\n"), 0600)
	before, err := ReadPersistentFileSnapshot(root, path)
	if err != nil {
		t.Fatal(err)
	}
	p := &YardIntegrationWrite{Path: path, Before: before, Content: []byte("CODING_TOOL_INTEGRATIONS=\n"), root: root, yard: "named"}
	raw, err := RecordOwnerIntegrationWrite(p)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreOwnerIntegrationWrite(raw, dir, root, "named")
	if err != nil || !bytes.Equal(restored.Content, p.Content) {
		t.Fatal("original write not restored", err)
	}
	if _, err := RestoreOwnerIntegrationWrite(raw, dir, testkit.TempDir(t), "named"); err == nil {
		t.Fatal("different root admitted")
	}
	testkit.WriteFile(t, path+".new", before.Content, 0600)
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreOwnerIntegrationWrite(raw, dir, root, "named"); err == nil {
		t.Fatal("same-byte replacement admitted")
	}
}

func TestOwnerIntegrationWriteRetainsNonUTF8Source(t *testing.T) {
	root := testkit.TempDir(t)
	dir := filepath.Join(testkit.TempDir(t), "source-\xff", "config")
	source := filepath.Join(dir, "..", "private", "yards", "named.env")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, source, []byte("AGENTS=none\n"), 0600)
	before, err := persistentFileSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	p := &YardIntegrationWrite{Path: filepath.Join(root, "yards/named/config.env"), SourcePath: source, SourceBefore: before, Content: []byte("CODING_TOOL_INTEGRATIONS=\n"), root: root, yard: "named"}
	raw, err := RecordOwnerIntegrationWrite(p)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreOwnerIntegrationWrite(raw, dir, root, "named")
	if err != nil || restored.SourcePath != source {
		t.Fatal("original path lost", err)
	}
	testkit.WriteFile(t, source, []byte("AGENTS=changed\n"), 0600)
	if _, err := RestoreOwnerIntegrationWrite(raw, dir, root, "named"); err == nil {
		t.Fatal("source drift admitted")
	}
}
