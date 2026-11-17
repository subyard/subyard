package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	rt "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	rollbackfixture "github.com/Subyard/Subyard/internal/testkit/rollback"
)

func TestPackagedRollbackTargetResumesAfterSourceDiesAtLinkSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	targetRoot := sourceAdmissionToolingFixture(t, runtimeRoot)
	targetRegistry := mustReadConfigFixture(t, filepath.Join(targetRoot, "config/release-transition.json"))
	targetManifest := mustReadConfigFixture(t, filepath.Join(targetRoot, "runtime-files.sha256"))
	registry, _, err := rt.ParseRegistryV2(targetRegistry, rt.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	registry.Migrations = append(registry.Migrations, rt.MigrationDefinitionV2{ID: "future-project-v2", Domain: "project-state", FromEpoch: 1, ToEpoch: 2, Kind: "future-project-kind"})
	registry.CurrentEpochs["project-state"] = 2
	sourceRegistry, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	target := rt.ReleaseID(filepath.Base(targetRoot))
	pair := rt.ReleasePair{From: "0.30.0-source", Target: target, Previous: &target}
	sourceRoot := filepath.Join(runtimeRoot, "releases", string(pair.From))
	for _, name := range []string{"bin", "config"} {
		if err := os.MkdirAll(filepath.Join(sourceRoot, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(sourceRoot, "bin/yard-engine"), []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 64\nprintf 'yard-engine 0.30.0\\n'\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(sourceRoot, "config/release-transition.json"), sourceRegistry, 0o600)
	sourceManifest := sourceFixtureManifest(t, sourceRoot)
	for name, release := range map[string]rt.ReleaseID{"current": pair.From, "previous": pair.Target} {
		if err := os.Symlink("releases/"+string(release), filepath.Join(runtimeRoot, name)); err != nil {
			t.Fatal(err)
		}
	}
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetExtensions("projects", "instances", "instance_all_projects", "storage", "network")
	environment = append(environment, "PATH=/usr/bin:/bin", "SUBYARD_INCUS_SOCKET="+server.SocketPath,
		"SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"),
		"SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
	runtime := releaseruntime.New(releaseruntime.Config{Inputs: releaseruntime.CaptureCommandInputs(environment, nil), Stderr: os.Stderr})
	defer runtime.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(root, "authorized-activation.json")
	helper := func(mode string) *exec.Cmd {
		process := exec.CommandContext(ctx, executable, "-test.run", "^TestPackagedRollbackTargetUpdaterHelper$")
		process.Env = append(append([]string{}, environment...), "SUBYARD_ROLLBACK_TEST_MODE="+mode, "SUBYARD_ROLLBACK_TEST_STATE="+requestPath)
		process.Stderr = os.Stderr
		return process
	}
	rollbackfixture.Seed(t, rollbackfixture.Inputs{
		RuntimeRoot: runtimeRoot, ConfigHome: home, Releases: pair,
		SourceArtifact: rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(sourceManifest))),
		TargetArtifact: rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(targetManifest))),
		SourceRegistry: sourceRegistry, TargetRegistry: targetRegistry,
		Inspect: func(request rt.RollbackTargetRequest) rt.RollbackActivationInspection {
			response, err := runtime.InvokeRollbackTarget(ctx, request, nil)
			if err != nil || response.Inspection == nil {
				t.Fatalf("packaged R inspection = %+v, %v", response, err)
			}
			return *response.Inspection
		},
		Activate: func(request rt.RollbackTargetRequest) rt.RollbackTargetResponse {
			// S's test driver persists the assessed inputs. A fresh updater
			// obtains authorization and the selected digest from the journal.
			request.Journal = ""
			request.Execution = nil
			payload, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, requestPath, payload, 0o600)
			first := helper("switch")
			stdout, err := first.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Start(); err != nil {
				t.Fatal(err)
			}
			defer first.Wait()
			if marker, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || marker != "switched\n" {
				t.Fatalf("S did not durably switch links: %q, %v", marker, err)
			}
			if err := first.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := first.Wait(); err == nil {
				t.Fatal("initial S unexpectedly exited successfully")
			}
			output, err := helper("resume").Output()
			if err != nil {
				t.Fatalf("fresh S failed against packaged R: %v", err)
			}
			response, err := rt.ParseRollbackTargetResponse(output)
			if err != nil {
				t.Fatal(err)
			}
			return response
		},
	})
}

func TestPackagedRollbackTargetUpdaterHelper(t *testing.T) {
	mode := os.Getenv("SUBYARD_ROLLBACK_TEST_MODE")
	if mode == "" {
		return
	}
	var request rt.RollbackTargetRequest
	if err := json.Unmarshal(mustReadConfigFixture(t, os.Getenv("SUBYARD_ROLLBACK_TEST_STATE")), &request); err != nil {
		t.Fatal(err)
	}
	store, err := rt.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.OpenTransitionLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if mode == "switch" {
		links, err := rt.NewRuntimeLinkStore(request.RuntimeRoot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := links.Activate(request.Releases); err != nil {
			t.Fatal(err)
		}
		fmt.Println("switched")
		time.Sleep(time.Minute)
		t.Fatal("S survived the test interruption")
	}
	selected, err := store.ReadCurrentJournal()
	if err != nil || !selected.Exists {
		t.Fatalf("fresh S has no selected journal: %v", err)
	}
	request.Journal = selected.Fingerprint
	request.Execution = &rt.Execution{Plan: request.Plan.Token}
	runtime := releaseruntime.New(releaseruntime.Config{Inputs: releaseruntime.CaptureCommandInputs(os.Environ(), nil), Stderr: os.Stderr})
	defer runtime.Close()
	response, err := runtime.InvokeRollbackTarget(context.Background(), request, lock)
	if err != nil || response.Completion == nil {
		t.Fatalf("durable rollback resumption = %+v, %v", response, err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // The subprocess stdout is solely the protocol response.
}
