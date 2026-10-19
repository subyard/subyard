package cli

import (
	"context"
	"fmt"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
	"github.com/Subyard/Subyard/internal/uninstall"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in local diagnostic exercises the actual owner-child constructors
// against retained shipped source. Its Incus reader is simulated; it is not
// physical create/cleanup acceptance.
func TestNativeOwnerRegistrationOriginalGraph(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH") != "1" {
		t.Skip("whole original owner graph diagnostic")
	}
	c, home := nativeActivationSourceFixture(t)
	c.cli.options.NetworkPolicy = allowTestNetworkPolicy()
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetInstance("subyard-e2e-yard", "yard-e2e-yard", map[string]any{"name": "yard-e2e-yard", "status": "Stopped", "type": "container", "config": map[string]string{"volatile.uuid": "original-legacy-uuid"}, "expanded_config": map[string]string{"volatile.uuid": "original-legacy-uuid"}})
	c.cli.options.AdapterRunner = ownerGraphRunner{binding: nativeValueDigest(c.context.RepositoryRoot), root: c.context.RepositoryRoot, socket: server.SocketPath}
	testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "config/incus.project.env"), []byte("SRV_POOL=default\nSRV_VOLUME=yard-srv\n"), 0600)
	repository := repositoryRoot(t)
	for _, prefix := range []string{"scripts", "config/yards"} {
		err := filepath.WalkDir(filepath.Join(repository, prefix), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, e := filepath.Rel(repository, path)
			if e != nil {
				return e
			}
			dst := filepath.Join(c.context.RepositoryRoot, rel)
			if d.IsDir() {
				if d.Name() == "__pycache__" {
					return filepath.SkipDir
				}
				return os.MkdirAll(dst, 0700)
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			testkit.WriteFile(t, dst, raw, info.Mode().Perm())
			return nil
		})
		if err != nil {
			t.Fatal("original sources", err)
		}
	}
	engine := os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH_ENGINE")
	if engine == "" {
		t.Fatal("normal cmd/yard helper is required; a test executable cannot be an owner child engine")
	}
	var e error
	raw, e := os.ReadFile(engine)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(c.context.RepositoryRoot, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "bin/yard-engine"), raw, 0700)
	if e = os.MkdirAll(filepath.Join(home, "yards/e2e-yard"), 0700); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/e2e-yard/config.env"), []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\nNESTED_E2E_VMS=0\nYARD_IMAGE=images:debian/13\n"), 0600)
	toolDir := filepath.Join(c.context.RepositoryRoot, "fixture-bin")
	if e = os.Mkdir(toolDir, 0700); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(toolDir, "incus"), []byte("#!/bin/sh\nset -eu\nOWNER_MOCK_CALLS=\"${OWNER_MOCK_CALLS:-${0%/*}/calls}\"\nprintf '%s/%s\\n' \"${1:-}\" \"${2:-}\" >>\"$OWNER_MOCK_CALLS\"\ncase \"$*\" in\n 'project list --format=json') echo '[{\"name\":\"subyard-e2e-yard\"}]';;\n 'project get subyard-e2e-yard features.images') echo false;;\n 'list yard-e2e-yard --project subyard-e2e-yard --format=json') state=STOPPED; [ ! -f \"${0%/*}/running\" ] || state=RUNNING; printf '[{\"name\":\"yard-e2e-yard\",\"status\":\"%s\"}]\\n' \"$state\";;\n 'config get yard-e2e-yard user.subyard.desired_power --project subyard-e2e-yard') echo running;;\n 'info'|'info yard-e2e-yard --project subyard-e2e-yard') exit 0;;\n 'start yard-e2e-yard --project subyard-e2e-yard') touch \"${0%/*}/running\";;\n 'stop yard-e2e-yard --project subyard-e2e-yard --force') rm -f \"${0%/*}/running\";;\n '--version') echo 6.23.0;;\n *) exit 2;;\nesac\n"), 0700)
	// Keep the privileged owning leaf local: this fixture substitutes only its
	// host publication body and writes exclusively beneath its private data root.
	powerRoot := filepath.Join(c.context.DataHome, "host-power")
	if e = os.MkdirAll(powerRoot, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(c.context.RepositoryRoot, "config/systemd"), 0700); e != nil {
		t.Fatal(e)
	}
	template, e := os.ReadFile(filepath.Join(repository, "config/systemd/subyard-power-reconcile.service.in"))
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "config/systemd/subyard-power-reconcile.service.in"), template, 0644)
	contracts, e := ownerpublication.Load(c.context.RepositoryRoot)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := ownerpublication.Encode(contracts)
	if e != nil {
		t.Fatal(e)
	}
	installer := `#!/bin/bash
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cp "$SUBYARD_POWER_ENGINE_SOURCE" "$SUBYARD_POWER_RECONCILER_PATH"
chmod 0755 "$SUBYARD_POWER_RECONCILER_PATH"
sed "s|@SUBYARD_POWER_RECONCILER@|$SUBYARD_POWER_RECONCILER_PATH|g" "$SCRIPT_DIR/../config/systemd/subyard-power-reconcile.service.in" >"$SUBYARD_POWER_UNIT_PATH"
printf '\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=%s\n' "$OWNER_FIXTURE_CONTRACTS" >>"$SUBYARD_POWER_UNIT_PATH"
`
	testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "scripts/install-power-reconciler.sh"), []byte(installer), 0755)
	testkit.WriteFile(t, filepath.Join(toolDir, "systemctl"), []byte("#!/bin/sh\ncase \"$1\" in is-active) echo inactive;exit 3;; show) printf 'LoadState=loaded\\nNeedDaemonReload=no\\n';; esac\n"), 0755)
	testkit.WriteFile(t, filepath.Join(toolDir, "ip"), []byte("#!/bin/sh\nprintf '[]\\n'\n"), 0755)
	t.Setenv("PATH", toolDir+":"+os.Getenv("PATH"))
	for _, m := range []map[string]string{c.cli.env, c.cli.baseEnv} {
		delete(m, "SSH_PORT")
		m["PATH"] = os.Getenv("PATH")
		m["SUBYARD_CONFIG_HOME"] = home
		m["SUBYARD_POWER_RECONCILER_PATH"] = filepath.Join(powerRoot, "engine")
		m["SUBYARD_POWER_UNIT_PATH"] = filepath.Join(powerRoot, "unit.service")
		m["OWNER_FIXTURE_CONTRACTS"] = encoded
		m["OWNER_MOCK_CALLS"] = filepath.Join(toolDir, "calls")
	}
	c.cli.options.Incus = &ownerGraphReader{Incus: &testkit.Incus{ServerInfo: ports.ServerInfo{Version: "6.23.0"}, Instances: map[string]ports.InstanceInfo{"subyard-e2e-yard/yard-e2e-yard": {Project: "subyard-e2e-yard", Name: "yard-e2e-yard", Status: "Stopped", Config: map[string]string{"volatile.uuid": "original-legacy-uuid", "user.subyard.desired_power": "running", "user.subyard.managed": "true", "user.subyard.name": "e2e-yard", "user.subyard.bridge": "incusbr0", "user.subyard.initialized": "true", "boot.autostart": "false"}}}}}
	if _, _, e = c.CaptureInventory(context.Background()); e != nil {
		t.Fatal("inventory", e)
	}
	port := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
	observed, e := port.Prepare(context.Background(), nil)
	if e != nil {
		t.Fatal("owning before", e)
	}
	before := releaseowner.OwnerBefore{State: string(observed.State), Registration: string(observed.Registration), Overrides: string(observed.Overrides), Controller: string(observed.Controller), SharedImages: observed.SharedImages}
	if before.State == string(testyardmigration.StateAbsent) {
		t.Fatal("missing original registration")
	}
	p, e := c.prepareOwnerChildren(context.Background(), before)
	if e != nil {
		t.Fatal("original owner graph capture", e)
	}
	defer p.Close()
	t.Logf("original owner graph captured %d init scopes", len(p.inits))
	if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH_APPLY") != "1" {
		return
	}
	before.PreparedRecipe = p.binding()
	if err := c.bindOwnerChildren(context.Background(), before.PreparedRecipe, p.raw); err != nil {
		t.Fatal("original bundle inert bind", err)
	}
	_, unlock, err := uninstall.OwnerLock(context.Background(), c.context.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	observer := &ownerGraphObserver{}
	call, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx := ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(call, observer), nativeValueDigest(c.context.RepositoryRoot)), c.context.DataHome)
	if err := c.bindOwnerChildrenForApply(ctx, before.PreparedRecipe, p.raw, strings.Repeat("2", 32)); err != nil {
		t.Fatal("original token-bound handle restore", err)
	}
	if err := c.applyOwnerChildren(ctx, before, strings.Repeat("2", 32)); err != nil {
		calls, _ := os.ReadFile(filepath.Join(toolDir, "calls"))
		t.Logf("fixture command classes: %s", calls)
		t.Fatal("original owner Commit", err)
	}
}

