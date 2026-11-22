package migration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBootstrapRediscoversInterruptedSourceIngressFromRecovery(t *testing.T) {
	requireJQ(t)
	for _, scenario := range []string{"entrypoint-switch", "entrypoint-switch-success", "entrypoint-switch-symlink-home", "runtime-symlink-home", "runtime-trailing-slash", "no-current", "shell-checkout", "native-checkout", "complete", "invalid-json", "wrong-data-home", "outside-home", "relative-root", "symlink-manifest"} {
		t.Run(scenario, func(t *testing.T) {
			sourceCheckout := scenario == "shell-checkout" || scenario == "native-checkout"
			noRuntime := scenario == "no-current" || sourceCheckout
			home := testkit.TempDir(t)
			runtimeLinked := scenario == "runtime-symlink-home" || scenario == "runtime-trailing-slash"
			bootstrapHome := home
			if scenario == "runtime-symlink-home" || scenario == "entrypoint-switch-symlink-home" {
				bootstrapHome = filepath.Join(home, "home-alias")
				if err := os.Symlink(home, bootstrapHome); err != nil {
					t.Fatal(err)
				}
			}
			requestData, requestConfig, requestBin := filepath.Join(bootstrapHome, "data"), filepath.Join(bootstrapHome, "config"), filepath.Join(bootstrapHome, "bin")
			data, bin := filepath.Join(home, "data"), filepath.Join(home, "bin")
			runtimeRoot, source := filepath.Join(data, "runtime"), filepath.Join(home, "source")
			cache := filepath.Join(data, "releases/fixture-test")
			bundleDigest := writeBootstrapFixtureAssets(t, cache)
			release := "releases/fixture-test-" + bundleDigest[:12]
			candidate := filepath.Join(runtimeRoot, release)
			recovery := filepath.Join(data, "recovery/pre-go-source")
			for _, directory := range []string{bin, filepath.Join(source, "bin"), filepath.Join(candidate, "bin"), cache} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for link, target := range map[string]string{
				filepath.Join(runtimeRoot, "current"): release,
				filepath.Join(bin, "yard"):            filepath.Join(runtimeRoot, "current/bin/yard"),
				filepath.Join(bin, "sy"):              filepath.Join(source, "bin/yard"),
			} {
				if noRuntime && link == filepath.Join(runtimeRoot, "current") {
					continue
				}
				if sourceCheckout {
					target = filepath.Join(source, "bin/yard")
				}
				if runtimeLinked && link != filepath.Join(runtimeRoot, "current") {
					target = filepath.Join(runtimeRoot, "current/bin/yard")
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			// Publishing and transition application stop at local recorder boundaries.
			writeTestFile(t, filepath.Join(source, "bin/yard"), 0o700, "#!/bin/sh\nexit 99\n")
			if sourceCheckout {
				launcher := "#!/bin/sh\n# thin dispatcher over scripts/\nexit 99\n"
				if scenario == "native-checkout" {
					launcher = string(readTestFile(t, filepath.Join("..", "..", "bin", "yard")))
				}
				writeTestFile(t, filepath.Join(source, "bin/yard"), 0o700, launcher)
				writeTestFile(t, filepath.Join(source, "config/commands.registry"), 0o600, "fixture\n")
				writeTestFile(t, filepath.Join(source, "completions/yard.bash"), 0o600, "fixture\n")
			}
			ingressLog, installerLog := filepath.Join(home, "ingress"), filepath.Join(home, "installer")
			classLog := filepath.Join(home, "class")
			writeTestFile(t, filepath.Join(candidate, "bin/yard"), 0o700, `#!/bin/sh
# Stable launcher for a release-installed native Go control-plane engine.
set -eu
case "$1" in update|migrate) ;; *) exit 99 ;; esac
printf '%s\n' "${SUBYARD_SOURCE_INGRESS_V1_ROOT-unset}" > "$TEST_INGRESS_LOG"
printf '%s\n' "${SUBYARD_SOURCE_INGRESS_V1_CLASS-unset}" > "$TEST_CLASS_LOG"
exit "$TEST_TRANSITION_EXIT"
`)
			writeTestFile(t, filepath.Join(candidate, "config/commands.registry"), 0o600, "fixture\n")
			writeTestFile(t, filepath.Join(candidate, "completions/yard.bash"), 0o600, "fixture\n")
			writeTestFile(t, filepath.Join(candidate, "config/installation-capabilities.json"), 0o600, `{"schemaVersion":1,"contract":"installation-inventory-v1"}`)
			writeTestFile(t, filepath.Join(candidate, "bin/yard-engine"), 0o700, `#!/bin/sh
set -eu
[ "$1" = _installation ] || exit 99
case "$2" in
  runtime) jq -e 'select(keys == ["VerifiedCacheInputs"] and (.VerifiedCacheInputs | length == 6))' > "$TEST_INSTALLER_LOG.cache" ;;
  full|inspect) cat >/dev/null ;;
  *) exit 2 ;;
esac
`)
			installer := filepath.Join(cache, "subyard-install-runtime-release.sh")
			installerPayload := `#!/bin/sh
set -eu
if [ "$3" = --publish-only ]; then
  printf 'publish-only\n' > "$TEST_INSTALLER_LOG"
  [ "$SUBYARD_INSTALL_RUNTIME_SCOPE" = private-draft ]
  mkdir "$2/releases"
  cp -a "$TEST_CANDIDATE" "$2/$TEST_RELEASE"
else
  printf 'initial\n' > "$TEST_INSTALLER_LOG"
  ln -s "$TEST_RELEASE" "$2/current"
fi
printf '%s\n' "$TEST_RELEASE"
`
			writeTestFile(t, installer, 0o700, installerPayload)
			writeTestFile(t, installer+".sha256", 0o600, fmt.Sprintf("%x\n", sha256.Sum256([]byte(installerPayload))))
			bootstrap := filepath.Join("..", "..", "dev", "bootstrap-runtime.sh")
			manifest := map[string]any{"schemaVersion": 2, "sourceRoot": source, "dataHome": requestData, "configHome": requestConfig}
			state := "schema=1\nphase=applying\nstep=entrypoint-switch\n"
			switch scenario {
			case "complete":
				state = "schema=1\nphase=complete\nstep=complete\n"
			case "wrong-data-home":
				manifest["dataHome"] = filepath.Join(home, "other-data")
			case "outside-home":
				manifest["sourceRoot"] = filepath.Dir(home)
			case "relative-root":
				manifest["sourceRoot"] = "source"
			}
			payload, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "invalid-json" {
				payload = []byte("invalid JSON")
			}
			manifestPath := filepath.Join(recovery, "source-install-manifest.json")
			if !sourceCheckout && !runtimeLinked {
				if scenario == "symlink-manifest" {
					ownedManifest := filepath.Join(home, "manifest.json")
					writeTestFile(t, ownedManifest, 0o600, string(payload))
					if err := os.MkdirAll(recovery, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(ownedManifest, manifestPath); err != nil {
						t.Fatal(err)
					}
				} else {
					writeTestFile(t, manifestPath, 0o600, string(payload))
				}
				writeTestFile(t, filepath.Join(recovery, "transaction"), 0o600, state)
			}
			for _, name := range []string{".bashrc", ".profile"} {
				writeTestFile(t, filepath.Join(home, name), 0o600, "# preserve startup file\n")
			}
			transitionExit := "23"
			if scenario == "entrypoint-switch-success" {
				transitionExit = "0"
			}

			snapshot := func() map[string]string { return bootstrapTreeSnapshot(t, home) }

			var before map[string]string
			if noRuntime {
				before = snapshot()
			}

			arguments := []string{bootstrap, "--version", "fixture-test", "--offline", "--yes"}
			if scenario == "runtime-trailing-slash" {
				arguments = append(arguments, "--runtime-root", runtimeRoot+"/")
			}
			command := exec.Command("bash", arguments...)
			command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + bootstrapHome, "SHELL=/bin/bash", "TMPDIR=" + testkit.TempDir(t),
				"SUBYARD_HOME=" + requestData, "SUBYARD_CONFIG_HOME=" + requestConfig, "YARD_BIN_DIR=" + requestBin,
				"YARD_SHELL_RC=" + filepath.Join(bootstrapHome, ".bashrc"), "YARD_LOGIN_RC=" + filepath.Join(bootstrapHome, ".profile"),
				"TEST_INGRESS_LOG=" + ingressLog, "TEST_INSTALLER_LOG=" + installerLog, "TEST_TRANSITION_EXIT=" + transitionExit, "TEST_CLASS_LOG=" + classLog,
				"TEST_CANDIDATE=" + candidate, "TEST_RELEASE=" + release}
			output, err := command.CombinedOutput()
			switch scenario {
			case "entrypoint-switch", "entrypoint-switch-success", "runtime-trailing-slash", "complete":
				wantExit := 23
				if scenario == "entrypoint-switch-success" {
					wantExit = 0
				}
				if (err != nil && exitStatus(err) != wantExit) || (err == nil && wantExit != 0) {
					t.Fatalf("bootstrap did not reach the transition recorder: %v\n%s", err, output)
				}
				if wantExit == 0 {
					var enrollment struct{ VerifiedCacheInputs []struct{ Path string } }
					if err := json.Unmarshal(readTestFile(t, installerLog+".cache"), &enrollment); err != nil ||
						len(enrollment.VerifiedCacheInputs) != 6 || enrollment.VerifiedCacheInputs[4].Path != installer || enrollment.VerifiedCacheInputs[5].Path != installer+".sha256" {
						t.Fatalf("verified cache enrollment = %#v, %v", enrollment, err)
					}
				}
				want := source + "\n"
				if scenario == "complete" || runtimeLinked {
					want = "unset\n"
				}
				if got := string(readTestFile(t, ingressLog)); got != want {
					t.Fatalf("transition source ingress = %q, want %q", got, want)
				}
				wantClass := "legacy\n"
				if scenario == "complete" || runtimeLinked {
					wantClass = "unset\n"
				}
				if got := string(readTestFile(t, classLog)); got != wantClass {
					t.Fatalf("candidate class = %q, want %q", got, wantClass)
				}
				wantPublication := "publish-only\n"
				if got := string(readTestFile(t, installerLog)); got != wantPublication {
					t.Fatalf("publication = %q, want %q", got, wantPublication)
				}
				if strings.Contains(string(output), "Sources before v0.11.0 are unsupported") {
					t.Fatalf("supported bootstrap reached the retired source refusal: %s", output)
				}
			default:
				want := "source recovery metadata is unsafe or invalid"
				if scenario == "entrypoint-switch-symlink-home" || scenario == "runtime-symlink-home" {
					want = "runtime root has a symlink or non-directory ancestor"
				}
				if noRuntime {
					want = "Sources before v0.11.0 are unsupported"
					if scenario == "native-checkout" {
						want = "bootstrap-runtime: entrypoints point at the checkout " + source + "; if this is an installation older than v0.11.0, see docs/reference/legacy-migration-support.md\n"
					}
				}
				if exitStatus(err) != 1 || !strings.Contains(string(output), want) {
					t.Fatalf("source recovery was not refused: %v\n%s", err, output)
				}
				for _, path := range []string{installerLog, ingressLog} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("refused recovery reached recorder %s: %v", path, err)
					}
				}
				if noRuntime {
					if _, err := os.Lstat(filepath.Join(runtimeRoot, "current")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("refused recovery published current: %v", err)
					}
				}
			}
			if noRuntime && !maps.Equal(before, snapshot()) {
				t.Fatal("source-only refusal changed the installation")
			}
			if !sourceCheckout && !runtimeLinked {
				if got := string(readTestFile(t, filepath.Join(recovery, "transaction"))); got != state {
					t.Fatalf("bootstrap changed recovery state: %q", got)
				}
			}
			for _, name := range []string{".bashrc", ".profile"} {
				if got := string(readTestFile(t, filepath.Join(home, name))); got != "# preserve startup file\n" {
					t.Fatalf("bootstrap changed startup file %s: %q", name, got)
				}
			}

			wantSy := filepath.Join(source, "bin/yard")
			if runtimeLinked {
				wantSy = filepath.Join(runtimeRoot, "current/bin/yard")
			}
			if target, err := os.Readlink(filepath.Join(bin, "sy")); err != nil || target != wantSy {
				t.Fatalf("bootstrap changed interrupted source link: target=%q err=%v", target, err)
			}
		})
	}
}

