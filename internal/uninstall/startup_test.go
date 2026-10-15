package uninstall

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestStartupEditPreservesForeignShellAndNeededPath(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "unused bin", true: "shared bin"}[keep], func(t *testing.T) {
			root := testkit.TempDir(t)
			file := filepath.Join(root, "bashrc")
			runtimeRoot := filepath.Join(root, "runtime")
			bin := filepath.Join(root, "bin")
			pathBlock := []byte("# Subyard CLI interactive PATH\nexport PATH=\"" + bin + ":$PATH\"\n")
			owned := []byte("# Subyard CLI completion\n[ -f \"" + runtimeRoot + "/current/completions/yard.bash\" ] && source \"" + runtimeRoot + "/current/completions/yard.bash\"\n")
			foreign := []byte("export USER_SETTING=value\n# Subyard CLI completion\n[ -f \"/other/runtime/current/completions/yard.bash\" ] && source \"/other/runtime/current/completions/yard.bash\"\n")
			testkit.WriteFile(t, file, bytes.Join([][]byte{foreign, pathBlock, owned}, nil), 0644)
			edit, err := PrepareStartup(file, runtimeRoot, bin, keep)
			if err != nil {
				t.Fatal(err)
			}
			edit, err = edit.PrepareApply()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := edit.CommitApply(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			expected := foreign
			if keep {
				expected = append(append([]byte{}, foreign...), []byte("export PATH=\""+bin+":$PATH\"\n")...)
			}
			if !bytes.Equal(got, expected) {
				t.Fatalf("startup mismatch: %q", got)
			}
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm() != 0644 {
				t.Fatalf("mode changed: %v", err)
			}
		})
	}
}

func TestStartupEditRefusesDriftBeforeWriting(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "profile")
	testkit.WriteFile(t, path, []byte("# Subyard CLI interactive PATH\nexport PATH=\"/owned/bin:$PATH\"\n"), 0644)
	edit, err := PrepareStartup(path, "/owned/runtime", "/owned/bin", false)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("user replacement\n"), 0644)
	if _, err := edit.PrepareApply(); err == nil {
		t.Fatal("changed startup file replaced")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "user replacement\n" {
		t.Fatalf("foreign replacement changed: %q %v", got, err)
	}
}

func TestKeepBinPathUsesExactLauncherNames(t *testing.T) {
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "yard"), []byte("owned"), 0755)
	testkit.WriteFile(t, filepath.Join(root, "sy"), []byte("owned"), 0755)
	keep, err := KeepBinPath(root)
	if err != nil || keep {
		t.Fatalf("unused bin kept=%v err=%v", keep, err)
	}
	testkit.WriteFile(t, filepath.Join(root, "YARD"), []byte("foreign"), 0755)
	keep, err = KeepBinPath(root)
	if err != nil || !keep {
		t.Fatalf("shared bin removed=%v err=%v", keep, err)
	}
}