type ownerGraphObserver struct{ commands uint64 }

func (*ownerGraphObserver) Before(ports.NativeOperation) error   { return nil }
func (*ownerGraphObserver) Accepted(ports.NativeOperation) error { return nil }
func (*ownerGraphObserver) Terminal(ports.NativeOperation) error { return nil }
func (*ownerGraphObserver) Uncertain()                           {}
func (o *ownerGraphObserver) CommandStarted(string, string) (uint64, error) {
	o.commands++
	return o.commands, nil
}
func (*ownerGraphObserver) CommandTerminal(uint64) error { return nil }
func (*ownerGraphObserver) RecordOwnerRestoration(string, string, ports.NativeOwnerMember) error {
	return nil
}
func (*ownerGraphObserver) ReadOwnerRestoration(string, string) (ports.NativeOwnerMember, error) {
	return ports.NativeOwnerMember{}, os.ErrNotExist
}

type ownerGraphReader struct{ *testkit.Incus }

func (*ownerGraphReader) OwnerCreateServer(context.Context) (string, error) {
	return strings.Repeat("c", 64), nil
}
func (*ownerGraphReader) PrepareImageProject(_ context.Context, p string, s bool) (ports.PreparedImageProject, error) {
	return ports.PreparedImageProject{Project: p, Absent: true, SharedImages: s}, nil
}
func (*ownerGraphReader) ApplyPreparedImageProject(context.Context, ports.PreparedImageProject) error {
	panic("capture effect")
}
func (*ownerGraphReader) PrepareTemporaryStop(_ context.Context, project, instance string) (ports.PreparedTemporaryStop, error) {
	return ports.PreparedTemporaryStop{Project: project, Instance: instance}, nil
}
func (*ownerGraphReader) ApplyPreparedTemporaryStop(context.Context, ports.PreparedTemporaryStop) error {
	panic("capture effect")
}
func (*ownerGraphReader) ReadOwnerImageCatalog(context.Context, string, domain.YardKind, []string, string) ([]byte, error) {
	return []byte(`[{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"container","aliases":[{"name":"debian/13"}]}]`), nil
}
func (*ownerGraphReader) PrepareOwnerImageSource(_ context.Context, r string, _ domain.YardKind, _ []string, _ string) (ports.PreparedOwnerImageSource, error) {
	return ports.PreparedOwnerImageSource{Version: 1, Reference: []byte(r), Kind: "container", Local: true, Protocol: "incus", Fingerprint: strings.Repeat("a", 64)}, nil
}
func (*ownerGraphReader) CheckOwnerImageSource(context.Context, ports.PreparedOwnerImageSource) error {
	return nil
}
func (*ownerGraphReader) PrepareOwnerVolumeGuard(_ context.Context, p ports.PreparedOwnerVolume) (ports.PreparedOwnerVolume, error) {
	p.Absent = true
	return p, nil
}

