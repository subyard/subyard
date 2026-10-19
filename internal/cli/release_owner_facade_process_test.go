//go:build linux

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// This executable is a synthetic facade fixture, never a historical owner or
// shipped helper. Its exact bytes are manifest-bound, preserving the real
// /proc/self/exe authentication at the staged frozen ingress.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--version":
			fmt.Println("yard-engine 1.0.0-facade-fixture")
			return
		case "_release-owner-native-v1":
			if err := RunReleaseOwnerNative(context.Background(), os.Stdin, os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "fixture native session refused")
				os.Exit(1)
			}
			return
		case "_release-transition":
			program, err := New(Options{RepositoryRoot: os.Getenv("SUBYARD_FACADE_FIXTURE_ROOT"), Environment: os.Environ(), Stdout: os.Stdout, Stderr: os.Stderr})
			if err == nil {
				var payload []byte
				payload, err = io.ReadAll(io.LimitReader(os.Stdin, releaseTransitionRequestLimit+1))
				if err == nil && len(payload) <= releaseTransitionRequestLimit {
					err = program.invokeRustReleaseTransitionFacade(context.Background(), payload, os.Stdout)
				}
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}
	os.Exit(m.Run())
}

func TestRustOwnerFrozenFacadeProcess(t *testing.T) {
	installed := os.Getenv("SUBYARD_RUST_DELIVERY_RUNTIME_ROOT")
	if installed == "" {
		t.Skip("opt-in actual normal Rust image and synthetic exact Go facade fixture")
	}
	published, err := filepath.EvalSymlinks(filepath.Join(installed, "current"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "subyard-frozen-facade.")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("retained controlled facade fixture:", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	runtimeRoot, configHome, dataHome := filepath.Join(root, "runtime"), filepath.Join(root, "config"), filepath.Join(root, "data")
	for _, path := range []string{runtimeRoot, configHome, dataHome, filepath.Join(root, "home"), filepath.Join(configHome, ".sync/settings")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	const sourceID, targetID = "1.0.0-facade-fixture-source", "1.0.0-facade-fixture-target"
	var artifact, registry releasetransition.Fingerprint
	for _, release := range []string{sourceID, targetID} {
		destination := filepath.Join(runtimeRoot, "releases", release)
		var entries []string
		err := filepath.WalkDir(published, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(published, path)
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.MkdirAll(filepath.Join(destination, relative), 0o700)
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("fixture asset is not regular")
			}
			if relative == "runtime-files.sha256" {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if relative == "bin/yard-engine" {
				body = image
			}
			testkit.WriteFile(t, filepath.Join(destination, relative), body, info.Mode().Perm())
			digest := sha256.Sum256(body)
			entries = append(entries, hex.EncodeToString(digest[:])+"  ./"+relative)
			if relative == "config/release-transition.json" {
				registry = releasetransition.Fingerprint(hex.EncodeToString(digest[:]))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(entries)
		manifest := []byte(strings.Join(entries, "\n") + "\n")
		testkit.WriteFile(t, filepath.Join(destination, "runtime-files.sha256"), manifest, 0o600)
		digest := sha256.Sum256(manifest)
		artifact = releasetransition.Fingerprint(hex.EncodeToString(digest[:]))
	}
	if err := os.Symlink("releases/"+sourceID, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configHome, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0o600)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields("bash dirname uname jq sha256sum tar gzip cp chmod mv mktemp cut install readlink basename ln sed awk sort find env date id getent python3 cat head stat tr grep wc realpath flock rm cmp comm mkdir rmdir sync") {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	ownerRoot := filepath.Join(runtimeRoot, "releases", targetID)
	environment := []string{"HOME=" + filepath.Join(root, "home"), "PATH=" + tools, "SUBYARD_HOME=" + dataHome, "SUBYARD_CONFIG_HOME=" + configHome,
		"SUBYARD_FACADE_FIXTURE_ROOT=" + ownerRoot, "SUBYARD_NO_AUDIT=1", "SUBYARD_INCUS_SOCKET=" + server.SocketPath,
		"SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH=" + filepath.Join(root, "absent-unit")}
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome,
		Yard: "default", Target: targetID, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: artifact, RegistryDigest: registry}
	invoke := func(request releasetransition.ProcessRequest, grant string, barrier string) (releasetransition.ProcessResponse, error) {
		t.Helper()
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(filepath.Join(ownerRoot, "bin/yard-engine"), "_release-transition")
		command.Env = append([]string{}, environment...)
		if barrier != "" {
			command.Env = append(command.Env, "SUBYARD_OWNER_LOCK_FD="+barrier)
		}
		if grant != "" {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			_, err = io.WriteString(write, grant+"\n")
			write.Close()
			if err != nil {
				t.Fatal(err)
			}
			command.ExtraFiles = []*os.File{read}
			command.Env = append(command.Env, "SUBYARD_RELEASE_TRANSITION_GRANT_FD=3")
		}
		var stdout, stderr bytes.Buffer
		command.Stdin, command.Stdout, command.Stderr = bytes.NewReader(raw), &stdout, &stderr
		if err := command.Run(); err != nil {
			return releasetransition.ProcessResponse{}, fmt.Errorf("%w: %s", err, stderr.String())
		}
		var response releasetransition.ProcessResponse
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			return response, err
		}
		return response, nil
	}
	inspected, err := invoke(request, "", "")
	if err != nil || inspected.Inspection == nil || !inspected.Inspection.Assessment.Changed {
		t.Fatal("frozen inspection", inspected, err)
	}
	if _, err := os.Stat(uninstall.OwnerLockPath(dataHome)); !os.IsNotExist(err) {
		t.Fatal("read-only frozen inspection acquired account lock", err)
	}
	inspectedAgain, err := invoke(request, "", "")
	if err != nil || inspectedAgain.Inspection == nil || inspectedAgain.Inspection.Plan != inspected.Inspection.Plan {
		t.Fatal("independent frozen inspection changed its stable plan", inspected.Inspection, inspectedAgain.Inspection, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: inspected.Inspection.Plan, Authorization: "fixture-parent-grant"}
	if _, err := invoke(request, "", ""); err == nil {
		t.Fatal("frozen convergence accepted absent private grant")
	}
	if _, err := invoke(request, "fixture-parent-grant", "44"); err == nil {
		t.Fatal("frozen convergence replaced invalid supplied barrier")
	}
	if _, err := os.Stat(filepath.Join(configHome, "release-transition/v2/journal.json")); !os.IsNotExist(err) {
		t.Fatal("rejected frozen caller published history", err)
	}
	converged, err := invoke(request, "fixture-parent-grant", "")
	if err != nil || converged.Outcome == nil || !converged.Outcome.ReachedGoal {
		t.Fatal("frozen real-grant/missing-barrier bridge", converged, err)
	}
	if _, err := invoke(request, "fixture-parent-grant", ""); err == nil {
		t.Fatal("frozen caller replayed its original consumed plan")
	}
}
