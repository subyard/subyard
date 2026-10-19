//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestReleaseOwnerDeclarationIsClosedBeforeExecution(t *testing.T) {
	for _, scenario := range []string{
		"valid", "legacy", "malformed", "unknown", "duplicate", "trailing", "missing-field", "case-substitution",
		"unknown-version", "unknown-protocol", "unknown-kind", "owner-substitution", "helper-substitution", "schema-substitution",
		"missing-owner", "missing-schema", "missing-build", "missing-declaration", "unlisted-declaration", "unlisted-owner",
		"schema-tamper", "schema-duplicate", "schema-unknown", "build-tamper", "build-version", "fixture-features",
		"build-unknown", "build-duplicate", "build-image", "build-lock", "build-source", "non-executable-owner",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			capture := filepath.Join(root, "engine-invoked")
			candidate := writeReleaseOwnerCandidate(t, root, fmt.Sprintf("#!/bin/sh\n: > %q\nprintf 'yard-engine 1.2.3\\n'\n", capture))
			change := func(path, old, next string) {
				payload, err := os.ReadFile(filepath.Join(candidate.root, path))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(payload, []byte(old)) {
					t.Fatalf("fixture does not contain %q", old)
				}
				testkit.WriteFile(t, filepath.Join(candidate.root, path), bytes.Replace(payload, []byte(old), []byte(next), 1), 0o600)
			}
			remove := func(path string) {
				if err := os.Remove(filepath.Join(candidate.root, path)); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "legacy":
				for _, path := range []string{releaseOwnerDeclarationPath, releaseOwnerSchemaPath, releaseOwnerBuildPath, releaseOwnerImagePath} {
					remove(path)
				}
			case "malformed":
				testkit.WriteFile(t, filepath.Join(candidate.root, releaseOwnerDeclarationPath), []byte("{"), 0o600)
			case "unknown":
				change(releaseOwnerDeclarationPath, "{", `{"other":1,`)
			case "duplicate":
				change(releaseOwnerDeclarationPath, "{", `{"schemaVersion":1,`)
			case "trailing":
				change(releaseOwnerDeclarationPath, "}", "} {}")
			case "missing-field":
				change(releaseOwnerDeclarationPath, `"schemaVersion":1,`, "")
			case "case-substitution":
				change(releaseOwnerDeclarationPath, "schemaVersion", "SchemaVersion")
			case "unknown-version":
				change(releaseOwnerDeclarationPath, `"schemaVersion":1`, `"schemaVersion":2`)
			case "unknown-protocol":
				change(releaseOwnerDeclarationPath, `"protocolVersion":1`, `"protocolVersion":2`)
			case "unknown-kind":
				change(releaseOwnerDeclarationPath, "rust-release-owner", "go-release-owner")
			case "owner-substitution":
				change(releaseOwnerDeclarationPath, releaseOwnerImagePath, "bin/yard-engine")
			case "helper-substitution":
				change(releaseOwnerDeclarationPath, "bin/yard-engine", releaseOwnerImagePath)
			case "schema-substitution":
				change(releaseOwnerDeclarationPath, releaseOwnerSchemaPath, "config/migrations.json")
			case "missing-owner":
				remove(releaseOwnerImagePath)
			case "missing-schema":
				remove(releaseOwnerSchemaPath)
			case "missing-build":
				remove(releaseOwnerBuildPath)
			case "missing-declaration":
				remove(releaseOwnerDeclarationPath)
			case "schema-tamper":
				change(releaseOwnerSchemaPath, `"additionalProperties":false`, `"additionalProperties":true`)
			case "schema-duplicate":
				change(releaseOwnerSchemaPath, `"const":1`, `"const":1,"const":1`)
			case "schema-unknown":
				change(releaseOwnerSchemaPath, "{", `{"other":1,`)
			case "build-version":
				change(releaseOwnerBuildPath, `"schemaVersion":1`, `"schemaVersion":2`)
			case "fixture-features":
				change(releaseOwnerBuildPath, `"features":[]`, `"features":["fixture-cuts"]`)
			case "build-unknown":
				change(releaseOwnerBuildPath, "{", `{"other":1,`)
			case "build-duplicate":
				change(releaseOwnerBuildPath, `"path":"release-owner/Cargo.lock"`, `"path":"release-owner/Cargo.lock","path":"release-owner/Cargo.lock"`)
			case "build-image":
				change(releaseOwnerBuildPath, `"imageSHA256":"`, `"imageSHA256":"0`)
			case "build-lock":
				change(releaseOwnerBuildPath, `"cargoLockSHA256":"`, `"cargoLockSHA256":"0`)
			case "build-source":
				change(releaseOwnerBuildPath, "rust-toolchain.toml", "other-toolchain.toml")
			case "non-executable-owner":
				if err := os.Chmod(filepath.Join(candidate.root, releaseOwnerImagePath), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rewriteReleaseOwnerManifest(t, candidate)
			if scenario == "build-tamper" {
				change(releaseOwnerBuildPath, "rustc", "wrong")
			}
			if strings.HasPrefix(scenario, "unlisted-") {
				path := releaseOwnerDeclarationPath
				if scenario == "unlisted-owner" {
					path = releaseOwnerImagePath
				}
				manifest, err := os.ReadFile(filepath.Join(candidate.root, "runtime-files.sha256"))
				if err != nil {
					t.Fatal(err)
				}
				var listed []string
				for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
					if !strings.HasSuffix(line, "  ./"+path) {
						listed = append(listed, line)
					}
				}
				testkit.WriteFile(t, filepath.Join(candidate.root, "runtime-files.sha256"), []byte(strings.Join(listed, "\n")+"\n"), 0o600)
			}
			runtime := New(Config{Stderr: &bytes.Buffer{}})
			defer runtime.Close()
			verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
			if scenario == "valid" || scenario == "legacy" {
				if err != nil {
					t.Fatal(err)
				}
				defer verified.Close()
				if (verified.releaseOwner != nil) != (scenario == "valid") {
					t.Fatal("release owner image does not match the declaration")
				}
			} else {
				if err == nil || verified != nil {
					t.Fatalf("invalid owner candidate accepted: %#v %v", verified, err)
				}
				if _, err := os.Stat(capture); !os.IsNotExist(err) {
					t.Fatalf("invalid owner assets executed candidate: %v", err)
				}
			}
		})
	}
}

