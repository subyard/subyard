package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

type hostCompletionObserver struct {
	custodyTransportObserver
	terminals   int
	uncertain   bool
	kind        string
	terminalErr error
}

func (o *hostCompletionObserver) CommandStarted(kind, source string) (uint64, error) {
	o.kind = kind
	return 7, nil
}
func (o *hostCompletionObserver) CommandTerminal(uint64) error { o.terminals++; return o.terminalErr }
func (o *hostCompletionObserver) Uncertain()                   { o.uncertain = true }

func TestNativeReadOnlyProbeClosesExpectedDrift(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts/06-network.sh"), []byte("#!/bin/sh\nexit 1\n"), 0700)
	_, release, err := uninstall.OwnerLock(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	runtime := Runtime{RepositoryRoot: root, Environment: append(os.Environ(), "SUBYARD_HOME="+root), Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: root}}, Incus: &testkit.Incus{}}
	for _, mode := range []string{"probe", "terminal-error", "mutation"} {
		t.Run(mode, func(t *testing.T) {
			observer := &hostCompletionObserver{}
			if mode == "terminal-error" {
				observer.terminalErr = errors.New("fixture terminal write failed")
			}
			ctx := ports.WithNativeOperationObserver(t.Context(), observer)
			if mode == "mutation" {
				if err := runtime.runScript(ctx, io.Discard, "06-network.sh", "--yes"); err == nil || !observer.uncertain || observer.terminals != 0 {
					t.Fatal("failed mutation lost uncertainty", err, observer)
				}
				return
			}
			converged, err := runtime.CheckStage(ctx, ports.ReconcileStageNetwork)
			if mode == "terminal-error" {
				if !errors.Is(err, observer.terminalErr) {
					t.Fatal("terminal failure was swallowed as expected drift", err)
				}
				return
			}
			if err != nil || converged || observer.uncertain || observer.terminals != 1 {
				t.Fatal("normal negative probe left uncertain custody", err, observer)
			}
		})
	}
}

func TestNativeHostInstallCompletionRequiresOriginalGroupDrain(t *testing.T) {
	for _, mode := range []string{"drained", "surviving-child", "failed-cleanup", "oversized-stdout", "diagnostics-separated"} {
		t.Run(mode, func(t *testing.T) {
			root := testkit.TempDir(t)
			_, release, e := uninstall.OwnerLock(context.Background(), root, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer release()
			script := "#!/bin/bash\nset -eu\nprintf '{\"version\":1}\\n'\n"
			switch mode {
			case "surviving-child":
				script += "sleep 90 </dev/null >/dev/null 2>&1 &\nprintf '%s' $! > '" + filepath.Join(root, "child") + "'\n"
			case "failed-cleanup":
				script += "trap 'exit 1' EXIT\n"
			case "oversized-stdout":
				script += "printf '%05000d' 0\n"
			case "diagnostics-separated":
				script += "printf 'diagnostic on stderr\\n' >&2\n"
			}
			path := filepath.Join(root, "install-power-reconciler.sh")
			testkit.WriteFile(t, path, []byte(script), 0755)
			observer := &hostCompletionObserver{}
			capture := &hostPowerCompletion{}
			runtime := Runtime{RepositoryRoot: root, Environment: append(os.Environ(), "SUBYARD_HOME="+root), Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: root}}, hostPowerCompletion: capture}
			err := runtime.runPath(ports.WithNativeOperationObserver(context.Background(), observer), io.Discard, path, "--native-installation-result")
			if mode == "surviving-child" {
				raw, e := os.ReadFile(filepath.Join(root, "child"))
				if e != nil {
					t.Fatal(e)
				}
				pid, e := strconv.Atoi(string(raw))
				if e != nil {
					t.Fatal(e)
				}
				defer syscall.Kill(pid, syscall.SIGKILL)
			}
			valid := mode == "drained" || mode == "diagnostics-separated"
			if observer.kind != "owner-host-power-install" {
				t.Fatal("wrong custody command kind", observer.kind)
			}
			if valid {
				if err != nil || !capture.completed || capture.command != 7 || observer.terminals != 1 || observer.uncertain || capture.output.String() != "{\"version\":1}\n" {
					t.Fatal("drained stdout completion", err, capture, observer)
				}
			} else if err == nil || capture.completed || observer.terminals != 0 || !observer.uncertain {
				t.Fatal("unproven installer published terminal/result", err, capture, observer)
			}
		})
	}
}
func TestHostInstallCompletionPayloadBound(t *testing.T) {
	var output hostPowerOutput
	if n, e := output.Write([]byte(strings.Repeat("x", 4096))); n != 4096 || e != nil {
		t.Fatal(n, e)
	}
	if _, e := output.Write([]byte("x")); e == nil {
		t.Fatal("completion payload exceeded fixed bound")
	}
}

