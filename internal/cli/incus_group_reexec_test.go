package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Exercise the real CLI -> init runtime -> exec -> new CLI path. Only the
// privileged installer, group switch and unavailable Incus socket are replaced.
func TestIncusGroupReexecPreservesCommandEnvironment(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		name := "first init"
		if guarded {
			name = "second failure does not loop"
		}
		t.Run(name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			values := environmentMap(environment)
			delete(values, "SSH_PORT")
			values["DEV_UID"] = "2001" // Explicit launch override beats the named file.
			values["SUBYARD_TEST_REEXEC_ROOT"] = root
			values["SUBYARD_TEST_REEXEC_PHASE"] = "install"
			if guarded {
				values["SUBYARD_SG_REEXEC"] = "1"
			}
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			values["PATH"] = bin + ":" + os.Getenv("PATH")
			yards := filepath.Join(root, "state", "yards")
			if err := os.MkdirAll(yards, 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), "SSH_PORT=2222\n", 0o600)
			writeCLIFile(t, filepath.Join(yards, "demo.env"), "SSH_PORT=2233\nDEV_UID=3001\n", 0o600)
			writeCLIFile(t, filepath.Join(root, "scripts", "01-install-incus.sh"),
				"#!/bin/sh\n[ \"$SSH_PORT\" = 2233 ] && [ \"$DEV_UID\" = 2001 ] || exit 72\n"+
					"printf installed > \"$SUBYARD_TEST_REEXEC_ROOT/installed\"\n", 0o700)
			writeCLIFile(t, filepath.Join(bin, "sg"),
				"#!/bin/sh\n[ \"$1\" = incus-admin ] && [ \"$2\" = -c ] || exit 73\nexec /bin/sh -c \"$3\"\n", 0o700)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(bin, "dispatcher"),
				"#!/bin/sh\nexport SUBYARD_TEST_REEXEC_PHASE=child\nexec "+shellquote.Word(executable)+
					" -test.run='^TestIncusGroupReexecProcess$' -- \"$@\"\n", 0o700)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestIncusGroupReexecProcess$")
			command.Env = environmentList(values, nil)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("group re-exec failed: %v\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(root, "installed")); err != nil {
				t.Fatalf("installer did not receive the resolved named context: %v", err)
			}
		})
	}
}

func TestIncusGroupReexecProcess(t *testing.T) {
	phase := os.Getenv("SUBYARD_TEST_REEXEC_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("SUBYARD_TEST_REEXEC_ROOT")
	program, err := New(Options{
		RepositoryRoot: root, DispatcherPath: filepath.Join(root, "bin", "dispatcher"),
		Program: "yard", Environment: os.Environ(),
		Incus: &testkit.Incus{Err: errors.New("group membership is not active")},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("demo")
	if err != nil {
		t.Fatal(err)
	}
	if phase == "install" {
		err := program.initPlatform(loaded, nil).ApplyStage(context.Background(), ports.ReconcileStageIncus)
		if os.Getenv("SUBYARD_SG_REEXEC") == "1" && err != nil && strings.Contains(err.Error(), "fresh incus-admin session") {
			return
		}
		t.Fatalf("expected process replacement or the retry guard, got %v", err)
	}
	if !slices.Equal(os.Args[len(os.Args)-4:], []string{"-Y", "demo", "init", "--yes"}) {
		t.Fatalf("re-exec lost the named init selection: %q", os.Args)
	}
	if os.Getenv("SUBYARD_SG_REEXEC") != "1" || os.Getenv("ASSUME_YES") != "1" {
		t.Fatal("re-exec omitted its loop guard or init consent")
	}
	yards, err := program.powerYardContexts(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(yards) != 2 || yards[0].YardName != "default" || yards[0].SSHPort != 2222 ||
		yards[1].YardName != "demo" || yards[1].SSHPort != 2233 {
		t.Fatalf("group re-exec polluted independent yard ports: %#v", yards)
	}
	if program.baseEnv["DEV_UID"] != "2001" || loaded.Environment["DEV_UID"] != "2001" {
		t.Fatal("group re-exec lost an explicit command override")
	}
}