func exitStatus(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required by the production installer")
	}
}

func writeTestFile(t *testing.T, path string, mode os.FileMode, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(contents), mode)
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func bootstrapTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		var payload []byte
		var target string
		if info.Mode().IsRegular() {
			payload, err = os.ReadFile(path)
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err = os.Readlink(path)
		}
		if err != nil {
			return err
		}
		result[path] = fmt.Sprintf("%s:%d:%d:%x:%s", info.Mode(), info.Size(), info.ModTime().UnixNano(), sha256.Sum256(payload), target)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeBootstrapFixtureAssets(t *testing.T, cache string) string {
	t.Helper()
	const engine = "#!/bin/sh\n[ \"$1\" = _bootstrap-draft-capability ] || exit 2\nprintf 'private-release-draft-v1\\n'\n"
	var payload bytes.Buffer
	compressed := gzip.NewWriter(&payload)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "./bin/yard-engine", Mode: 0o755, Size: int64(len(engine))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(archive, engine); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(archive.Close(), compressed.Close()); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(payload.Bytes()))
	for _, arch := range []string{"amd64", "arm64"} {
		name := "subyard-fixture-test-linux-" + arch + ".tar.gz"
		bundle := filepath.Join(cache, name)
		writeTestFile(t, bundle, 0o600, payload.String())
		writeTestFile(t, bundle+".sha256", 0o600, digest+"\n")
		writeTestFile(t, bundle+".manifest.json", 0o600, fmt.Sprintf(`{"schemaVersion":1,"kind":"runtime","version":"fixture-test","os":"linux","arch":%q,"bootstrapDraft":"private-release-draft-v1"}`, arch))
		writeTestFile(t, bundle+".provenance.json", 0o600, fmt.Sprintf(`{"schemaVersion":1,"artifact":%q,"version":"fixture-test","sha256":%q,"sourceRepository":"github.com/Dmitry-Borodin/Subyard","sourceRevision":"fixture"}`, name, digest))
	}
	return digest
}