func TestRuntimeNativeAndOrdinaryBackgroundContracts(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(strconv.FormatBool(native), func(t *testing.T) {
			root := testkit.TempDir(t)
			_, release, e := uninstall.OwnerLock(context.Background(), root, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer release()
			path := filepath.Join(root, "component.sh")
			testkit.WriteFile(t, path, []byte("#!/bin/sh\nsleep 90 </dev/null >/dev/null 2>&1 &\nprintf '%s' $! > '"+filepath.Join(root, "child")+"'\n"), 0755)
			runtime := Runtime{RepositoryRoot: root, Environment: append(os.Environ(), "SUBYARD_HOME="+root), Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: root}}}
			ctx := context.Background()
			observer := &hostCompletionObserver{}
			if native {
				ctx = ports.WithNativeOperationObserver(ctx, observer)
			}
			err := runtime.runPath(ctx, io.Discard, path)
			raw, e := os.ReadFile(filepath.Join(root, "child"))
			if e != nil {
				t.Fatal(e)
			}
			pid, e := strconv.Atoi(string(raw))
			if e != nil {
				t.Fatal(e)
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			if syscall.Kill(pid, 0) != nil {
				t.Fatal("fixture background child did not survive")
			}
			if native {
				if err == nil || observer.terminals != 0 || !observer.uncertain {
					t.Fatal("native survivor closed", err, observer)
				}
			} else if err != nil {
				t.Fatal("ordinary background-child contract changed", err)
			}
		})
	}
}

