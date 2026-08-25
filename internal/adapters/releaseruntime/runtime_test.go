package releaseruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestPrepareReturnsTypedReleaseActions(t *testing.T) {
	home := t.TempDir()
	var output bytes.Buffer
	release := New(Config{Environment: map[string]string{"HOME": home}, Stdout: &output})
	defaults, _, err := release.parse([]string{"--version", "1.2.3"})
	if err != nil || defaults.repository != "Subyard/Subyard" {
		t.Fatalf("invalid default release repository: %q, %v", defaults.repository, err)
	}

	help, err := release.Prepare(context.Background(), []string{"--help"})
	if err != nil || help.Action != "update.help" || help.Changed || help.Execute(context.Background()) != nil ||
		!strings.Contains(output.String(), "Usage: yard update") {
		t.Fatalf("invalid help operation: %#v, %q, %v", help, output.String(), err)
	}
	check, err := release.Prepare(context.Background(), []string{"--version", "1.2.3", "--check"})
	if err != nil || check.Action != "update.check" || !check.Changed || check.RefreshConfigs {
		t.Fatalf("update check must be a typed bounded write: %#v, %v", check, err)
	}
	update, err := release.Prepare(context.Background(), []string{"--version", "1.2.3"})
	if err != nil || update.Action != "update.activate" || !update.Changed || !update.RefreshConfigs ||
		update.ActiveLauncher != filepath.Join(home, ".subyard", "runtime", "current", "bin", "yard") ||
		!strings.Contains(strings.Join(update.Consequences, " "), "lifecycle migration") ||
		strings.Contains(strings.Join(check.Consequences, " "), "lifecycle migration") {
		t.Fatalf("release migration consequences are incomplete: update=%#v check=%#v err=%v",
			update.Consequences, check.Consequences, err)
	}
	explicitRoot := filepath.Join(home, "explicit-runtime")
	explicit, err := release.Prepare(context.Background(), []string{
		"--version", "1.2.3", "--runtime-root", explicitRoot,
	})
	if err != nil || explicit.ActiveLauncher != filepath.Join(explicitRoot, "current", "bin", "yard") {
		t.Fatalf("explicit runtime launcher is invalid: %#v, %v", explicit, err)
	}
	prepareRollbackRuntime(t, explicitRoot, "current-a", "previous-b")
	rollback, err := release.Prepare(context.Background(), []string{"--runtime-root", explicitRoot, "--rollback"})
	if err != nil || rollback.Action != "update.rollback" || !rollback.Changed || !rollback.RefreshConfigs ||
		rollback.ActiveLauncher != filepath.Join(explicitRoot, "current", "bin", "yard") {
		t.Fatalf("rollback must refresh materialized config: %#v, %v", rollback, err)
	}
	for _, arguments := range [][]string{
		{"--offline"},
		{"--runtime-root", "relative", "--version", "1"},
		{"--version", "bad/version"},
		{"--rollback", "--check"},
		{"--channel", "edge", "--version", "1"},
	} {
		if _, err := release.Prepare(context.Background(), arguments); err == nil {
			t.Fatalf("invalid arguments were accepted: %q", arguments)
		}
	}
	if err := (Prepared{}).Execute(context.Background()); err == nil {
		t.Fatal("empty prepared release operation was executable")
	}
}

func TestPrepareReportsGitHubLatestReleaseHTTPStatus(t *testing.T) {
	release := New(Config{
		Environment: map[string]string{"HOME": t.TempDir()},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Body:       io.NopCloser(strings.NewReader(`{"message":"rate limit exceeded"}`)),
			}, nil
		})},
	})

	_, err := release.Prepare(context.Background(), nil)
	if err == nil || err.Error() != "GitHub latest release request returned 403 Forbidden" {
		t.Fatalf("latest release error = %v", err)
	}
}

func TestPrepareRollbackFailsBeforePlanningWithoutValidPreviousRuntime(t *testing.T) {
	root := t.TempDir()
	release := New(Config{Environment: map[string]string{"HOME": root}, Stdout: &bytes.Buffer{}})
	_, err := release.Prepare(context.Background(), []string{
		"--runtime-root", filepath.Join(root, "runtime"), "--rollback",
	})
	if err == nil || !strings.Contains(err.Error(), "previous") {
		t.Fatalf("rollback precondition error = %v", err)
	}
}