func TestReleaseOwnerAndHelperImagesSurviveReplacementAndClose(t *testing.T) {
	root := testkit.TempDir(t)
	candidate := writeReleaseOwnerCandidate(t, root, "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
	runtime := New(Config{})
	defer runtime.Close()
	verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path, output string
		image        *os.File
	}{{releaseOwnerImagePath, "sealed-owner\n", verified.releaseOwner}, {"bin/yard-engine", "yard-engine 1.2.3\n", verified.engine}} {
		// Replace the pathname and modify the original inode: neither may affect the image.
		testkit.WriteFile(t, filepath.Join(candidate.root, item.path), []byte("#!/bin/sh\nexit 93\n"), 0o700)
		if err := os.Remove(filepath.Join(candidate.root, item.path)); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(candidate.root, item.path), []byte("#!/bin/sh\nexit 94\n"), 0o700)
		seals, err := unix.FcntlInt(item.image.Fd(), unix.F_GET_SEALS, 0)
		want := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
		if err != nil || seals&want != want {
			t.Fatalf("execution image seals=%x err=%v", seals, err)
		}
		command := exec.Command("/proc/self/fd/3")
		command.ExtraFiles = []*os.File{item.image}
		output, err := command.Output()
		if err != nil || string(output) != item.output {
			t.Fatalf("sealed %s output=%q err=%v", item.path, output, err)
		}
	}
	owner, helper := verified.releaseOwner, verified.engine
	if err := verified.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []*os.File{owner, helper} {
		if _, err := file.Stat(); err == nil {
			t.Fatal("candidate close retained an execution image")
		}
	}
}

