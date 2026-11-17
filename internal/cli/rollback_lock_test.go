package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	rt "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRollbackTargetClosesEffectLockInAssetProbe(t *testing.T) {
	root := testkit.TempDir(t)
	store, err := rt.NewPOSIXV2Store(root)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.OpenTransitionLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	observed := filepath.Join(root, "probe")
	lockPath := filepath.Join(root, "migrations", "update.lock")
	source := filepath.Join(root, "releases", "1.2.3-source")
	for _, name := range []string{"bin", "config"} {
		if err := os.MkdirAll(filepath.Join(source, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf("#!/bin/sh\nstate=closed\nfor fd in /proc/self/fd/*; do if [ \"$fd\" -ef %q ]; then state=inherited; fi; done\nprintf '%%s' \"$state\" > %q\nprintf 'yard-engine 1.2.3\\n'\n", lockPath, observed)
	testkit.WriteFile(t, filepath.Join(source, "bin", "yard-engine"), []byte(script), 0o700)
	registry := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`)
	testkit.WriteFile(t, filepath.Join(source, "config", "release-transition.json"), registry, 0o600)
	manifest := sourceFixtureManifest(t, source)
	pin := rt.Fingerprint(strings.Repeat("a", 64))
	token := rt.PlanToken("plan-v1-" + string(pin))
	request := rt.RollbackTargetRequest{SchemaVersion: 3, Mode: "activate-under-v2", RuntimeRoot: root, ConfigHome: root,
		Releases:       rt.ReleasePair{From: "1.2.3-source", Target: "1.2.3-missing"},
		SourceArtifact: rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest))), SourceRegistry: rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(registry))),
		TargetArtifact: pin, TargetRegistry: pin, Projection: pin, Journal: pin, NativeHistory: &rt.LedgerV2{},
		Plan: &rt.RollbackActivationPlan{Token: token, Assessment: domain.ActionAssessment{Action: "fixture"}}, Execution: &rt.Execution{Plan: token}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(root, "request")
	testkit.WriteFile(t, requestPath, raw, 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer placeholder.Close()
	command := exec.Command(executable, "-test.run", "^TestRollbackTargetLockProbeHelper$")
	command.Env = append(os.Environ(), "SUBYARD_TEST_ROLLBACK_LOCK_REQUEST="+requestPath, "SUBYARD_RELEASE_TRANSITION_LOCK_FD=8")
	command.ExtraFiles = []*os.File{placeholder, placeholder, placeholder, placeholder, placeholder, lock}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("asset verification helper: %v: %s", err, output)
	}
	if value := mustReadConfigFixture(t, observed); string(value) != "closed" {
		t.Fatalf("read-only probe held the effect lock: %s", value)
	}
}

func TestRollbackTargetLockProbeHelper(t *testing.T) {
	path := os.Getenv("SUBYARD_TEST_ROLLBACK_LOCK_REQUEST")
	if path == "" {
		return
	}
	cli := &CLI{options: Options{Stderr: os.Stderr}}
	if code := cli.runRollbackTargetProcess(context.Background(), mustReadConfigFixture(t, path), io.Discard); code != 1 {
		t.Fatalf("unexpected result: %d", code)
	}
}