func TestPrepareRollbackSelfCheckIsolatesThePreviousRuntimeFromCurrentConfig(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	prepareRollbackRuntime(t, runtimeRoot, "current-a", "previous-b")
	probeHome := filepath.Join(runtimeRoot, "releases", "previous-b")
	capture := filepath.Join(root, "previous-environment")
	probe := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "${HOME-}" "${SUBYARD_HOME-unset}" "${CODING_TOOL_INTEGRATIONS-unset}" > %q
printf 'yard 1.2.3\n'
`, capture)
	if err := os.WriteFile(
		filepath.Join(probeHome, "bin", "yard-engine"), []byte(probe), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	release := New(Config{Environment: map[string]string{
		"HOME": root, "SUBYARD_HOME": filepath.Join(root, "live-data"),
		"CODING_TOOL_INTEGRATIONS": "none",
	}, Stdout: &bytes.Buffer{}})

	if _, err := release.Prepare(context.Background(), []string{
		"--runtime-root", runtimeRoot, "--rollback",
	}); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := probeHome + "\nunset\nunset\n"
	if string(actual) != want {
		t.Fatalf("previous runtime self-check environment = %q, want %q", actual, want)
	}
}

func TestPrepareRejectsUnsafeReleaseRootsBeforePlanning(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheLink := filepath.Join(root, "cache-link")
	if err := os.Symlink(external, cacheLink); err != nil {
		t.Fatal(err)
	}
	runtimeLink := filepath.Join(root, "runtime-link")
	if err := os.Symlink(external, runtimeLink); err != nil {
		t.Fatal(err)
	}
	unsafeCache := filepath.Join(root, "unsafe-cache")
	if err := os.Mkdir(unsafeCache, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeCache, 0o770); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name        string
		cache       string
		runtimeRoot string
		arguments   []string
	}{
		{
			name: "relative check cache", cache: "relative-cache",
			arguments: []string{"--check", "--version", "1.2.3"},
		},
		{
			name: "filesystem root check cache", cache: string(filepath.Separator),
			arguments: []string{"--check", "--version", "1.2.3"},
		},
		{
			name: "symlink ancestor check cache", cache: filepath.Join(cacheLink, "releases"),
			arguments: []string{"--check", "--version", "1.2.3"},
		},
		{
			name: "writable check cache", cache: unsafeCache,
			arguments: []string{"--check", "--version", "1.2.3"},
		},
		{
			name: "symlink activation runtime root", cache: filepath.Join(root, "cache"),
			runtimeRoot: runtimeLink, arguments: []string{"--version", "1.2.3"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeRoot := test.runtimeRoot
			if runtimeRoot == "" {
				runtimeRoot = filepath.Join(root, "runtime")
			}
			environment := map[string]string{
				"HOME": root, "SUBYARD_HOME": root,
				"YARD_RELEASE_CACHE": test.cache,
				"YARD_RUNTIME_ROOT":  runtimeRoot,
			}
			release := New(Config{Environment: environment, Stdout: &bytes.Buffer{}})
			if _, err := release.Prepare(context.Background(), test.arguments); err == nil {
				t.Fatal("unsafe release roots were accepted")
			}
		})
	}
}

func TestPreparedReleaseRejectsStaleRuntimeLinksBeforeInstaller(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	prepareRollbackRuntime(t, runtimeRoot, "current-a", "previous-b")
	capture := filepath.Join(root, "installer-ran")
	installer := filepath.Join(root, "installer.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\ntouch \"$RELEASE_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	release := New(Config{Environment: map[string]string{
		"HOME": root, "RELEASE_CAPTURE": capture,
	}, Installer: installer, Stdout: &bytes.Buffer{}})
	prepared, err := release.Prepare(context.Background(), []string{
		"--runtime-root", runtimeRoot, "--rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/previous-c", filepath.Join(runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("stale release error = %v", err)
	}
	if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale release reached installer: %v", err)
	}
}

func TestPreparedCheckRejectsReleaseCacheBoundaryDriftBeforeWrite(t *testing.T) {
	for _, drift := range []string{"symlink", "permissions"} {
		t.Run(drift, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, "cache")
			runtimeRoot := filepath.Join(root, "runtime")
			if drift == "permissions" {
				if err := os.Mkdir(cache, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			capture := filepath.Join(root, "installer-ran")
			installer := filepath.Join(root, "installer.sh")
			if err := os.WriteFile(installer, []byte("#!/bin/sh\ntouch \"$RELEASE_CAPTURE\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			release := New(Config{Environment: map[string]string{
				"HOME": root, "YARD_RELEASE_CACHE": cache, "YARD_RUNTIME_ROOT": runtimeRoot,
				"YARD_RELEASE_BASE_URL": "file://" + filepath.Join(root, "missing-assets"),
				"RELEASE_CAPTURE":       capture,
			}, Installer: installer, Stdout: &bytes.Buffer{}})
			prepared, err := release.Prepare(context.Background(), []string{
				"--check", "--version", "1.2.3",
			})
			if err != nil {
				t.Fatal(err)
			}

			switch drift {
			case "symlink":
				external := filepath.Join(root, "external-cache")
				if err := os.Mkdir(external, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, cache); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(cache, 0o770); err != nil {
					t.Fatal(err)
				}
			}

			if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("release cache %s drift error = %v", drift, err)
			}
			if _, err := os.Stat(filepath.Join(cache, "1.2.3")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("release cache %s drift wrote cache data: %v", drift, err)
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("release cache %s drift reached installer: %v", drift, err)
			}
		})
	}
}

func TestPreparedActivationRejectsRuntimeRootBoundaryDriftBeforeEffects(t *testing.T) {
	for _, drift := range []string{"symlink", "permissions"} {
		t.Run(drift, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, "cache")
			runtimeRoot := filepath.Join(root, "runtime")
			if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(root, "installer-ran")
			installer := filepath.Join(root, "installer.sh")
			if err := os.WriteFile(installer, []byte("#!/bin/sh\ntouch \"$RELEASE_CAPTURE\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			release := New(Config{Environment: map[string]string{
				"HOME": root, "YARD_RELEASE_CACHE": cache, "YARD_RUNTIME_ROOT": runtimeRoot,
				"YARD_RELEASE_BASE_URL": "file://" + filepath.Join(root, "missing-assets"),
				"RELEASE_CAPTURE":       capture,
			}, Installer: installer, Stdout: &bytes.Buffer{}})
			prepared, err := release.Prepare(context.Background(), []string{"--version", "1.2.3"})
			if err != nil {
				t.Fatal(err)
			}

			switch drift {
			case "symlink":
				external := filepath.Join(root, "external-runtime")
				if err := os.Mkdir(external, 0o700); err != nil {
					t.Fatal(err)
				}
				original := filepath.Join(root, "prepared-runtime")
				if err := os.Rename(runtimeRoot, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, runtimeRoot); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(runtimeRoot, 0o770); err != nil {
					t.Fatal(err)
				}
			}

			if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("activation runtime root %s drift error = %v", drift, err)
			}
			if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("activation runtime root %s drift populated cache: %v", drift, err)
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("activation runtime root %s drift reached installer: %v", drift, err)
			}
		})
	}
}

func TestPreparedRollbackRejectsRuntimeRootBoundaryDriftBeforeEngineOrInstaller(t *testing.T) {
	for _, drift := range []string{"symlink", "permissions"} {
		t.Run(drift, func(t *testing.T) {
			root := t.TempDir()
			runtimeRoot := filepath.Join(root, "runtime")
			prepareRollbackRuntime(t, runtimeRoot, "current-a", "previous-b")
			engineCapture := filepath.Join(root, "engine-ran-after-prepare")
			installerCapture := filepath.Join(root, "installer-ran")
			installer := filepath.Join(root, "installer.sh")
			if err := os.WriteFile(installer, []byte("#!/bin/sh\ntouch \"$INSTALLER_CAPTURE\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			release := New(Config{Environment: map[string]string{
				"HOME": root, "ENGINE_CAPTURE": engineCapture, "INSTALLER_CAPTURE": installerCapture,
			}, Installer: installer, Stdout: &bytes.Buffer{}})
			prepared, err := release.Prepare(context.Background(), []string{
				"--runtime-root", runtimeRoot, "--rollback",
			})
			if err != nil {
				t.Fatal(err)
			}

			probeEngine := []byte("#!/bin/sh\ntouch \"$ENGINE_CAPTURE\"\nprintf 'yard 1.2.3\\n'\n")
			switch drift {
			case "symlink":
				external := filepath.Join(root, "external-runtime")
				prepareRollbackRuntime(t, external, "current-a", "previous-b")
				if err := os.WriteFile(filepath.Join(external, "releases", "previous-b", "bin", "yard-engine"), probeEngine, 0o700); err != nil {
					t.Fatal(err)
				}
				original := filepath.Join(root, "prepared-runtime")
				if err := os.Rename(runtimeRoot, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, runtimeRoot); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.WriteFile(filepath.Join(runtimeRoot, "releases", "previous-b", "bin", "yard-engine"), probeEngine, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(runtimeRoot, 0o770); err != nil {
					t.Fatal(err)
				}
			}

			if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("rollback runtime root %s drift error = %v", drift, err)
			}
			if _, err := os.Stat(engineCapture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback runtime root %s drift executed engine: %v", drift, err)
			}
			if _, err := os.Stat(installerCapture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback runtime root %s drift reached installer: %v", drift, err)
			}
		})
	}
}

func prepareRollbackRuntime(t *testing.T, root, current, previous string) {
	t.Helper()
	for _, name := range []string{current, previous} {
		engine := filepath.Join(root, "releases", name, "bin", "yard-engine")
		if err := os.MkdirAll(filepath.Dir(engine), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(engine, []byte("#!/bin/sh\nprintf 'yard 1.2.3\\n'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/"+current, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+previous, filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteDownloadsAssetsAndPassesValidatedEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("release runtime supports Linux amd64/arm64")
	}
	root := t.TempDir()
	assets := filepath.Join(root, "assets")
	cache := filepath.Join(root, "cache")
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "subyard-1.2.3-linux-" + runtime.GOARCH + ".tar.gz"
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		if err := os.WriteFile(filepath.Join(assets, name+suffix), []byte("fixture"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	capture := filepath.Join(root, "installer.args")
	installer := filepath.Join(root, "installer.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\nset -eu\n[ \"$RELEASE_SENTINEL\" = fixture ]\nprintf '%s\\n' \"$@\" > \"$RELEASE_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELEASE_SENTINEL", "ambient-must-not-leak")
	var output bytes.Buffer
	release := New(Config{
		Environment: map[string]string{
			"HOME": root, "SUBYARD_HOME": root, "YARD_RELEASE_BASE_URL": "file://" + assets,
			"YARD_RELEASE_CACHE": cache, "RELEASE_SENTINEL": "fixture", "RELEASE_CAPTURE": capture,
		},
		Installer: installer, Stdout: &output, Stderr: &output,
	})
	prepared, err := release.Prepare(context.Background(), []string{
		"--runtime-root", runtimeRoot, "--version", "1.2.3", "--check",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatalf("execute release: %v (%s)", err, output.String())
	}
	arguments, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(arguments))
	for _, required := range []string{"--runtime-root", runtimeRoot, "--check"} {
		if !slices.Contains(lines, required) {
			t.Fatalf("installer arguments omit %q: %q", required, lines)
		}
	}
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		path := filepath.Join(cache, "1.2.3", name+suffix)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("cached asset is missing or unsafe: %s (%v)", path, err)
		}
	}
	if !strings.Contains(output.String(), "available=1.2.3") {
		t.Fatalf("release status was not reported: %q", output.String())
	}

	currentEngine := filepath.Join(runtimeRoot, "releases", "current-a", "bin", "yard-engine")
	if err := os.MkdirAll(filepath.Dir(currentEngine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		currentEngine,
		[]byte("#!/bin/sh\nprintf 'yard 1.2.3\\n'\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/current-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(capture); err != nil {
		t.Fatal(err)
	}
	prepared, err = release.Prepare(context.Background(), []string{
		"--runtime-root", runtimeRoot, "--version", "1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatalf("execute same-version release: %v (%s)", err, output.String())
	}
	arguments, err = os.ReadFile(capture)
	if err != nil {
		t.Fatal("same-version update skipped the installer")
	}
	if slices.Contains(strings.Fields(string(arguments)), "--check") ||
		!strings.Contains(output.String(), "runtime is already current; checking migrations") {
		t.Fatalf("same-version update did not check migrations: args=%q output=%q",
			arguments, output.String())
	}
}
