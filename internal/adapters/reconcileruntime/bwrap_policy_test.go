package reconcileruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBubblewrapPolicyExtractsDistributionParser(t *testing.T) {
	functions, _, found := strings.Cut(bubblewrapPolicy, "\ncompile_policy()")
	if !found {
		t.Fatal("policy compiler definition is missing")
	}
	for _, tc := range []struct{ name, path string }{
		{"Ubuntu 24.04", "sbin/apparmor_parser"},
		{"Debian 13", "usr/sbin/apparmor_parser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := testkit.TempDir(t)
			root := filepath.Join(dir, "package")
			for _, subdir := range []string{"DEBIAN", filepath.Dir(tc.path)} {
				path := filepath.Join(root, subdir)
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(root, "DEBIAN/control"),
				[]byte("Package: apparmor\nVersion: 1\nArchitecture: all\nMaintainer: Test <test@example.invalid>\nDescription: policy fixture\n"), 0o644)
			const parser = "#!/bin/sh\nexit 0\n"
			testkit.WriteFile(t, filepath.Join(root, tc.path), []byte(parser), 0o755)
			archive := filepath.Join(dir, "apparmor.deb")
			if output, err := exec.Command("dpkg-deb", "--build", "--root-owner-group", root, archive).CombinedOutput(); err != nil {
				t.Fatalf("build package: %v\n%s", err, output)
			}
			output, err := exec.Command("bash", "-c", functions+"\nextract_parser \"$2\"", "subyard", "observe", archive).CombinedOutput()
			if err != nil || string(output) != parser {
				t.Fatalf("extract %s: %v\n%s", tc.path, err, output)
			}
		})
	}
}
