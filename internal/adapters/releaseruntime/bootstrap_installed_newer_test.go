package releaseruntime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// An installer re-run that picks the channel's latest release refuses, without
// changing anything, when the active release names a newer X.Y.Z core.
// Explicit selections and every other link keep the ordinary download path.
func TestBootstrapRerunRefusesAnOlderLatestRelease(t *testing.T) {
	script, err := filepath.Abs("../../../dev/bootstrap-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		release      string
		dangling     bool
		absoluteLink bool
		customRoot   bool
		arguments    []string
		environment  []string
		refuse       bool
	}{
		"newer core":             {release: "1.3.0-aaaaaaaaaaaa", refuse: true},
		"newer patch":            {release: "1.2.10-aaaaaaaaaaaa", refuse: true},
		"newer pre-release":      {release: "1.3.0-rc.1-aaaaaaaaaaaa", refuse: true},
		"build metadata":         {release: "1.3.0+b.5-aaaaaaaaaaaa", refuse: true},
		"non-semver pre-release": {release: "1.3.0-01-aaaaaaaaaaaa", refuse: true},
		"unsafe version text":    {release: "1.3.0-rc$x-aaaaaaaaaaaa", refuse: true},
		"leading-zero core":      {release: "01.3.0-aaaaaaaaaaaa"},
		"custom runtime root":    {release: "1.3.0-aaaaaaaaaaaa", customRoot: true, refuse: true},
		"dangling current":       {release: "1.3.0-aaaaaaaaaaaa", dangling: true, refuse: true},
		"absolute current":       {release: "1.3.0-aaaaaaaaaaaa", absoluteLink: true, refuse: true},
		"equal core":             {release: "1.2.3-aaaaaaaaaaaa"},
		"pre-release of latest":  {release: "1.2.3-rc.1-aaaaaaaaaaaa"},
		"older core":             {release: "1.2.2-aaaaaaaaaaaa"},
		"unversioned release":    {release: "release-a"},
		"explicit version":       {release: "1.3.0-aaaaaaaaaaaa", arguments: []string{"--version", "1.2.3"}},
		"environment version":    {release: "1.3.0-aaaaaaaaaaaa", environment: []string{"YARD_RELEASE_VERSION=1.2.3"}},
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
			data := filepath.Join(home, ".subyard")
			runtimeRoot := filepath.Join(data, "runtime")
			arguments := append([]string{script, "--yes"}, test.arguments...)
			rootArgument := ""
			if test.customRoot {
				runtimeRoot = filepath.Join(root, "custom runtime")
				arguments = append(arguments, "--runtime-root", runtimeRoot)
				rootArgument = " --runtime-root " + strings.ReplaceAll(runtimeRoot, " ", `\ `)
			}
			if err := os.MkdirAll(data, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases"), 0o700); err != nil {
				t.Fatal(err)
			}
			if !test.dangling {
				if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", test.release), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			link := "releases/" + test.release
			if test.absoluteLink {
				link = filepath.Join(runtimeRoot, link)
			}
			current := filepath.Join(runtimeRoot, "current")
			if err := os.Symlink(link, current); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", arguments...)
			command.Env = append([]string{
				"PATH=" + root + ":" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=/tmp", "SHELL=/bin/bash",
				"FAKE_LOG=" + filepath.Join(root, "requests"), "FAKE_RESPONSES=" + root,
			}, test.environment...)
			output, err := command.CombinedOutput()
			if !test.refuse {
				// Ordinary selections reach the download, whose fixture bytes stop
				// at checksum validation before any installer can run.
				if err == nil || strings.Contains(string(output), "newer than the latest published release") ||
					!strings.Contains(string(output), "installer checksum mismatch") {
					t.Fatalf("bootstrap = %v: %s", err, output)
				}
				return
			}
			version := test.release[:strings.LastIndex(test.release, "-")]
			if strings.ContainsAny(version, "$") {
				version = "1.3.0"
			}
			for _, want := range []string{
				"installed version " + version + " is newer than the latest published release 1.2.3; no release was selected and nothing was changed",
				"subyard-install.sh --version " + version + rootArgument + "\n",
				"an unpublished build needs its own assets",
				"subyard-install.sh --version 1.2.3" + rootArgument + "\n",
			} {
				if err == nil || !strings.Contains(string(output), want) {
					t.Fatalf("bootstrap = %v, missing %q: %s", err, want, output)
				}
			}
			if _, statErr := os.Lstat(filepath.Join(data, "releases")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("refusal created the release cache: %v", statErr)
			}
			requests, readErr := os.ReadFile(filepath.Join(root, "requests"))
			if readErr != nil || strings.Count(string(requests), "\n") != 2 {
				t.Fatalf("refusal made other requests: %q, %v", requests, readErr)
			}
			if target, linkErr := os.Readlink(current); linkErr != nil || target != link {
				t.Fatalf("refusal changed the current link: %q, %v", target, linkErr)
			}
		})
	}
}