func (*ownerGraphReader) CaptureOwnerImagePolicy(_ context.Context, primary, fallback string, kind domain.YardKind, env []string, home string) (ports.PreparedOwnerImagePolicy, error) {
	k := "container"
	if kind == domain.YardVM {
		k = "virtual-machine"
	}
	return ports.PreparedOwnerImagePolicy{Version: 1, Primary: []byte(primary), Fallback: []byte(fallback), Kind: k}, nil
}
func (*ownerGraphReader) CheckOwnerImagePolicy(context.Context, ports.PreparedOwnerImagePolicy, []string, string) error {
	return nil
}
func (r *ownerGraphReader) ReadOwnerPolicyImageCatalog(ctx context.Context, p ports.PreparedOwnerImagePolicy, env []string, home string) ([]byte, error) {
	return r.ReadOwnerImageCatalog(ctx, string(p.Primary), domain.YardContainer, env, home)
}

func (r *ownerGraphReader) ReconcileState(_ context.Context, project, instance, pool, volume, bridge string) (ports.ReconcileState, error) {
	i, found := r.Instances[project+"/"+instance]
	var config map[string]string
	if found {
		config = map[string]string{"user.subyard.installation": "1"}
	}
	return ports.ReconcileState{ProjectFound: found, ProjectConfig: config, InstanceFound: found, Instance: i, HostPoolFound: true, HostNetworkFound: true}, nil
}
func (r *ownerGraphReader) TeardownInventory(_ context.Context, project string) ([]ports.TeardownResource, error) {
	var result []ports.TeardownResource
	for _, i := range r.Instances {
		if i.Project == project {
			result = append(result, ports.TeardownResource{Kind: "instance", Name: i.Name, Binding: strings.Repeat("a", 64)}, ports.TeardownResource{Kind: "project", Name: project, Binding: strings.Repeat("b", 64)})
		}
	}
	return result, nil
}
func (*ownerGraphReader) CheckOwnerVolumeGuard(context.Context, ports.PreparedOwnerVolume) error {
	return nil
}

