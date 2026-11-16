package releaseruntime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// An installer re-run that picks the channel's latest release never installs a
// release older than the active one; explicit selections keep the ordinary path.
func TestBootstrapRerunNeverSelectsAnOlderLatestRelease(t *testing.T) {
	script, err := filepath.Abs("../../../dev/bootstrap-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		active    string
		arguments []string
		noOp      bool
	}{
		"newer active":       {active: "1.3.0-aaaaaaaaaaaa", noOp: true},
		"newer patch":        {active: "1.2.10-aaaaaaaaaaaa", noOp: true},
		"newer pre-release":  {active: "1.3.0-rc.1-aaaaaaaaaaaa", noOp: true},
		"equal active":       {active: "1.2.3-aaaaaaaaaaaa"},
		"older pre-release":  {active: "1.2.3-rc.1-aaaaaaaaaaaa"},
		"older active":       {active: "1.2.2-aaaaaaaaaaaa"},
		"unversioned active": {active: "release-a"},
		"explicit downgrade": {active: "1.3.0-aaaaaaaaaaaa", arguments: []string{"--version", "1.2.3"}},
	} {
		t.Run(name, func(t *testing.T) {
			root := testkit.TempDir(t)
			testkit.WriteFile(t, filepath.Join(root, "curl"), []byte(fakeLatestReleaseCurl), 0o700)
			testkit.WriteFile(t, filepath.Join(root, "headers-1"),
				[]byte("HTTP/2 302\r\nLocation: "+stableReleaseURL+"\r\n\r\n"), 0o600)
			testkit.WriteFile(t, filepath.Join(root, "status-1"), []byte("302"), 0o600)
			testkit.WriteFile(t, filepath.Join(root, "headers-2"), []byte("HTTP/2 200\r\n\r\n"), 0o600)
			testkit.WriteFile(t, filepath.Join(root, "status-2"), []byte("200"), 0o600)
			home := filepath.Join(root, "home")
			runtimeRoot := filepath.Join(home, ".subyard", "runtime")
			if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", test.active), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(home, ".subyard"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/"+test.active, filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", append([]string{script, "--yes"}, test.arguments...)...)
			command.Env = []string{
				"PATH=" + root + ":" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=/tmp", "SHELL=/bin/bash",
				"FAKE_LOG=" + filepath.Join(root, "requests"), "FAKE_RESPONSES=" + root,
			}
			output, err := command.CombinedOutput()
			cache := filepath.Join(home, ".subyard", "releases")
			if test.noOp {
				version := test.active[:strings.LastIndex(test.active, "-")]
				want := fmt.Sprintf("Installed version %s is newer than the latest published release 1.2.3; no release was selected.", version)
				if err != nil || !strings.Contains(string(output), want) ||
					!strings.Contains(string(output), "yard update --version 1.2.3 --check") {
					t.Fatalf("bootstrap = %v: %s", err, output)
				}
				if _, statErr := os.Lstat(cache); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("no-op created the release cache: %v", statErr)
				}
				requests, readErr := os.ReadFile(filepath.Join(root, "requests"))
				if readErr != nil || strings.Count(string(requests), "\n") != 2 {
					t.Fatalf("no-op downloaded assets: %q, %v", requests, readErr)
				}
				return
			}
			// Ordinary selections reach the download, whose fixture bytes stop
			// at checksum validation before any installer can run.
			if err == nil || strings.Contains(string(output), "no release was selected") ||
				!strings.Contains(string(output), "installer checksum mismatch") {
				t.Fatalf("bootstrap = %v: %s", err, output)
			}
		})
	}
}