// Enumerate marker × target × recovery × current, repeated for each path layout.
// A release in the request runtime requires its current link: nine impossible
// combinations are omitted per layout (45 rows × five layouts = 225 cases).
func TestBootstrapSourceClassificationMatrix(t *testing.T) {
	requireJQ(t)
	const legacy = "Sources before v0.11.0 are unsupported. Stage verified current tooling with subyard-install.sh --version <version> --publish-only, then run the printed candidate's bin/yard uninstall --full. Full uninstall deletes this installation's owned data; review and confirm its removal plan. After verified cleanup, install a new version separately."
	for _, layout := range []string{"plain", "symlink-home", "symlink-runtime", "trailing-slash", "custom-request"} {
		for _, marker := range []string{"dispatcher", "native", "none"} {
			for _, target := range []string{"checkout", "request-runtime", "other-runtime"} {
				for _, recoveryState := range []string{"absent", "pending", "complete"} {
					for _, current := range []bool{false, true} {
						if target == "request-runtime" && !current {
							continue
						}
						name := fmt.Sprintf("%s/%s/%s/recovery=%s/current=%t", layout, marker, target, recoveryState, current)
						t.Run(name, func(t *testing.T) {
							fixture := testkit.TempDir(t)
							home := filepath.Join(fixture, "home")
							if err := os.Mkdir(home, 0o700); err != nil {
								t.Fatal(err)
							}
							logicalHome := home
							if layout == "symlink-home" {
								logicalHome = filepath.Join(fixture, "home-alias")
								if err := os.Symlink(home, logicalHome); err != nil {
									t.Fatal(err)
								}
							}
							data, bin := filepath.Join(home, "data"), filepath.Join(home, "bin")
							requestRoot := filepath.Join(data, "runtime")
							logicalRoot := filepath.Join(logicalHome, "data/runtime")
							if layout == "custom-request" {
								requestRoot = filepath.Join(home, "opt-runtime")
								logicalRoot = requestRoot
							}
							for _, path := range []string{data, bin, requestRoot} {
								if err := os.MkdirAll(path, 0o700); err != nil {
									t.Fatal(err)
								}
							}
							if layout == "symlink-runtime" {
								logicalRoot = filepath.Join(home, "runtime-alias")
								if err := os.Symlink(requestRoot, logicalRoot); err != nil {
									t.Fatal(err)
								}
							}
							if layout == "trailing-slash" {
								logicalRoot += "/"
							}
							if current {
								if err := os.Symlink("releases/0.28.1-fixture", filepath.Join(requestRoot, "current")); err != nil {
									t.Fatal(err)
								}
							}
							otherRoot := filepath.Join(home, "opt-runtime")
							if otherRoot == requestRoot {
								otherRoot = filepath.Join(home, "other-runtime")
							}
							source := filepath.Join(home, "checkout")
							if target == "request-runtime" {
								source = filepath.Join(requestRoot, "releases/0.28.1-fixture")
							}
							if target == "other-runtime" {
								source = filepath.Join(otherRoot, "releases/0.28.1-fixture")
								if err := os.MkdirAll(otherRoot, 0o700); err != nil {
									t.Fatal(err)
								}
								if err := os.Symlink("releases/0.28.1-fixture", filepath.Join(otherRoot, "current")); err != nil {
									t.Fatal(err)
								}
							}
							launcher := "#!/bin/sh\nexit 99\n"
							if marker == "dispatcher" {
								launcher = "#!/bin/sh\n# thin dispatcher over scripts/\nexit 99\n"
							}
							if marker == "native" {
								launcher = string(readTestFile(t, filepath.Join("..", "..", "bin", "yard")))
							}
							writeTestFile(t, filepath.Join(source, "bin/yard"), 0o700, launcher)
							writeTestFile(t, filepath.Join(source, "config/commands.registry"), 0o600, "fixture\n")
							writeTestFile(t, filepath.Join(source, "completions/yard.bash"), 0o600, "fixture\n")
							if target != "checkout" {
								writeTestFile(t, filepath.Join(source, "bin/yard-engine"), 0o700, "#!/bin/sh\nexit 99\n")
							}
							for _, name := range []string{"yard", "sy"} {
								if err := os.Symlink(filepath.Join(source, "bin/yard"), filepath.Join(bin, name)); err != nil {
									t.Fatal(err)
								}
							}
							recoverySource := filepath.Join(home, "recovery-source")
							if recoveryState != "absent" {
								if err := os.Mkdir(recoverySource, 0o700); err != nil {
									t.Fatal(err)
								}
								manifest, err := json.Marshal(map[string]any{"schemaVersion": 2, "sourceRoot": recoverySource, "dataHome": filepath.Join(logicalHome, "data"), "configHome": filepath.Join(logicalHome, "config")})
								if err != nil {
									t.Fatal(err)
								}
								writeTestFile(t, filepath.Join(data, "recovery/pre-go-source/source-install-manifest.json"), 0o600, string(manifest))
								state := "schema=1\nphase=applying\nstep=entrypoint-switch\n"
								if recoveryState == "complete" {
									state = "schema=1\nphase=complete\nstep=complete\n"
								}
								writeTestFile(t, filepath.Join(data, "recovery/pre-go-source/transaction"), 0o600, state)
							}
							writeBootstrapFixtureAssets(t, filepath.Join(data, "releases/fixture-test"))
							// The ordinary/recovery paths stop at their first mutating boundary.
							// Classification is captured through a pipe, outside the file tree.
							probe := filepath.Join(home, "probe.sh")
							writeTestFile(t, probe, 0o600, `detector_pid=$BASHPID
trap 'if [ "$BASHPID" = "$detector_pid" ]; then printf "%s\t%s\n" "${SOURCE_INGRESS_CLASS-none}" "${SOURCE_INGRESS_ROOT-}" >&3; fi' EXIT
install() { return 73; }
`)
							reader, writer, err := os.Pipe()
							if err != nil {
								t.Fatal(err)
							}
							defer reader.Close()
							defer writer.Close()
							args := []string{filepath.Join("..", "..", "dev", "bootstrap-runtime.sh"), "--version", "fixture-test", "--offline", "--yes"}
							if layout != "plain" && layout != "symlink-home" {
								args = append(args, "--runtime-root", logicalRoot)
							}
							command := exec.Command("bash", args...)
							command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + logicalHome, "SHELL=/bin/bash", "TMPDIR=" + testkit.TempDir(t), "BASH_ENV=" + probe, "SUBYARD_HOME=" + filepath.Join(logicalHome, "data"), "SUBYARD_CONFIG_HOME=" + filepath.Join(logicalHome, "config"), "YARD_BIN_DIR=" + filepath.Join(logicalHome, "bin"), "YARD_SHELL_RC=" + filepath.Join(logicalHome, ".bashrc"), "YARD_LOGIN_RC=" + filepath.Join(logicalHome, ".profile")}
							command.ExtraFiles = []*os.File{writer}
							var stdout, stderr strings.Builder
							command.Stdout, command.Stderr = &stdout, &stderr
							before := bootstrapTreeSnapshot(t, fixture)
							err = command.Run()
							writer.Close()
							observed, readErr := io.ReadAll(reader)
							if readErr != nil {
								t.Fatal(readErr)
							}
							wantClass, wantRoot, wantMessage, wantCode := "none", "", "", 73
							switch {
							case target == "other-runtime":
								wantClass, wantCode = "runtime", 1
								wantMessage = fmt.Sprintf("bootstrap-runtime: entrypoints belong to runtime %s; re-run with --runtime-root %s\n", otherRoot, otherRoot)
							case recoveryState == "pending":
								wantClass, wantRoot = "legacy", recoverySource
							case target == "request-runtime":
							case marker == "dispatcher":
								wantClass, wantRoot = "legacy", source
							case marker == "native":
								wantClass, wantCode = "checkout", 1
								wantMessage = fmt.Sprintf("bootstrap-runtime: entrypoints point at the checkout %s; if this is an installation older than v0.11.0, see docs/reference/legacy-migration-support.md\n", source)
							}
							if wantClass == "legacy" && !current {
								wantMessage, wantCode = "bootstrap-runtime: "+legacy+"\n", 1
							}
							if layout == "symlink-home" || layout == "symlink-runtime" {
								unsafe := logicalRoot
								if layout == "symlink-home" {
									unsafe = logicalHome
								}
								wantClass, wantRoot, wantCode = "none", "", 1
								wantMessage = fmt.Sprintf("bootstrap-runtime: runtime root has a symlink or non-directory ancestor: '%s'\n", unsafe)
							}
							if exitStatus(err) != wantCode || stderr.String() != wantMessage || string(observed) != wantClass+"\t"+wantRoot+"\n" {
								t.Fatalf("class=%q code=%d message=%q; want class=%q root=%q code=%d message=%q; stdout=%s", observed, exitStatus(err), stderr.String(), wantClass, wantRoot, wantCode, wantMessage, stdout.String())
							}
							if !maps.Equal(before, bootstrapTreeSnapshot(t, fixture)) {
								t.Fatal("classification wrote to the installation")
							}
						})
					}
				}
			}
		}
	}
}