// Only the simulated tools differ from the normal fixed shell runner. Its
// native metadata/OFD/intent/drain path still runs unchanged.
type ownerGraphRunner struct{ binding, root, socket string }

func (r ownerGraphRunner) Run(ctx context.Context, request domain.AdapterRequest, input io.Reader) (domain.AdapterResult, string, error) {
	stage := filepath.Join(fmt.Sprintf("/tmp/subyard-native-installed-%d", os.Getuid()), r.binding+"-owner-native-runtime")
	actions := lifecycleShellActions(stage)
	actions["teardown"] = map[string]shelladapter.Action{}
	for _, phase := range []string{"apply", "prepare-lock", "check-sessions", "check-profiles", "remove-shared", "remove-files", "stop-services"} {
		actions["teardown"][phase] = shelladapter.Action{Path: filepath.Join(stage, "scripts/teardown-physical.sh"), Direct: true}
	}
	request.Context["SUBYARD_INCUS_SOCKET"] = r.socket
	keys := map[string]struct{}{}
	for k := range request.Context {
		keys[k] = struct{}{}
	}
	runner := shelladapter.Runner{RepositoryRoot: stage, Path: filepath.Join(r.root, "fixture-bin") + ":/usr/bin:/bin", Actions: actions, ContextKeys: keys}
	result, diagnostic, err := runner.Run(ctx, request, input)
	if err != nil {
		var classes []string
		for _, label := range []string{"engine context", "native owner", "owner barrier", "Traceback", "incus", "missing", "unavailable", "Permission denied", "No such file", "not found", "SUBYARD_METADATA_FD", "SUBYARD_NATIVE_OWNER_FD3", "could not", "failed", "cannot", "publication", "metadata", "initialized", "VM CPU", "stopped", "NetworkManager", "start", "project", "unknown", "EOF"} {
			if strings.Contains(diagnostic, label) {
				classes = append(classes, label)
			}
		}
		return result, diagnostic, fmt.Errorf("%w: fixture diagnostic bytes=%d sha=%s classes=%v", err, len(diagnostic), nativeDigest([]byte(diagnostic)), classes)
	}
	return result, diagnostic, err
}
