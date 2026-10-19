//go:build linux

package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

// The Rust image deliberately uses compile-time cuts and omits artifact tuples.
// This proves coexistence on the actual update.lock, not production admission.
func TestRustOwnerReleasedWriterExclusion(t *testing.T) {
	owner, helper, corpusPath := os.Getenv("SUBYARD_RUST_FIXTURE_OWNER"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER"), os.Getenv("SUBYARD_RUST_RUNTIME_CORPUS")
	if owner == "" || helper == "" || corpusPath == "" {
		t.Skip("opt-in fixture-cut Rust owner and authentic released Go writer")
	}
	var corpus struct {
		Registry string                                   `json:"registry"`
		Catalog  []releasetransition.CapabilityDescriptor `json:"catalog"`
	}
	raw, err := os.ReadFile(corpusPath)
	if err != nil || json.Unmarshal(raw, &corpus) != nil {
		t.Fatal("runtime corpus unavailable", err)
	}
	for _, cut := range []string{"after-prepared-bundle", "after-authorized-journal"} {
		t.Run(cut, func(t *testing.T) {
			repository, environment, _ := nativeFixture(t)
			writeCLIFile(t, filepath.Join(repository, "config/projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0o644)
			root, err := os.MkdirTemp("/tmp", "subyard-writer-exclusion.")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					t.Log("retained controlled exclusion fixture:", root)
					return
				}
				_ = os.RemoveAll(root)
			})
			var terminals []map[string]any
			persistTerminal := func(phase string, state *os.ProcessState) {
				t.Helper()
				terminals = append(terminals, map[string]any{"phase": phase, "exitCode": state.ExitCode(), "success": state.Success(), "state": state.String()})
				if evidence := os.Getenv("SUBYARD_RUST_DELIVERY_EVIDENCE_DIR"); evidence != "" {
					body, _ := json.MarshalIndent(terminals, "", "  ")
					testkit.WriteFile(t, filepath.Join(evidence, cut+"-child-native.json"), append(body, '\n'), 0o600)
				}
			}
			runtimeRoot, configHome, dataHome := filepath.Join(root, "runtime"), filepath.Join(root, "config"), filepath.Join(root, "data")
			for _, path := range []string{runtimeRoot, configHome, dataHome, filepath.Join(configHome, ".sync/settings"), filepath.Join(runtimeRoot, "releases/new")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			sourceID, targetID := "0.11.2-authentic-source", "0.18.1-authentic-go"
			extractFacadeHistoricalRuntime(t, "0.11.2", filepath.Join(runtimeRoot, "releases", sourceID))
			target := filepath.Join(runtimeRoot, "releases", targetID)
			extractFacadeHistoricalRuntime(t, "0.18.1", target)
			if err := os.Symlink("releases/"+sourceID, filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(configHome, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0o600)
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			environment = append(environment, "SUBYARD_HOME="+dataHome, "SUBYARD_CONFIG_HOME="+configHome, "YARD_RUNTIME_ROOT="+runtimeRoot,
				"SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
			goEnvironment := append(append([]string{}, environment...), "SUBYARD_REPOSITORY_ROOT="+target)
			manifest, err := os.ReadFile(filepath.Join(target, "runtime-files.sha256"))
			if err != nil {
				t.Fatal(err)
			}
			registry, err := os.ReadFile(filepath.Join(target, "config/release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome, Yard: "default", Target: releasetransition.ReleaseID(targetID), Direction: releasetransition.DirectionActivateTarget,
				ArtifactDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest))), RegistryDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(registry)))}
			payload, _ := json.Marshal(request)
			inspect := exec.Command(filepath.Join(target, "bin/yard-engine"), "_release-transition")
			inspect.Env, inspect.Stdin = goEnvironment, bytes.NewReader(payload)
			var inspectedOutput, inspectedError bytes.Buffer
			inspect.Stdout, inspect.Stderr = &inspectedOutput, &inspectedError
			if err := inspect.Run(); err != nil {
				t.Fatal("authentic released inspection", err, inspectedError.String())
			}
			persistTerminal("authentic-go-inspect", inspect.ProcessState)
			var response releasetransition.ProcessResponse
			if err := json.Unmarshal(inspectedOutput.Bytes(), &response); err != nil || response.Inspection == nil || len(response.Inspection.Blockers) != 0 {
				t.Fatal("authentic released preparation", response, err)
			}
			process := startRustOwnerWithBudget(t, owner, helper, environment, repository, true, 60*time.Second)
			defer process.close()
			options := map[string]any{"dataHome": dataHome, "configHome": configHome, "runtimeRoot": runtimeRoot, "repositoryRoot": repository, "yard": "default", "releases": map[string]string{"from": sourceID, "target": "new"}, "artifactDigest": strings.Repeat("a", 64), "registryRaw": []byte(corpus.Registry), "catalog": corpus.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil}
			process.exchange(map[string]any{"op": "open", "options": options})
			prepared := process.exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": "new", "direction": "activate-target"}}).Inspection
			if prepared == nil || len(prepared.Blockers) != 0 {
				t.Fatal("Rust fixture preparation", prepared)
			}
			if err := process.encoder.Encode(map[string]any{"op": "execute", "plan": prepared.Plan, "authorization": "fixture-private-consent"}); err != nil {
				t.Fatal(err)
			}
			process.admit(prepared.Plan, "")
			control := bufio.NewReader(process.control)
			readCut := func(want string) {
				t.Helper()
				for n := 0; n < 128; n++ {
					_ = process.control.SetReadDeadline(time.Now().Add(5 * time.Second))
					phase, err := control.ReadString('\n')
					if err != nil {
						var reply rustOwnerReply
						decodeErr := process.decoder.Decode(&reply)
						t.Fatal("Rust cut not reached", want, err, "reply", reply, decodeErr)
					}
					if strings.TrimSpace(phase) == want {
						return
					}
					if _, err := process.control.Write([]byte{'R'}); err != nil {
						t.Fatal(err)
					}
				}
				t.Fatal("Rust cut bound")
			}
			readCut("after-prepared-bundle")
			request.Mode, request.Execution = releasetransition.ProcessConverge, &releasetransition.Execution{Plan: response.Inspection.Plan, Authorization: "released-writer-real-private-grant"}
			payload, _ = json.Marshal(request)
			grant, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer grant.Close()
			_, _ = io.WriteString(write, "released-writer-real-private-grant\n")
			write.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			writer := exec.CommandContext(ctx, filepath.Join(target, "bin/yard-engine"), "_release-transition")
			writer.Env, writer.Stdin, writer.ExtraFiles = append(goEnvironment, "SUBYARD_RELEASE_TRANSITION_GRANT_FD=3"), bytes.NewReader(payload), []*os.File{grant}
			var stdout, stderr bytes.Buffer
			writer.Stdout, writer.Stderr = &stdout, &stderr
			if err := writer.Start(); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- writer.Wait() }()
			defer writer.Process.Kill()
			start := facadeProcessStart(t, writer.Process.Pid)
			var lock unix.Stat_t
			if err := unix.Stat(filepath.Join(configHome, "migrations/update.lock"), &lock); err != nil {
				t.Fatal(err)
			}
			identity := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(lock.Dev)), unix.Minor(uint64(lock.Dev)), lock.Ino)
			end := time.Now().Add(3 * time.Second)
			for {
				locks, err := os.ReadFile("/proc/locks")
				if err != nil {
					t.Fatal(err)
				}
				waiting := false
				for _, line := range strings.Split(string(locks), "\n") {
					fields := strings.Fields(line)
					if len(fields) >= 8 && fields[1] == "->" && fields[2] == "FLOCK" && fields[4] == "WRITE" && fields[5] == strconv.Itoa(writer.Process.Pid) && fields[6] == identity {
						waiting = true
					}
				}
				if waiting {
					break
				}
				select {
				case err := <-finished:
					t.Fatal("released writer did not wait on Rust update lease", err, stderr.String())
				default:
				}
				if time.Now().After(end) || facadeProcessStart(t, writer.Process.Pid) != start {
					t.Fatal("released writer wait identity was not proven")
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Log("authentic released writer blocked on exact update.lock", writer.Process.Pid, start, identity)
			journal := filepath.Join(configHome, "release-transition/v2/journal.json")
			var protected []byte
			var protectedFiles map[string]string
			if cut == "after-authorized-journal" {
				_, _ = process.control.Write([]byte{'R'})
				readCut(cut)
				protected, err = os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
				protectedFiles = facadeProtectedFiles(t, configHome)
			} else if _, err := os.Stat(journal); !os.IsNotExist(err) {
				t.Fatal("before-CAS cut already published history", err)
			}
			_ = process.cmd.Process.Kill()
			if err := process.cmd.Wait(); err == nil {
				t.Fatal("Rust cut process was not interrupted")
			}
			persistTerminal("rust-cut", process.cmd.ProcessState)
			result := <-finished
			persistTerminal("authentic-go-converge", writer.ProcessState)
			var outcome releasetransition.ProcessResponse
			parseErr := json.Unmarshal(stdout.Bytes(), &outcome)
			if outcome.Outcome != nil {
				t.Log("released writer outcome", outcome.Outcome.Status, outcome.Outcome.Code)
			}
			if cut == "after-authorized-journal" {
				fresh, err := os.ReadFile(journal)
				if err != nil || !bytes.Equal(protected, fresh) || !maps.Equal(protectedFiles, facadeProtectedFiles(t, configHome)) || outcome.Outcome != nil && outcome.Outcome.ReachedGoal {
					t.Fatal("released writer appropriated Rust pending history", result, parseErr)
				}
				current, _ := os.Readlink(filepath.Join(runtimeRoot, "current"))
				settings, _ := os.ReadFile(filepath.Join(configHome, ".sync/settings/config.env"))
				if current != "releases/"+sourceID || string(settings) != "SSH_PORT=2240\n" {
					t.Fatal("released writer changed protected state after pending publication")
				}
			} else if result != nil || parseErr != nil || outcome.Outcome == nil || !outcome.Outcome.ReachedGoal {
				t.Fatal("released writer could not proceed after pre-journal Rust death", result, parseErr, outcome, stderr.String())
			}
		})
	}
}

func facadeProtectedFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	values := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(root, path)
		if err != nil || name == "migrations/update.lock" {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected protected fixture file kind")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			return err
		}
		values[name] = fmt.Sprintf("%x:%o:%d:%d:%d", sha256.Sum256(body), info.Mode(), stat.Ino, stat.Uid, stat.Nlink)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func facadeProcessStart(t *testing.T, pid int) string {
	t.Helper()
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(body)[strings.LastIndexByte(string(body), ')')+1:])
	if len(fields) < 20 {
		t.Fatal("process identity unavailable")
	}
	return fields[19]
}

func extractFacadeHistoricalRuntime(t *testing.T, version, destination string) {
	t.Helper()
	root := repositoryRoot(t)
	var index struct {
		Assets []struct {
			Version, Directory, BundleSHA256 string
		}
	}
	input, err := os.ReadFile(filepath.Join(root, ".build/rust-p1-predecessors/attempt-1/index.json"))
	if err != nil || json.Unmarshal(input, &index) != nil {
		t.Fatal("authenticated historical cache index unavailable", err)
	}
	for _, asset := range index.Assets {
		if asset.Version != version {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, asset.Directory, "subyard-"+version+"-linux-amd64.tar.gz"))
		if err != nil || hex.EncodeToString(sha256SumFacade(body)) != asset.BundleSHA256 {
			t.Fatal("authenticated historical bundle mismatch", version, err)
		}
		compressed, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer compressed.Close()
		archive := tar.NewReader(compressed)
		for {
			entry, err := archive.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Clean(entry.Name)
			if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
				t.Fatal("historical archive path escaped fixture")
			}
			path = filepath.Join(destination, path)
			switch entry.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case tar.TypeReg:
				body, err := io.ReadAll(archive)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, body, os.FileMode(entry.Mode))
			default:
				t.Fatal("historical archive has unsupported fixture entry")
			}
		}
	}
	t.Fatal("historical version absent from authenticated cache", version)
}

func sha256SumFacade(raw []byte) []byte {
	digest := sha256.Sum256(raw)
	return digest[:]
}
