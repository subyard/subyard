package architecture

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestVerandaContractMirror(t *testing.T) {
	core := filepath.Join("..", "..", "api", "yard-rpc", "v1")
	mirror := filepath.Join("..", "..", "veranda", "contract", "yard-rpc", "v1")
	compare := func(source, target string) {
		t.Helper()
		err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			original, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			copied, err := os.ReadFile(filepath.Join(target, relative))
			if err != nil {
				return err
			}
			if !bytes.Equal(original, copied) {
				t.Errorf("contract mirror differs: %s", relative)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	compare(core, mirror)
	compare(mirror, core)
}
