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

type bootstrapInstallation struct {
	home, runtimeRoot, link string
	// Steps a completed earlier run leaves behind.
	journal, inventory, entrypoints, startupFiles, runtime bool
}

func writeBootstrapInstallation(t *testing.T, installation bootstrapInstallation, release string) {
	t.Helper()
	data := filepath.Join(installation.home, ".subyard")
	if err := os.MkdirAll(filepath.Join(installation.runtimeRoot, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if installation.runtime {
		if err := os.MkdirAll(filepath.Join(installation.runtimeRoot, "releases", release, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, binary := range []string{"yard", "yard-engine"} {
			testkit.WriteFile(t, filepath.Join(installation.runtimeRoot, "releases", release, "bin", binary), []byte("#!/bin/sh\nexit 0\n"), 0o700)
		}
	}
	if err := os.Symlink(installation.link, filepath.Join(installation.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if installation.journal {
		if err := os.MkdirAll(filepath.Join(installation.home, ".config", "subyard", "release-transition", "v2"), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(installation.home, ".config", "subyard", "release-transition", "v2", "journal.json"),
			[]byte(`{"checkpoint":"complete"}`), 0o600)
	}
	if installation.inventory {
		testkit.WriteFile(t, filepath.Join(data, "installation.json"), []byte("{}\n"), 0o600)
	}
	bin := filepath.Join(installation.home, ".local", "bin")
	if installation.entrypoints {
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"yard", "sy"} {
			if err := os.Symlink(filepath.Join(installation.runtimeRoot, "current", "bin", "yard"), filepath.Join(bin, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if installation.startupFiles {
		lines := "# Subyard CLI login PATH\n# Subyard CLI login completion\n# Subyard CLI completion\nexport PATH=\"" + bin + ":$PATH\"\n"
		for _, name := range []string{".bashrc", ".profile", ".zshrc", ".zprofile"} {
			testkit.WriteFile(t, filepath.Join(installation.home, name), []byte(lines), 0o600)
		}
	}
}

// An installer re-run that picks the channel's latest release never installs a
// release older than the active one; explicit selections keep the ordinary path.
func TestBootstrapRerunNeverSelectsAnOlderLatestRelease(t *testing.T) {
	script, err := filepath.Abs("../../../dev/bootstrap-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	complete := bootstrapInstallation{journal: true, inventory: true, entrypoints: true, startupFiles: true, runtime: true}
	for name, test := range map[string]struct {
		release      string
		installation bootstrapInstallation
		absoluteLink bool
		customRoot   bool
		arguments    []string
		environment  []string
		// "no-op", "refuse" or "" for the ordinary download path.
		want string
	}{
		"newer active":          {release: "1.3.0-aaaaaaaaaaaa", installation: complete, want: "no-op"},
		"newer patch":           {release: "1.2.10-aaaaaaaaaaaa", installation: complete, want: "no-op"},
		"newer pre-release":     {release: "1.3.0-rc.1-aaaaaaaaaaaa", installation: complete, want: "no-op"},
		"newer build metadata":  {release: "1.3.0+build.5-aaaaaaaaaaaa", installation: complete, want: "no-op"},
		"custom runtime root":   {release: "1.3.0-aaaaaaaaaaaa", installation: complete, customRoot: true, want: "no-op"},
		"interrupted install":   {release: "1.3.0-aaaaaaaaaaaa", installation: bootstrapInstallation{runtime: true}, want: "refuse"},
		"missing entrypoints":   {release: "1.3.0-aaaaaaaaaaaa", installation: bootstrapInstallation{journal: true, inventory: true, startupFiles: true, runtime: true}, want: "refuse"},
		"missing startup files": {release: "1.3.0-aaaaaaaaaaaa", installation: bootstrapInstallation{journal: true, inventory: true, entrypoints: true, runtime: true}, want: "refuse"},
		"dangling current":      {release: "1.3.0-aaaaaaaaaaaa", installation: bootstrapInstallation{journal: true, inventory: true, entrypoints: true, startupFiles: true}, want: "refuse"},
		"absolute current":      {release: "1.3.0-aaaaaaaaaaaa", installation: complete, absoluteLink: true, want: "refuse"},
		"malformed pre-release": {release: "1.3.0-01-aaaaaaaaaaaa", installation: complete},
		"equal active":          {release: "1.2.3-aaaaaaaaaaaa", installation: complete},
		"older pre-release":     {release: "1.2.3-rc.1-aaaaaaaaaaaa", installation: complete},
		"older active":          {release: "1.2.2-aaaaaaaaaaaa", installation: complete},
		"unversioned active":    {release: "release-a", installation: complete},
		"explicit downgrade":    {release: "1.3.0-aaaaaaaaaaaa", installation: complete, arguments: []string{"--version", "1.2.3"}},
		"environment downgrade": {release: "1.3.0-aaaaaaaaaaaa", installation: complete, environment: []string{"YARD_RELEASE_VERSION=1.2.3"}},
	} {
		t.Run(name, func(t *testing.T) {
			root := testkit.TempDir(t)
			testkit.WriteFile(t, filepath.Join(root, "curl"), []byte(fakeLatestReleaseCurl), 0o700)
			testkit.WriteFile(t, filepath.Join(root, "headers-1"),
				[]byte("HTTP/2 302\r\nLocation: "+stableReleaseURL+"\r\n\r\n"), 0o600)
			testkit.WriteFile(t, filepath.Join(root, "status-1"), []byte("302"), 0o600)
			testkit.WriteFile(t, filepath.Join(root, "headers-2"), []byte("HTTP/2 200\r\n\r\n"), 0o600)
			testkit.WriteFile(t, filepath.Join(root, "status-2"), []byte("200"), 0o600)
			installation := test.installation
			installation.home = filepath.Join(root, "home")
			installation.runtimeRoot = filepath.Join(installation.home, ".subyard", "runtime")
			arguments := append([]string{script, "--yes"}, test.arguments...)
			rootArgument := ""
			if test.customRoot {
				installation.runtimeRoot = filepath.Join(root, "custom runtime")
				arguments = append(arguments, "--runtime-root", installation.runtimeRoot)
				rootArgument = " --runtime-root " + strings.ReplaceAll(installation.runtimeRoot, " ", `\ `)
			}
			installation.link = "releases/" + test.release
			if test.absoluteLink {
				installation.link = filepath.Join(installation.runtimeRoot, "releases", test.release)
			}
			writeBootstrapInstallation(t, installation, test.release)
			command := exec.Command("bash", arguments...)
			command.Env = append([]string{
				"PATH=" + root + ":" + os.Getenv("PATH"), "HOME=" + installation.home, "TMPDIR=/tmp", "SHELL=/bin/bash",
				"FAKE_LOG=" + filepath.Join(root, "requests"), "FAKE_RESPONSES=" + root,
			}, test.environment...)
			output, err := command.CombinedOutput()
			cache := filepath.Join(installation.home, ".subyard", "releases")
			version := test.release[:strings.LastIndex(test.release, "-")]
			switch test.want {
			case "no-op":
				want := "Installed version " + version + " is newer than the latest published release 1.2.3; no release was selected."
				if err != nil || !strings.Contains(string(output), want) ||
					!strings.Contains(string(output), "yard update --version 1.2.3"+rootArgument+" --check") {
					t.Fatalf("bootstrap = %v: %s", err, output)
				}
			case "refuse":
				want := "repair it with: subyard-install.sh --version " + version
				if err == nil || !strings.Contains(string(output), want) || strings.Contains(string(output), "no release was selected") {
					t.Fatalf("bootstrap = %v: %s", err, output)
				}
			default:
				// Ordinary selections reach the download, whose fixture bytes stop
				// at checksum validation before any installer can run.
				if err == nil || strings.Contains(string(output), "newer than the latest published release") ||
					!strings.Contains(string(output), "installer checksum mismatch") {
					t.Fatalf("bootstrap = %v: %s", err, output)
				}
				return
			}
			if _, statErr := os.Lstat(cache); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("guard created the release cache: %v", statErr)
			}
			requests, readErr := os.ReadFile(filepath.Join(root, "requests"))
			if readErr != nil || strings.Count(string(requests), "\n") != 2 {
				t.Fatalf("guard downloaded assets: %q, %v", requests, readErr)
			}
		})
	}
}
