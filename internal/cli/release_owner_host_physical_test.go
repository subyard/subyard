package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type nativeHostPhysicalRequest struct {
	Script      string
	Environment []string
	PID         string
	Terminal    string
}

func TestNativeHostInstallerPhysicalTransportChild(t *testing.T) {
	request := os.Getenv("SUBYARD_NATIVE_HOST_CHILD")
	if request == "" {
		return
	}
	if _, err := os.Stat("/run/subyard-e2e-lease.json"); err != nil || !strings.HasPrefix(request, "/tmp/") {
		os.Exit(2)
	}
	raw, err := os.ReadFile(request)
	if err != nil {
		os.Exit(2)
	}
	var value nativeHostPhysicalRequest
	if json.Unmarshal(raw, &value) != nil || filepath.Base(value.Script) != "install-power-reconciler.sh" {
		os.Exit(2)
	}
	command := exec.Command("/bin/bash", "-c", `set +e
"$1" --yes
status=$?
printf '%s\n' "$status" > "$2"
exit "$status"`, "native-host-fixture", value.Script, value.Terminal)
	command.Env = value.Environment
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if command.Start() != nil {
		os.Exit(3)
	}
	if os.WriteFile(value.PID, []byte(strconv.Itoa(command.Process.Pid)), 0o600) != nil {
		os.Exit(3)
	}
	if command.Wait() != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

// This fixture uses synthetic component inputs and Incus facts. Only the
// original-OFD privilege transfer, surviving installer and full teardown
// admission boundary are physical claims.
func TestNativeHostInstallerPhysicalOwnerCustody(t *testing.T) {
	preflight := os.Getenv("SUBYARD_NATIVE_HOST_PREFLIGHT") == "1"
	if !preflight && os.Getenv("SUBYARD_NATIVE_HOST_PHYSICAL") != "native-host-custody-v1" {
		t.Skip("leased physical host fixture not selected")
	}
	if !preflight {
		if _, err := os.Stat("/run/subyard-e2e-lease.json"); err != nil || os.Getuid() == 0 {
			t.Fatal("leased default non-root sudo operator required")
		}
	}
	program, loaded, _, _ := teardownResetFixture(t)
	publicRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	manifest := ""
	for _, relative := range []string{"bin/yard", "config/commands.registry", "scripts/teardown-physical.sh", "scripts/lib/teardown-services.sh", "scripts/lib/teardown-plan.py"} {
		path := filepath.Join(program.options.RepositoryRoot, relative)
		if relative != "config/commands.registry" {
			raw, err := os.ReadFile(filepath.Join(publicRoot, relative))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, raw, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		manifest += fmt.Sprintf("%x  %s\n", sha256.Sum256(raw), relative)
	}
	testkit.WriteFile(t, filepath.Join(program.options.RepositoryRoot, "runtime-files.sha256"), []byte(manifest), 0o600)
	binaryPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	engineBytes, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	program.options.DispatcherPath = filepath.Join(program.options.RepositoryRoot, "bin/yard-engine")
	testkit.WriteFile(t, program.options.DispatcherPath, engineBytes, 0o700)
	definition, _ := program.manifest.Lookup("teardown")
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Definition: definition, Loaded: loaded, Arguments: []string{"--full", "--yes"}})
	if err != nil {
		t.Fatal("full teardown prerequisite", err)
	}
	defer prepared.Close()
	authorizePreparedFixture(prepared)
	if preflight {
		return
	}
	repository, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if selected := os.Getenv("SUBYARD_NATIVE_HOST_REPOSITORY"); selected != "" {
		repository = selected
	}
	root := testkit.TempDir(t)
	entered, release := filepath.Join(root, "entered"), filepath.Join(root, "release")
	unit := "/etc/systemd/system/subyard-native-custody-power.service"
	if _, err := os.Lstat(unit); !os.IsNotExist(err) {
		t.Fatal("refusing existing physical fixture unit", err)
	}
	t.Cleanup(func() {
		exec.Command("sudo", "-n", "systemctl", "disable", "subyard-native-custody-power.service").Run()
		exec.Command("sudo", "-n", "rm", "-f", unit).Run()
		exec.Command("sudo", "-n", "systemctl", "daemon-reload").Run()
		exec.Command("sudo", "-n", "rm", "-rf", filepath.Join(root, "root-resources")).Run()
	})
	engine := filepath.Join(root, "engine")
	engineBody := fmt.Sprintf("#!/bin/sh\nset -eu\ncase \"$*\" in\n_publication-contracts)\n touch '%s'\n while [ ! -f '%s' ]; do sleep .05; done\n printf 'eyJ2ZXJzaW9uIjoxLCJyZWFkZXJFcG9jaCI6MiwiY29udHJhY3RzIjpbXX0=\\n';;\n'_network-lock ensure') exit 0;;\n*) exit 64;;\nesac\n", entered, release)
	testkit.WriteFile(t, engine, []byte(engineBody), 0o700)
	_, unlock, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	owner, err := uninstall.BorrowOwnerLock(loaded.Context.Paths.DataHome)
	if err != nil || owner == nil {
		t.Fatal("original owner barrier", err)
	}
	environment := append(os.Environ(), "SUBYARD_NATIVE_OWNER_STDIN=1", "SUBYARD_USER="+os.Getenv("USER"), "SUBYARD_HOME="+loaded.Context.Paths.DataHome, "SUBYARD_POWER_ENGINE_SOURCE="+engine, "SUBYARD_POWER_LIBEXEC_DIR="+filepath.Join(root, "root-resources"), "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "root-resources", "engine"), "SUBYARD_POWER_UNIT_PATH="+unit, "SUBYARD_SUDO_PREAUTHORIZED=1")
	request := nativeHostPhysicalRequest{Script: filepath.Join(repository, "scripts/install-power-reconciler.sh"), Environment: environment, PID: filepath.Join(root, "installer.pid"), Terminal: filepath.Join(root, "installer.terminal")}
	raw, _ := json.Marshal(request)
	path := filepath.Join(root, "request.json")
	testkit.WriteFile(t, path, raw, 0o600)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(root, "installer.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	helper := exec.Command(binary, "-test.run=^TestNativeHostInstallerPhysicalTransportChild$")
	helper.Env = append(os.Environ(), "SUBYARD_NATIVE_HOST_CHILD="+path)
	helper.Stdin = owner
	helper.Stdout = log
	helper.Stderr = log
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	t.Cleanup(func() {
		os.WriteFile(release, nil, 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, closeOnly, err := uninstall.OwnerLock(ctx, loaded.Context.Paths.DataHome, nil); err == nil {
			closeOnly()
		} else {
			t.Errorf("physical fixture privileged child did not drain before cleanup: %v", err)
		}
	})
	defer func() { helper.Process.Kill(); helper.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			raw, _ := os.ReadFile(filepath.Join(root, "installer.log"))
			t.Fatal("actual sudo installer never entered approved query", string(raw))
		}
		time.Sleep(50 * time.Millisecond)
	}
	helper.Process.Kill()
	helper.Wait()
	unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	_, err = prepared.Execute(ctx, nil, io.Discard)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "owner mutation lock") {
		t.Fatal("full teardown did not wait original surviving installer OFD", err)
	}
	testkit.WriteFile(t, release, nil, 0o600)
	deadline = time.Now().Add(30 * time.Second)
	for {
		if terminal, err := os.ReadFile(request.Terminal); err == nil {
			if string(terminal) != "0\n" {
				output, _ := os.ReadFile(filepath.Join(root, "installer.log"))
				t.Fatal("actual installer returned failure", string(terminal), string(output))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("installer did not resume after fixture release")
		}
		time.Sleep(50 * time.Millisecond)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	_, done, err := uninstall.OwnerLock(ctx, loaded.Context.Paths.DataHome, nil)
	cancel()
	if err != nil {
		t.Fatal("installer did not close owner custody", err)
	}
	done()
	raw, _ = os.ReadFile(filepath.Join(root, "installer.log"))
	t.Logf("physical default-sudo original-OFD survived helper death; full-teardown waited; installer log: %s", raw)
}
