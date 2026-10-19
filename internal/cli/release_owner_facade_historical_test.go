//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRustDeliveryAuthenticHistoricalRollback(t *testing.T) {
	rustDeliveryHistoricalRollback(t, false)
}

func TestRustDeliveryReleasedPublicCallerFinal(t *testing.T) {
	if os.Getenv("SUBYARD_RUST_FINAL_DELIVERY_BUNDLE") == "" {
		t.Skip("opt-in unmodified final packaged public caller gate")
	}
	rustDeliveryHistoricalRollback(t, true)
}

func rustDeliveryHistoricalRollback(t *testing.T, publicRollforward bool) {
	t.Helper()
	installed := os.Getenv("SUBYARD_RUST_DELIVERY_RUNTIME_ROOT")
	if installed == "" {
		t.Skip("opt-in declared current source and exact authentic 0.21 rollback owner")
	}
	active, err := filepath.EvalSymlinks(filepath.Join(installed, "current"))
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	runtimeRoot, configHome, dataHome := filepath.Join(root, "runtime"), filepath.Join(root, "config"), filepath.Join(root, "data")
	for _, path := range []string{runtimeRoot, configHome, dataHome, filepath.Join(root, "home"), filepath.Join(configHome, ".sync/settings")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceID, targetID := filepath.Base(active)+"-active", "0.21.0-1fee6251856e"
	source := filepath.Join(runtimeRoot, "releases", sourceID)
	if err := filepath.WalkDir(active, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(active, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(source, relative), 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		testkit.WriteFile(t, filepath.Join(source, relative), body, info.Mode().Perm())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(runtimeRoot, "releases", targetID)
	extractFacadeHistoricalRuntime(t, "0.21.0", target)
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(cache, "0.21.0"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		name := "subyard-0.21.0-linux-amd64.tar.gz" + suffix
		body, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".build/rust-p1-predecessors/attempt-1/baseline-0.21.0", name))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(cache, "0.21.0", name), body, 0o600)
	}
	if err := os.Symlink("releases/"+sourceID, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+targetID, filepath.Join(runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configHome, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0o600)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	environment := append([]string{}, os.Environ()...)
	environment = append(environment, "HOME="+filepath.Join(root, "home"), "SUBYARD_HOME="+dataHome, "SUBYARD_CONFIG_HOME="+configHome, "YARD_RUNTIME_ROOT="+runtimeRoot, "YARD_RELEASE_CACHE="+cache,
		"SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_NO_AUDIT=1", "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
	program, err := New(Options{RepositoryRoot: source, Environment: environment, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	config := program.releaseRuntimeConfig(releaseruntime.RuntimeContext{OperatorHome: filepath.Join(root, "home"), DataHome: dataHome, ConfigHome: configHome, RuntimeRoot: runtimeRoot})
	runtime := releaseruntime.New(config)
	defer runtime.Close()
	prepared, err := runtime.PrepareTransition(context.Background(), []string{"--rollback", "--runtime-root", runtimeRoot}, configHome, "default")
	if err != nil {
		t.Fatal("authentic 0.21 rollback preparation", err)
	}
	if prepared.Close != nil {
		defer prepared.Close()
	}
	if prepared.TargetVersion != "0.21.0" || prepared.TargetRelease != targetID {
		t.Fatal("rollback did not select the exact historical target", prepared.TargetVersion, prepared.TargetRelease)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal("authentic 0.21 rollback execution", err)
	}
	current, _ := os.Readlink(filepath.Join(runtimeRoot, "current"))
	previous, _ := os.Readlink(filepath.Join(runtimeRoot, "previous"))
	if current != "releases/"+targetID || previous != "releases/"+sourceID {
		t.Fatal("historical rollback did not reach the exact retained runtime")
	}
	raw, err := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(raw)
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete || journal.Goal.Target != releasetransition.ReleaseID(targetID) {
		t.Fatal("historical owner did not complete its own durable journal", err)
	}
	// Exercise ordinary explicit historical selection independently of rollback.
	// A complete original historical owner remains responsible for same-target
	// inspection; a newly compiled current helper never impersonates it.
	ordinary := releaseruntime.New(config)
	defer ordinary.Close()
	checked, err := ordinary.PrepareTransition(context.Background(), []string{"--check", "--offline", "--version", "0.21.0", "--runtime-root", runtimeRoot}, configHome, "default")
	if err != nil {
		t.Fatal("ordinary explicit historical target", err)
	}
	if checked.Close != nil {
		defer checked.Close()
	}
	if err := checked.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var declaration map[string]any
	if body, err := os.ReadFile(filepath.Join(target, "config/release-owner.json")); err == nil && json.Unmarshal(body, &declaration) == nil {
		t.Fatal("historical fixture unexpectedly declares a rebuilt Rust owner")
	}
	if !publicRollforward {
		return
	}
	// The released launcher and candidate helper remain byte-for-byte packaged
	// images. This exercises the old public command, not the TestMain facade.
	bundle := os.Getenv("SUBYARD_RUST_FINAL_DELIVERY_BUNDLE")
	metadata, err := os.ReadFile(bundle + ".manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(metadata, &manifest); err != nil || manifest.Version == "" {
		t.Fatal("final candidate version is unavailable", err)
	}
	finalCache := filepath.Join(cache, manifest.Version)
	if err := os.MkdirAll(finalCache, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		body, err := os.ReadFile(bundle + suffix)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(finalCache, filepath.Base(bundle)+suffix), body, 0o600)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	nativeTools := filepath.Join(root, "native-tools")
	if err := os.MkdirAll(nativeTools, 0o700); err != nil {
		t.Fatal(err)
	}
	// Match the existing cold-install acceptance whitelist. No Go, Cargo or
	// rustc executable is available to the released caller or either owner.
	for _, tool := range strings.Fields("bash dirname uname jq sha256sum tar gzip cp chmod mv mktemp cut install readlink basename ln sed awk sort find env date id getent python3 cat head stat tr grep wc realpath flock rm cmp comm mkdir rmdir sync") {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(nativeTools, tool)); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(ctx, filepath.Join(target, "bin/yard"), "update", "--offline", "--version", manifest.Version, "--yes")
	command.Env = append(environment, "PATH="+nativeTools, "SUBYARD_OPERATOR_HOME="+filepath.Join(root, "home"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("released public caller could not complete the final Rust transition", err, string(output))
	}
	current, err = os.Readlink(filepath.Join(runtimeRoot, "current"))
	if err != nil || !strings.HasPrefix(filepath.Base(current), manifest.Version+"-") {
		t.Fatal("released public caller did not activate the final candidate", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRoot, current, "config/release-owner.json")); err != nil {
		t.Fatal("final public transition did not select a declared Rust runtime", err)
	}
	raw, err = os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err = releasetransition.ParseJournal(raw)
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete || string(journal.Goal.Target) != filepath.Base(current) {
		t.Fatal("released public caller left the final Rust transition incomplete", err)
	}
	indexRaw, err := os.ReadFile(filepath.Join(configHome, "migrations/native-custody/v1", string(journal.Transaction), "bundle.json"))
	if err != nil {
		t.Fatal("final caller did not execute the Rust owner with its retained native bundle", err)
	}
	var index struct {
		Version     int    `json:"version"`
		Transaction string `json:"transaction"`
		Plan        string `json:"plan"`
		Journal     string `json:"journal"`
	}
	if err := json.Unmarshal(indexRaw, &index); err != nil || index.Version != 1 ||
		index.Transaction != string(journal.Transaction) || index.Plan != string(journal.AuthorizationPlan) || index.Journal != string(journal.IntentDigest) {
		t.Fatal("final Rust owner bundle differs from its exact durable journal", err)
	}
}