func TestNativeHostInstallerActualStdoutResultAndCleanup(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_INSTALL_RESULT_NS") != "1" {
		command := exec.Command("unshare", "--user", "--map-root-user", "--mount", os.Args[0], "-test.v", "-test.run=^TestNativeHostInstallerActualStdoutResultAndCleanup$", "-test.count=1")
		command.Env = append(os.Environ(), "SUBYARD_TEST_INSTALL_RESULT_NS=1")
		out, e := command.CombinedOutput()
		if e != nil {
			t.Fatal("required actual installer private namespace", e, string(out))
		}
		t.Log(string(out))
		return
	}
	for _, cut := range []string{"success", "without-plan", "failed-cleanup"} {
		t.Run(cut, func(t *testing.T) {
			runtime := shippedPreparedSourcesFixture(t)
			home := runtime.Yard.Paths.DataHome
			_, release, e := uninstall.OwnerLock(context.Background(), home, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer release()
			root := runtime.RepositoryRoot
			if e := os.MkdirAll(filepath.Join(root, "bin"), 0755); e != nil {
				t.Fatal(e)
			}
			engine := filepath.Join(root, "bin/yard-engine")
			installed := filepath.Join(root, "installed")
			if e := os.Mkdir(installed, 0755); e != nil {
				t.Fatal(e)
			}
			contracts, e := ownerpublication.Load(root)
			if e != nil {
				t.Fatal(e)
			}
			encoded, e := ownerpublication.Encode(contracts)
			if e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, engine, []byte("#!/bin/sh\ncase \"$*\" in _publication-contracts) printf '%s\\n' '"+encoded+"';; '_network-lock ensure') :;; *) exit 64;; esac\n"), 0755)
			unit := filepath.Join(installed, "power.service")
			passive := filepath.Join(installed, ownerpublication.HandoffService)
			manager := filepath.Join(root, "systemctl")
			testkit.WriteFile(t, manager, []byte("#!/bin/sh\nif [ \"$1\" = show ] && [ \"$2\" != subyard-publication-reconcile.service ]; then printf 'LoadState=loaded\\nNeedDaemonReload=no\\n'; exit 0; fi\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nNeedDaemonReload=no\\nFragmentPath="+passive+"\\nDropInPaths=\\n'; fi\n"), 0755)
			if e := unix.Mount(manager, "/usr/bin/systemctl", "", unix.MS_BIND, ""); e != nil {
				t.Fatal(e)
			}
			defer unix.Unmount("/usr/bin/systemctl", 0)
			runtime.Environment = append(runtime.Environment, "PATH=/usr/bin:/bin", "SUBYARD_USER=root", "SUBYARD_ENGINE_CONTEXT=1", "SUBYARD_ENGINE_CONTEXT_SCHEMA=1", "ASSUME_YES=1", "SUBYARD_HOME="+home, "SUBYARD_CONFIG_HOME="+runtime.Yard.Paths.ConfigHome, "SUBYARD_POWER_ENGINE_SOURCE="+engine, "SUBYARD_POWER_LIBEXEC_DIR="+installed, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(installed, "engine"), "SUBYARD_POWER_UNIT_PATH="+unit)
			runtime.Environment = append(runtime.Environment, "SUBYARD_OPERATOR_HOME="+home, "SUBYARD_CONFIG_DIR="+root, "STORAGE_PATH="+filepath.Join(home, "storage"), "HOST_BASE="+filepath.Join(home, "host"), "RESTRICTED_DISK_PATHS="+home, "ACCESS_KIND=local", "YARD_KIND=container", "YARD_INSTANCE_NAME=yard", "INCUS_PROJECT=project", "INCUS_BRIDGE=incusbr0", "SSH_HOST=yard", "DEV_USER=dev", "DEV_UID=1000", "DEV_SUDO=0", "FORWARD_SSH_AGENT=0", "NESTED_E2E_VMS=0")
			var diagnostics bytes.Buffer
			runtime.Stderr = &diagnostics
			observer := &publicationInstallResultObserver{}
			ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), ownerpublication.Digest([]byte(root)))
			record, e := runtime.PrepareOwnerPublication(ctx)
			if e != nil {
				t.Fatal(e)
			}
			runtime, e = runtime.RestoreOwnerPublication(record)
			if e != nil {
				t.Fatal(e)
			}
			prepared, e := runtime.RestoreOwnerHostPower(ctx, record.Power)
			if e != nil {
				t.Fatal(e)
			}
			defer prepared.Close()
			sources, e := runtime.PrepareOwnerRuntimeSources(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer sources.Close()
			sibling := runtime
			var siblingSources *PreparedInstalledRuntime
			if cut == "without-plan" {
				sibling.Environment = append(append([]string(nil), runtime.Environment...), "SUBYARD_YARD_ROLE=legacy")
				siblingRecord, err := sibling.PrepareOwnerPublication(ctx)
				if err != nil || siblingRecord == nil || siblingRecord.OutputDigest != record.OutputDigest || bytes.Equal(siblingRecord.Power.Source, record.Power.Source) {
					t.Fatal("sibling must retain distinct source and identical output", err)
				}
				// The owning CLI selects one nonce for this shared output before consent.
				siblingRecord.Consumer.Generation = record.Consumer.Generation
				sibling, err = sibling.RestoreOwnerPublication(siblingRecord)
				if err != nil {
					t.Fatal(err)
				}
				siblingSources, err = sibling.PrepareOwnerRuntimeSources(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer siblingSources.Close()
			}
			if cut == "failed-cleanup" {
				// Original capsule includes this owning cleanup implementation before consent.
				// A failing installer-local EXIT cleanup must make the command unclosed.
				sources.Close()
				testkit.WriteFile(t, filepath.Join(root, "scripts/lib/native-installed-stage.sh"), []byte("subyard_prepared_installed_finish() { trap - EXIT; printf 'cleanup diagnostic\\n'; exit 2; }\n"), 0644)
				// Reprepare this explicitly changed fixture before executing it.
				record, e = runtime.ownerPublication.original.PrepareOwnerPublication(ctx)
				if e != nil {
					t.Fatal(e)
				}
				runtime, e = runtime.ownerPublication.original.RestoreOwnerPublication(record)
				if e != nil {
					t.Fatal(e)
				}
				prepared, e = runtime.RestoreOwnerHostPower(ctx, record.Power)
				if e != nil {
					t.Fatal(e)
				}
				defer prepared.Close()
				sources, e = runtime.PrepareOwnerRuntimeSources(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer sources.Close()
			}
			plan := &record.Consumer
			if cut == "without-plan" {
				plan = nil
			}
			err := runtime.ApplyOwnerPublicationPower(ctx, prepared, sources, plan)
			if cut == "failed-cleanup" {
				if err == nil || observer.records != 0 || runtime.OwnerPublicationInstallation() != nil || !observer.unknown {
					t.Fatal("failed cleanup published successful result/cap", err, observer, diagnostics.String())
				}
				return
			}
			if err != nil || observer.records != 1 || observer.result == nil || runtime.OwnerPublicationInstallation() == nil {
				t.Fatal("actual installer completion/result/cap", err, observer, diagnostics.String())
			}
			if observer.result.Consumer.Generations[0].Value != record.Consumer.Generation {
				t.Fatal("selected generation lost")
			}
			if siblingSources != nil {
				sibling, e = sibling.OwnerSourceRuntime(ctx, siblingSources)
				if e != nil {
					t.Fatal(e)
				}
				sibling, e = sibling.WithOwnerPublicationInstallation(ctx, runtime.OwnerPublicationInstallation())
				if e != nil {
					t.Fatal("sibling refused actual producer installation", e)
				}
				if e = sibling.ApplyOwnerPublicationPower(ctx, nil, siblingSources, nil); e != nil || observer.records != 1 {
					t.Fatal("sibling repeated shared installation", e)
				}
			}
		})
	}
}
