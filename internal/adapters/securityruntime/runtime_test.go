package securityruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestSecurityRuntimeRejectsStaticSocketMountWithoutHostAccess(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["HOST_MOUNTS"] = "daemon:/run/docker.sock:rw:0755"
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected contract failure, got %v", err)
	}
}

func TestSecurityRuntimeValidatesLivePolicyWithoutIncusHost(t *testing.T) {
	runtime := testRuntime(t)
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return safeState(), true, nil
	}
	state, err := runtime.CheckSecurity(context.Background(), true, true)
	if err != nil || state != "live" {
		t.Fatalf("state=%q err=%v", state, err)
	}
}

func TestSecurityRuntimeRejectsManagedDiskOutsideHostBase(t *testing.T) {
	runtime := testRuntime(t)
	state := safeState()
	state.Instance.Devices["host-source"] = map[string]string{
		"type": "disk", "source": "/etc", "path": "/mnt/host/source",
	}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	_, err := runtime.CheckSecurity(context.Background(), true, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected contract failure, got %v", err)
	}
}

func TestSecurityRuntimeRejectsProfileSocketMount(t *testing.T) {
	runtime := testRuntime(t)
	profile := filepath.Join(runtime.RepositoryRoot, "config", "profiles", "unsafe")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "profile.conf"),
		[]byte(`ENV_MOUNTS="/var/run/docker.sock:/var/run/docker.sock"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected profile socket failure, got %v", err)
	}
}

func TestSecurityRuntimeRejectsUnsupportedAndDisabledNestedDevices(t *testing.T) {
	for name, source := range map[string]string{
		"unsupported": "/dev/mem",
		"disabled":    "/dev/vsock",
	} {
		t.Run(name, func(t *testing.T) {
			runtime := testRuntime(t)
			state := safeState()
			state.Instance.Devices["fixture"] = map[string]string{"type": "unix-char", "source": source}
			runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
				return state, true, nil
			}
			_, err := runtime.CheckSecurity(context.Background(), true, true)
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected unix-char failure, got %v", err)
			}
		})
	}
}

func TestSecurityRuntimeAcceptsNestedDevicePolicy(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Yard.NestedE2EVMs = true
	state := safeState()
	state.ProjectConfig["restricted.containers.interception"] = "allow"
	state.Instance.LocalConfig["security.syscalls.intercept.bpf"] = "true"
	state.Instance.LocalConfig["security.syscalls.intercept.bpf.devices"] = "true"
	state.Instance.Devices["vsock"] = map[string]string{"type": "unix-char", "source": "/dev/vsock"}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	if _, err := runtime.CheckSecurity(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityRuntimeWarnsForExplicitDiskOutsideHostBase(t *testing.T) {
	runtime := testRuntime(t)
	state := safeState()
	state.Instance.Devices["fixture"] = map[string]string{
		"type": "disk", "source": "/etc", "path": "/workspace",
	}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	if _, err := runtime.CheckSecurity(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "encapsulation is reduced") {
		t.Fatalf("missing explicit-disk warning: %q", diagnostics.String())
	}
}

func TestSecurityRuntimeRequiresPrivateIdentityMode(t *testing.T) {
	runtime := testRuntime(t)
	root := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(filepath.Join(root, "identity"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "identity", "age.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime.Environment["SUBYARD_KEYS_ROOT"] = root
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) || !strings.Contains(diagnostics.String(), "mode 0600") {
		t.Fatalf("expected identity-mode failure, err=%v output=%q", err, diagnostics.String())
	}
}

func TestSecurityRuntimeRejectsLedgerUnderHostBase(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["SUBYARD_KEYS_ROOT"] = filepath.Join(runtime.Yard.Paths.HostBase, "keys")
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected ledger boundary failure, got %v", err)
	}
}

func testRuntime(t *testing.T) Runtime {
	t.Helper()
	root := t.TempDir()
	operator := t.TempDir()
	profiles := filepath.Join(root, "config", "profiles")
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	return Runtime{
		RepositoryRoot: root,
		Environment:    map[string]string{"SUBYARD_SECURITY_SKIP_LIVE": "1"},
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard",
			Paths: domain.RuntimePaths{
				ConfigHome: filepath.Join(operator, "config-home"),
				HostBase:   filepath.Join(operator, "host"),
			},
		},
	}
}

func safeState() ports.ReconcileState {
	return ports.ReconcileState{
		ProjectFound: true,
		ProjectConfig: map[string]string{
			"restricted":                         "true",
			"restricted.containers.privilege":    "unprivileged",
			"restricted.containers.interception": "block",
		},
		InstanceFound: true,
		Instance: ports.InstanceInfo{
			Config:       map[string]string{},
			LocalConfig:  map[string]string{},
			Devices:      map[string]map[string]string{},
			LocalDevices: map[string]map[string]string{},
		},
	}
}
