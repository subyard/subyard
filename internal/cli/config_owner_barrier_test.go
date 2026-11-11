package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestConfigApplierOwnerBarrierProcess(t *testing.T) {
	home := os.Getenv("SUBYARD_TEST_CONFIG_OWNER_HOME")
	if home == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, release, err := uninstall.OwnerLock(ctx, home, nil)
	if err != nil {
		t.Fatal("configuration child could not enter the owner barrier", err)
	}
	release()
	if (os.Getenv("SUBYARD_OWNER_LOCK_FD") != "") != (os.Getenv("SUBYARD_TEST_CONFIG_OWNER_HELD") == "1") {
		t.Fatal("configuration child received the wrong owner barrier authority")
	}
}

func TestDispatcherConfigApplierRetainsOwnerBarrier(t *testing.T) {
	for _, mode := range []string{"direct", "drift-aware", "update-refresh"} {
		for _, homeSource := range []string{"explicit", "operator-home", "home"} {
			for _, held := range []bool{false, true} {
				name := mode + "/" + homeSource + "/unheld"
				if held {
					name = mode + "/" + homeSource + "/held"
				}
				t.Run(name, func(t *testing.T) {
					root := testkit.TempDir(t)
					home := filepath.Join(root, ".subyard")
					executable, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					dispatcher := filepath.Join(root, "yard")
					testkit.WriteFile(t, dispatcher, []byte("#!/bin/sh\nexec \"$SUBYARD_TEST_EXECUTABLE\" -test.run=^TestConfigApplierOwnerBarrierProcess$ -- \"$@\"\n"), 0700)
					environment := map[string]string{
						"HOME": root, "SUBYARD_TEST_EXECUTABLE": executable,
						"SUBYARD_TEST_CONFIG_OWNER_HOME": home,
					}
					switch homeSource {
					case "explicit":
						environment["SUBYARD_HOME"] = home
						environment["SUBYARD_OPERATOR_HOME"] = filepath.Join(root, "other-operator")
					case "operator-home":
						environment["SUBYARD_OPERATOR_HOME"] = root
						environment["HOME"] = filepath.Join(root, "other-home")
					}
					if held {
						_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
						if err != nil {
							t.Fatal(err)
						}
						defer release()
						environment["SUBYARD_TEST_CONFIG_OWNER_HELD"] = "1"
					}
					var output bytes.Buffer
					// Bound both lock acquisition and the complete synthetic process.
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if mode == "update-refresh" {
						program := &CLI{baseEnv: environment, options: Options{Stdout: &output, Stderr: &output}}
						execution := &releaseExecution{yard: "named", prepared: releaseruntime.Prepared{ActiveLauncher: dispatcher}}
						err = program.refreshReleaseConfig(ctx, execution)
					} else {
						applier := dispatcherConfigApplier{path: dispatcher, environment: environment,
							stdout: &output, stderr: &output, applyDrift: mode == "drift-aware"}
						err = applier.ApplyConfig(ctx, "named")
					}
					if err != nil {
						t.Fatalf("configuration dispatch failed: %v\n%s", err, output.String())
					}
					// The child and the borrowed descriptor must only close their OFD
					// references; neither may unlock the parent's ongoing operation.
					probe, err := os.OpenFile(uninstall.OwnerLockPath(home), os.O_RDWR, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer probe.Close()
					err = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
					if held && !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
						t.Fatal("configuration child released its parent's owner barrier", err)
					}
					if !held && err != nil {
						t.Fatal("configuration child retained the owner barrier after exit", err)
					}
				})
			}
		}
	}
}