func TestReleaseOwnerBuildMetadataBindsWholeArtifactManifest(t *testing.T) {
	root := testkit.TempDir(t)
	capture := filepath.Join(root, "engine-invoked")
	candidate := writeReleaseOwnerCandidate(t, root, fmt.Sprintf("#!/bin/sh\n: > %q\nprintf 'yard-engine 1.2.3\\n'\n", capture))
	runtime := New(Config{})
	defer runtime.Close()
	verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(candidate.root, "runtime-files.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	want := releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest)))
	if verified.manifestDigest != want {
		t.Fatal("artifact digest does not bind the complete runtime inventory")
	}
	if err := verified.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(capture); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(candidate.root, releaseOwnerBuildPath)
	metadata, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, bytes.Replace(metadata, []byte("cargo 1.99.1"), []byte("cargo 1.99.2"), 1), 0o600)
	rewriteReleaseOwnerManifest(t, candidate)
	if verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, &want); err == nil || verified != nil {
		t.Fatal("changed build inputs retained the protected artifact identity")
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("changed protected candidate executed before artifact verification")
	}
}

// Opt-in process proof of the actual Go-sealed helper crossing into the Rust owner.
// Ordinary Go tests need neither a compiler nor a Rust executable.
func TestReleaseOwnerNormalSealedMissingProofRefusal(t *testing.T) {
	ownerPath, helperPath := os.Getenv("SUBYARD_RUST_OWNER_EXECUTABLE"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER")
	corpusPath := os.Getenv("SUBYARD_RUST_RUNTIME_CORPUS")
	if ownerPath == "" || helperPath == "" || corpusPath == "" {
		t.Skip("opt-in actual sealed Rust owner/helper process")
	}
	seal := func(path string) *os.File {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		image, err := sealVerifiedEngine(file, sha256.Sum256(payload))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { image.Close() })
		return image
	}
	owner, helper := seal(ownerPath), seal(helperPath)
	var corpus struct {
		Registry string          `json:"registry"`
		Catalog  json.RawMessage `json:"catalog"`
	}
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	for _, relative := range []string{"config", "scripts", "state", "data", "home", "runtime/releases/old", "runtime/releases/new"} {
		if err := os.MkdirAll(filepath.Join(root, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := os.ReadFile("../../../config/commands.registry")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/commands.registry"), registry, 0o600)
	if err := os.Symlink("releases/old", filepath.Join(root, "runtime/current")); err != nil {
		t.Fatal(err)
	}
	pair := func() (*os.File, *os.File) {
		descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		left, right := os.NewFile(uintptr(descriptors[0]), "delivery-client"), os.NewFile(uintptr(descriptors[1]), "delivery-child")
		t.Cleanup(func() { left.Close(); right.Close() })
		return left, right
	}
	client, child := pair()
	_, custody := pair()
	grantRead, grantWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer grantRead.Close()
	if _, err := grantWrite.WriteString("delivery-fixture-private-consent"); err != nil {
		t.Fatal(err)
	}
	if err := grantWrite.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/proc/self/fd/3", "_release-owner-operation-v1")
	command.ExtraFiles = []*os.File{owner}
	command.Env = []string{"HOME=" + filepath.Join(root, "home"), "PATH=/nonexistent", "SUBYARD_NO_AUDIT=1"}
	command.Stdin, command.Stdout = child, child
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { command.Process.Kill(); command.Wait() }()
	child.Close()
	repository, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := unix.Sendmsg(int(client.Fd()), []byte{'C'}, unix.UnixRights(int(helper.Fd()), int(grantRead.Fd()), int(custody.Fd()), int(repository.Fd())), nil, unix.MSG_NOSIGNAL); err != nil {
		t.Fatal(err)
	}
	options := map[string]any{
		"dataHome": filepath.Join(root, "data"), "configHome": filepath.Join(root, "state"),
		"runtimeRoot": filepath.Join(root, "runtime"), "repositoryRoot": root, "yard": "default",
		"releases": map[string]string{"from": "old", "target": "new"}, "artifactDigest": strings.Repeat("a", 64),
		"registryRaw": []byte(corpus.Registry), "catalog": corpus.Catalog, "inheritedSettingIds": []string{},
		"rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil,
	}
	if err := json.NewEncoder(client).Encode(map[string]any{"op": "open", "options": options}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Custody struct {
			Session string `json:"session"`
		} `json:"custody"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatalf("sealed owner Open response: %v (%s)", err, stderr.String())
	}
	if response.Error == "" || response.Custody.Session != "" {
		t.Fatalf("normal owner admitted missing authenticated artifact proofs: %#v (%s)", response, stderr.String())
	}
	client.Close()
	_ = command.Wait()
	if _, err := os.Lstat(filepath.Join(root, "state/migrations/update.lock")); !os.IsNotExist(err) {
		t.Fatal("read-only Open created an update lease")
	}
}

func TestReleaseOwnerRealPublishedCandidate(t *testing.T) {
	root := os.Getenv("SUBYARD_RUST_DELIVERY_RUNTIME_ROOT")
	if root == "" {
		t.Skip("opt-in actual installed delivery candidate")
	}
	current, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := publishedCandidate{root: filepath.Join(root, current), release: releasetransition.ReleaseID(filepath.Base(current))}
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if verified.engine == nil || verified.releaseOwner == nil || verified.manifestDigest == "" {
		t.Fatal("actual package did not produce both protected execution images")
	}
}

func writeReleaseOwnerCandidate(t *testing.T, root, engine string) publishedCandidate {
	t.Helper()
	candidate := writeRuntimeCandidatePayload(t, root, engine)
	owner := []byte("#!/bin/sh\nprintf 'sealed-owner\\n'\n")
	testkit.WriteFile(t, filepath.Join(candidate.root, releaseOwnerImagePath), owner, 0o700)
	for _, path := range []string{releaseOwnerDeclarationPath, releaseOwnerSchemaPath} {
		payload, err := os.ReadFile(filepath.Join("..", "..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(candidate.root, path), payload, 0o600)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(nil))
	inputs := []map[string]string{}
	for _, path := range []string{"dev/build-release-owner.sh", "release-owner/Cargo.lock", "release-owner/Cargo.toml",
		"release-owner/src/bin/subyard-release-owner.rs", "release-owner/src/lib.rs", "rust-toolchain.toml"} {
		inputs = append(inputs, map[string]string{"path": path, "sha256": hash})
	}
	build, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "kind": "rust-release-owner-build", "compiler": "rustc 1.99.1\nhost: x86_64-unknown-linux-gnu",
		"cargo": "cargo 1.99.1", "target": "x86_64-unknown-linux-musl", "profile": "release", "features": []string{},
		"locked": true, "staticELF": true, "imageSHA256": fmt.Sprintf("%x", sha256.Sum256(owner)), "cargoLockSHA256": hash,
		"codegen": []string{"linker=rust-lld", "target-feature=+crt-static", "remap-path-prefix=repository=."}, "inputs": inputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(candidate.root, releaseOwnerBuildPath), build, 0o600)
	rewriteReleaseOwnerManifest(t, candidate)
	return candidate
}

func rewriteReleaseOwnerManifest(t *testing.T, candidate publishedCandidate) {
	t.Helper()
	var manifest strings.Builder
	err := filepath.WalkDir(candidate.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path == filepath.Join(candidate.root, "runtime-files.sha256") {
			return err
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(candidate.root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(payload), relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(candidate.root, "runtime-files.sha256"), []byte(manifest.String()), 0o600)
}

// The Go release must work without Rust tooling and must never reinterpret an
// explicitly Rust-owned artifact through the Go metadata policy.
func TestGoSourceAdmissionDoesNotAdoptExperimentalOwner(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(fmt.Sprint(declared), func(t *testing.T) {
			root, home := testkit.TempDir(t), testkit.TempDir(t)
			candidate := writeReleaseOwnerCandidate(t, root, "#!/bin/sh\n[ \"$1\" = --version ] || exit 99\nprintf 'yard-engine 1.2.3\\n'\n")
			next := filepath.Join(root, "releases", "1.2.3-owner")
			if err := os.Rename(candidate.root, next); err != nil {
				t.Fatal(err)
			}
			candidate.root = next
			candidate.release = "1.2.3-owner"
			testkit.WriteFile(t, filepath.Join(next, "config/release-transition.json"), []byte(sourceAdmissionRegistry), 0600)
			if !declared {
				for _, name := range []string{releaseOwnerDeclarationPath, releaseOwnerSchemaPath, releaseOwnerBuildPath, releaseOwnerImagePath} {
					if err := os.Remove(filepath.Join(next, name)); err != nil {
						t.Fatal(err)
					}
				}
			}
			rewriteReleaseOwnerManifest(t, candidate)
			if err := os.Symlink("releases/"+string(candidate.release), filepath.Join(root, "current")); err != nil {
				t.Fatal(err)
			}
			runtime := New(Config{RepositoryRoot: filepath.Join(root, "no-rust-tooling")})
			defer runtime.Close()
			proof, err := runtime.InspectSourceAdmission(context.Background(), root, home)
			if declared {
				if err == nil || proof.Binding != "" || !strings.Contains(fmt.Sprint(errors.Unwrap(err)), "experimental Rust owner") {
					t.Fatal("Go adopted Rust ownership", proof, err)
				}
			} else if err != nil || !lowerSHA256(proof.Binding) {
				t.Fatal("Go metadata required Rust tooling", proof, err)
			}
		})
	}
}

func TestGoTransitionsRefuseExperimentalOwnerBeforeInvocation(t *testing.T) {
	root := testkit.TempDir(t)
	marker := filepath.Join(root, "effect")
	candidate := writeReleaseOwnerCandidate(t, root, fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'yard-engine 1.2.3\\n'; else : > %q; exit 99; fi\n", marker))
	runtime := New(Config{})
	defer runtime.Close()
	verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	goRoot := testkit.TempDir(t)
	goCandidate := writeDelegateRuntimeFixture(t, goRoot, "1.2.3-go", "Go owner", []byte(sourceAdmissionRegistry), "#!/bin/sh\n[ \"$1\" = --version ] || exit 99\nprintf 'yard-engine 1.2.3\\n'\n")
	goOwner, err := runtime.verifyPublishedCandidate(context.Background(), goCandidate, goRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer goOwner.Close()
	for _, pair := range [][2]*verifiedPublishedCandidate{{goOwner, verified}, {verified, goOwner}} {
		_, err := runtime.prepareVerifiedTransition(context.Background(), options{}, pair[0], pair[1], releasetransition.ProcessRequest{}, nil)
		if err == nil || (!strings.Contains(err.Error(), "experimental Rust release ownership is disabled") || allowsCompletedMetadataAdmission(nil, err)) {
			t.Fatal("Go preparation adopted Rust target/owner", err)
		}
	}
	for _, mode := range []releasetransition.ProcessMode{releasetransition.ProcessInspect, releasetransition.ProcessConverge} {
		_, err := runtime.invokeVerifiedCandidateTransition(context.Background(), verified, releasetransition.ProcessRequest{Mode: mode}, "unconsumed-grant")
		if err == nil || (!strings.Contains(err.Error(), "experimental Rust release ownership is disabled") || allowsCompletedMetadataAdmission(nil, err)) {
			t.Fatal("ordinary Go dispatch adopted Rust", mode, err)
		}
	}
	_, err = runtime.invokeVerifiedRecoveryTransition(context.Background(), verified, root, releasetransition.RecoveryProcessRequest{Mode: releasetransition.RecoveryProcessCapabilities}, "")
	if err == nil || (!strings.Contains(err.Error(), "experimental Rust release ownership is disabled") || allowsCompletedMetadataAdmission(nil, err)) {
		t.Fatal("Go recovery adopted Rust", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("held owner executed before refusal", err)
	}
	if _, err := os.Stat(filepath.Join(root, "migrations/update.lock")); !os.IsNotExist(err) {
		t.Fatal("refusal acquired update lock", err)
	}
}
