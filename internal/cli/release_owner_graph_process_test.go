package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
	"golang.org/x/sys/unix"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This opt-in local diagnostic exercises the actual owner-child constructors
// against retained shipped source. Its Incus reader is simulated; it is not
// physical create/cleanup acceptance.
func TestNativeOwnerRegistrationOriginalGraph(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH") != "1" {
		t.Skip("original owner graph capture diagnostic")
	}
	c, _, _ := ownerOriginalGraphFixture(t)
	var e error
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

}

func ownerOriginalGraphFixture(t *testing.T) (*releaseOwnerComponents, string, string) {
	t.Helper()
	c, home := nativeActivationSourceFixture(t)
	c.cli.options.NetworkPolicy = allowTestNetworkPolicy()
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetInstance("subyard-e2e-yard", "yard-e2e-yard", map[string]any{"name": "yard-e2e-yard", "status": "Stopped", "type": "container", "config": map[string]string{"volatile.uuid": "original-legacy-uuid"}, "expanded_config": map[string]string{"volatile.uuid": "original-legacy-uuid"}})
	c.cli.options.AdapterRunner = ownerGraphRunner{binding: nativeValueDigest(c.context.RepositoryRoot), root: c.context.RepositoryRoot, socket: server.SocketPath}
	testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "config/incus.project.env"), []byte("SRV_POOL=default\nSRV_VOLUME=yard-srv\n"), 0600)
	repository := repositoryRoot(t)
	for _, prefix := range []string{"scripts", "config"} {
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
			// Retain synthetic fixture declarations; stage the remaining
			// shipped runtime inputs as one complete source subtree.
			if strings.HasPrefix(rel, "config/") && !strings.HasPrefix(rel, "config/yards/") {
				if _, err := os.Lstat(dst); err == nil {
					return nil
				} else if !os.IsNotExist(err) {
					return err
				}
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
	testkit.WriteFile(t, filepath.Join(toolDir, "incus"), []byte("#!/bin/sh\nset -eu\nOWNER_MOCK_CALLS=\"${OWNER_MOCK_CALLS:-${0%/*}/calls}\"\nprintf '%s/%s\\n' \"${1:-}\" \"${2:-}\" >>\"$OWNER_MOCK_CALLS\"\ncase \"$*\" in\n 'list --all-projects --format=json') cat \"${0%/*}/owner-inventory.json\";;\n 'project list --format=json') echo '[{\"name\":\"subyard-e2e-yard\"}]';;\n 'project get subyard-e2e-yard features.images') echo false;;\n 'list yard-e2e-yard --project subyard-e2e-yard --format=json') state=STOPPED; [ ! -f \"${0%/*}/running\" ] || state=RUNNING; printf '[{\"name\":\"yard-e2e-yard\",\"status\":\"%s\"}]\\n' \"$state\";;\n 'config get yard-e2e-yard user.subyard.desired_power --project subyard-e2e-yard') echo running;;\n 'info'|'info yard-e2e-yard --project subyard-e2e-yard') exit 0;;\n 'start yard-e2e-yard --project subyard-e2e-yard') touch \"${0%/*}/running\";;\n 'stop yard-e2e-yard --project subyard-e2e-yard --force') rm -f \"${0%/*}/running\";;\n '--version') echo 6.23.0;;\n *) exit 2;;\nesac\n"), 0700)
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
		m["OWNER_FIXTURE_CONTRACTS"] = encoded
		m["OWNER_MOCK_CALLS"] = filepath.Join(toolDir, "calls")
	}
	c.cli.options.Incus = &ownerGraphReader{Incus: &testkit.Incus{ServerInfo: ports.ServerInfo{Version: "6.23.0"}, Instances: map[string]ports.InstanceInfo{"subyard-e2e-yard/yard-e2e-yard": {Project: "subyard-e2e-yard", Name: "yard-e2e-yard", Status: "Stopped", Config: map[string]string{"volatile.uuid": "original-legacy-uuid", "user.subyard.desired_power": "running", "user.subyard.managed": "true", "user.subyard.name": "e2e-yard", "user.subyard.bridge": "incusbr0", "user.subyard.initialized": "true", "boot.autostart": "false"}}}}}
	inventory := []ports.InstanceInfo{}
	for _, instance := range c.cli.options.Incus.(*ownerGraphReader).Instances {
		inventory = append(inventory, instance)
	}
	payload, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(toolDir, "owner-inventory.json"), payload, 0600)
	// This is a declared shipped setting, so author it in the fixture source
	// before both original capture and fresh native configuration loading.
	host := filepath.Join(c.context.RepositoryRoot, "config/host.env")
	hostConfig, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	hostConfig = append(hostConfig, []byte(fmt.Sprintf("\nSUBYARD_POWER_LIBEXEC_DIR='%s'\nSUBYARD_POWER_RECONCILER_PATH='%s'\nSUBYARD_POWER_UNIT_PATH='%s'\n", powerRoot, filepath.Join(powerRoot, "engine"), filepath.Join(powerRoot, "unit.service")))...)
	testkit.WriteFile(t, host, hostConfig, 0600)
	// Use the same checked catalog for initial loads and original record restoration.
	// The complete fixture source has now been staged, as before production construction.
	catalog, err := config.LoadCatalog(c.context.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	c.cli.catalog = catalog
	// Original capture and a fresh native factory use the same production
	// command environment boundary after the complete catalog is available.
	c.cli = c.cli.releaseActivationOperation()
	c.catalog = &c.cli.catalog
	return c, home, toolDir
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

type ownerGraphReader struct {
	*testkit.Incus
	unmarked bool
}

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
	if found && !r.unmarked {
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

// Preparation uses the actual original child constructors and checked capsule;
// the simulated Incus reader panics on an attempted mutation.
func TestNativeOwnerProjectionRestoresOriginalChildren(t *testing.T) {
	nativeOwnerProjectionRestoresOriginalChildren(t, false)
}
func TestNativeOwnerProjectionRestoresPublishedOriginalChildren(t *testing.T) {
	nativeOwnerProjectionRestoresOriginalChildren(t, true)
}
func TestNativeOwnerProjectionAfterTerminalArchiveCleanup(t *testing.T) {
	nativeOwnerProjectionRestoresOriginalChildren(t, true, false, true)
}
func TestNativeOwnerOriginalUnmarkedLifecycleChild(t *testing.T) {
	nativeOwnerProjectionRestoresOriginalChildren(t, false, true)
}
func nativeOwnerProjectionRestoresOriginalChildren(t *testing.T, published bool, unmarked ...bool) {
	if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH") != "1" {
		t.Skip("original owner child preparation requires matching ordinary engine")
	}
	c, home, _ := ownerOriginalGraphFixture(t)
	lifecycle := len(unmarked) > 0 && unmarked[0]
	terminal := len(unmarked) > 1 && unmarked[1]
	if lifecycle {
		c.cli.options.Incus.(*ownerGraphReader).unmarked = true
	}
	if published {
		dir := filepath.Join(home, "yards", testyardmigration.LegacyYard)
		if err := os.Rename(filepath.Join(dir, "config.env"), dir+".env"); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	if _, _, err := c.CaptureInventory(ctx); err != nil {
		t.Fatal(err)
	}
	var observed releaseowner.OwnerBefore
	var p *preparedOwnerChildren
	var err error
	if lifecycle {
		observed, err = c.CaptureOwner(ctx, nil)
		if err != nil {
			t.Fatal("original owner capture", err)
		}
		generation := c.ownerChildren.inits[testyardmigration.LegacyYard].runtime.OwnerPublicationRecord().Consumer.Generation
		if _, _, err := c.CaptureInventory(ctx); err != nil {
			t.Fatal(err)
		}
		fresh, err := c.CaptureOwner(ctx, nil)
		if err != nil {
			t.Fatal("independent owner capture", err)
		}
		current := c.ownerChildren.inits[testyardmigration.LegacyYard].runtime.OwnerPublicationRecord().Consumer.Generation
		if observed.PreparedRecipe != fresh.PreparedRecipe || generation != current {
			t.Fatalf("unchanged independent owner capture changed original plan: recipe_same=%t generation_same=%t", observed.PreparedRecipe == fresh.PreparedRecipe, generation == current)
		}
		observed, p = fresh, c.ownerChildren
	} else {
		observed, err = c.CaptureOwnerObservation(ctx)
		if err != nil {
			t.Fatal("original owner observation", err)
		}
		p, err = c.prepareOwnerChildren(ctx, observed)
		if err != nil {
			t.Fatal("original child preparation", err)
		}
	}
	if p == nil || len(p.inits) != 2 {
		t.Fatal("original recreate fixture incomplete")
	}
	if lifecycle {
		current, legacy := p.inits[testyardmigration.CurrentYard].runtime.OwnerPublicationRecord(), p.inits[testyardmigration.LegacyYard].runtime.OwnerPublicationRecord()
		for _, child := range []preparedOwnerInitChild{p.inits[testyardmigration.LegacyYard], p.recreate[testyardmigration.LegacyYard]} {
			publication := child.runtime.OwnerPublicationRecord()
			if current == nil || publication == nil || current.OutputDigest != publication.OutputDigest || current.Consumer.Generation != publication.Consumer.Generation {
				t.Fatal("identical shared publication selected different generations")
			}
		}
		if legacy == nil || bytes.Equal(current.Power.Source, legacy.Power.Source) {
			t.Fatal("fixture lost distinct original child sources")
		}
		var record ownerChildrenRecord
		for _, file := range p.wire.Files {
			if file.Path == "prepared/owner-children.json" {
				if err := json.Unmarshal(file.Content, &record); err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(record.Inits) != 3 {
			t.Fatal("original child metadata absent")
		}
		child := record.Inits[1]
		if !ownerInitChildSlot(child, 1) || ownerInitChildSlot(child, 0) || ownerInitChildSlot(child, 2) {
			t.Fatal("lifecycle accepted outside original legacy slot")
		}
		withoutOriginal := child
		volume := *child.Volume
		volume.InstanceUUID = ""
		withoutOriginal.Volume = &volume
		if ownerInitChildSlot(withoutOriginal, 1) {
			t.Fatal("lifecycle accepted without original instance")
		}
		for _, raw := range [][]byte{append([]byte(" "), child.Metadata...), append(bytes.Clone(child.Metadata[:len(child.Metadata)-1]), []byte(",\"Unexpected\":true}")...)} {
			altered := child
			altered.Metadata = raw
			if _, _, err := altered.input(); err == nil {
				t.Fatal("noncanonical or unknown lifecycle metadata accepted")
			}
		}
		if p.inits[testyardmigration.LegacyYard].execution != nil {
			t.Fatal("unreachable original init plan was prepared")
		}
		if child, ok := p.recreate[testyardmigration.LegacyYard]; !ok || child.execution == nil || child.projection == nil {
			t.Fatal("original capture lost recreate child")
		}
	}
	c.ownerChildren = p
	observed.PreparedRecipe = p.binding()
	if _, err := c.ProjectActivation(ctx, observed); err != nil {
		t.Fatal("initial owner projection", err)
	}
	var originalRuntime []byte
	var originalTarget releaseowner.Target
	for _, owner := range []string{"core-guest-runtime"} {
		values, err := c.CaptureActivation(ctx, owner, false)
		if err != nil {
			t.Fatal("original reserved capture", owner, err)
		}
		originalRuntime, err = json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		reserved := false
		for _, v := range values {
			if v.Yard == testyardmigration.CurrentYard && v.AfterOwner != "" {
				reserved = true
				originalTarget = v
			}
		}
		if !reserved {
			t.Fatal("initial reserved view missing", owner)
		}
	}
	original := append([]byte(nil), p.raw...)
	if published {
		t.Run("published-original-scope", func(t *testing.T) {
			raw, err := c.InstalledRuntimeRecipe(originalTarget.PreparedRecipe)
			if err != nil {
				t.Fatal(err)
			}
			port := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
			options, err := port.options()
			if err != nil {
				t.Fatal(err)
			}
			options.RecoveryToken = strings.Repeat("1", 32)
			before := ownerMigrationBefore(observed)
			if err := testyardmigration.ApplyPreparedRegistrationAction(ctx, options, before, "publish-registration"); err != nil {
				t.Fatal("original publication", err)
			}
			if err := testyardmigration.ApplyPreparedRegistrationAction(ctx, options, before, "archive-registration"); err != nil {
				t.Fatal("original archive", err)
			}
			t.Cleanup(func() {
				if terminal {
					return
				}
				if err := testyardmigration.ApplyPreparedRegistrationAction(ctx, options, before, "restore-registration"); err != nil {
					t.Error(err)
				}
				if err := os.Remove(filepath.Join(home, "yards", testyardmigration.CurrentYard+".env")); err != nil {
					t.Error(err)
				}
			})
			fresh, _ := nativeActivationSourceFixture(t)
			fresh.cli, fresh.catalog, fresh.context, fresh.root = c.cli, c.catalog, c.context, c.root
			if _, _, err := fresh.CaptureInventory(ctx); err != nil {
				t.Fatal(err)
			}
			if err := fresh.BindInstalledRuntimeRecipe(ctx, observed.PreparedRecipe, original); err != nil {
				t.Fatal("inert original capsule", err)
			}
			reader := &ownerProjectionRestorationReader{missing: true}
			if err := fresh.BindOwnerResults(ports.WithNativeOperationObserver(ctx, reader), observed, options.RecoveryToken, nil); err != nil {
				t.Fatal("original producer binding", err)
			}
			if _, err := fresh.ProjectActivation(ctx, observed); err != nil {
				t.Fatalf("published inert projection stale=%t type=%T", errors.Is(err, domain.ErrPlanStale), err)
			}
			if fresh.ownerChildren.binding() != observed.PreparedRecipe {
				t.Fatal("projection replaced original capsule")
			}
			if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, originalTarget.PreparedRecipe, raw, originalTarget); err != nil {
				t.Fatal("same original bind after publication projection", err)
			}
			if terminal {
				incus := filepath.Join(c.context.RepositoryRoot, "fixture-bin", "incus")
				command, err := os.ReadFile(incus)
				if err != nil {
					t.Fatal(err)
				}
				command = bytes.ReplaceAll(command, []byte(`echo '[{"name":"subyard-e2e-yard"}]'`), []byte(`echo '[{"name":"subyard-test-yard"}]'`))
				command = bytes.ReplaceAll(command, []byte(`'project get subyard-e2e-yard features.images')`), []byte(`'project get subyard-e2e-yard features.images'|'project get subyard-test-yard features.images')`))
				testkit.WriteFile(t, incus, command, 0700)
				if err := c.CleanupOwner(ctx, observed, options.RecoveryToken); err != nil {
					t.Fatal("actual terminal cleanup", err)
				}
				completed, _ := nativeActivationSourceFixture(t)
				completed.cli, completed.catalog, completed.context, completed.root = c.cli, c.catalog, c.context, c.root
				if _, _, err := completed.CaptureInventory(ctx); err != nil {
					t.Fatal(err)
				}
				if err := completed.BindInstalledRuntimeRecipe(ctx, observed.PreparedRecipe, original); err != nil {
					t.Fatal(err)
				}
				if err := completed.BindOwnerResults(ports.WithNativeOperationObserver(ctx, reader), observed, options.RecoveryToken, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := completed.ProjectActivation(ctx, observed); err == nil || !strings.Contains(err.Error(), "owner registration archive is not an authorized recovery fact") {
					t.Fatalf("retired original archive guard changed: stale=%t type=%T", errors.Is(err, domain.ErrPlanStale), err)
				}
			}
		})
		if terminal {
			return
		}
	}
	var reader *ownerProjectionRestorationReader
	token := strings.Repeat("1", 32)
	if published {
		// The existing owning fixture moves the supported flat registration
		// file itself, then performs its original compensation rename. No child SDK create result is invented by this control.
		owner := newInterruptedRenameOwner(t, c.context.RepositoryRoot, home)
		owner.legacy = filepath.Join(home, "yards", testyardmigration.LegacyYard+".env")
		owner.current = filepath.Join(home, "yards", testyardmigration.CurrentYard+".env")
		if err := owner.Commit(ctx, releasetransition.OwnerRegistrationObservation{}); err != nil {
			t.Fatal(err)
		}
		// The same fixture rename helper models original compensation; its
		// returned inode/bytes remain original, with a newly recorded ctime.
		owner.legacy, owner.current = owner.current, owner.legacy
		if err := owner.Commit(ctx, releasetransition.OwnerRegistrationObservation{}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(home, "yards", testyardmigration.LegacyYard+".env")
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		source := p.registrationSource
		if source.Changed == st.Ctim {
			t.Fatal("fixture rename did not change original member ctime")
		}
		member := ports.NativeOwnerMember{Device: st.Dev, Inode: st.Ino, UID: st.Uid, GID: st.Gid, Mode: st.Mode, Links: uint64(st.Nlink), Size: st.Size, ModifiedSeconds: st.Mtim.Sec, ModifiedNanos: st.Mtim.Nsec, ChangedSeconds: st.Ctim.Sec, ChangedNanos: st.Ctim.Nsec, Digest: source.Before.RegistrationDigest}
		if err := source.ValidateRestoredMember(member); err != nil {
			t.Fatal("original owning rename member", err)
		}
		var record ownerChildrenRecord
		for _, file := range p.wire.Files {
			if file.Path == "prepared/owner-children.json" {
				if err := json.Unmarshal(file.Content, &record); err != nil {
					t.Fatal(err)
				}
			}
		}
		if record.Version != 1 {
			t.Fatal("missing original owner metadata")
		}
		reader = &ownerProjectionRestorationReader{token: token, source: nativeDigest(record.RegistrationSource), member: member}
	}
	observed.PreparedRecipe = p.binding()
	if _, _, err := c.CaptureInventory(ctx); err != nil {
		t.Fatal("fresh inventory", err)
	}
	if err := c.BindInstalledRuntimeRecipe(ctx, observed.PreparedRecipe, original); err != nil {
		t.Fatal("bind original capsule", err)
	}
	if c.needsAfterOwner(testyardmigration.CurrentYard) {
		t.Fatal("ordinary tokenless bind became executable preparation")
	}
	if published {
		if err := c.BindOwnerResults(ports.WithNativeOperationObserver(ctx, reader), observed, token, nil); err != nil {
			t.Fatal("original producer binding", err)
		}
	}
	before, err := operationReplayFiles(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProjectActivation(ctx, observed); err != nil {
		t.Fatal("project checked owner", err)
	}
	if !c.needsAfterOwner(testyardmigration.CurrentYard) {
		t.Fatal("original owner reserved views lost before first invocation")
	}
	if !published && !lifecycle {
		owned, _ := nativeActivationSourceFixture(t)
		owned.cli, owned.catalog, owned.context, owned.root = c.cli, c.catalog, c.context, c.root
		incoming := bytes.Clone(original)
		if err := owned.BindInstalledRuntimeRecipe(ctx, observed.PreparedRecipe, incoming); err != nil {
			t.Fatal("first incoming capsule", err)
		}
		incoming[0] ^= 1
		retained, err := owned.InstalledRuntimeRecipe(observed.PreparedRecipe)
		if err != nil || owned.ownerChildren.binding() != observed.PreparedRecipe || !bytes.Equal(retained, original) {
			t.Fatal("caller mutation changed retained original capsule", err)
		}
		if err := c.bindOwnerChildrenForApply(ctx, observed.PreparedRecipe, original, token); err != nil {
			t.Fatal("first original restoration", err)
		}
		first := c.ownerChildren
		if err := c.bindOwnerChildrenForApply(ctx, observed.PreparedRecipe, original, token); err != nil {
			t.Fatal("second original restoration", err)
		}
		second := c.ownerChildren
		if first.binding() != observed.PreparedRecipe || second.binding() != observed.PreparedRecipe || !bytes.Equal(first.raw, original) || !bytes.Equal(second.raw, original) {
			t.Fatal("restoration changed original capsule authority")
		}
		// Retaining physical views must not duplicate their immutable capsule.
		if &first.raw[0] != &second.raw[0] {
			t.Error("restored views retained duplicate original capsule bytes")
		}
		for i, file := range first.wire.Files {
			if len(file.Content) != 0 && &file.Content[0] != &second.wire.Files[i].Content[0] {
				t.Error("restored views retained duplicate original file bytes")
				break
			}
		}
		for _, children := range []map[string]preparedOwnerInitChild{second.inits, second.recreate} {
			for _, child := range children {
				if child.sources == nil {
					t.Fatal("restored original source descriptor missing")
				}
				if err := child.sources.ShareSources(func(payload []byte) ([]byte, error) {
					shared := c.sourceBytes[nativeDigest(payload)]
					if len(payload) > 0 && (len(shared) != len(payload) || &shared[0] != &payload[0]) {
						t.Error("restored descriptor retained duplicate approved source bytes")
					}
					return payload, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if c.ownerChildren.binding() != observed.PreparedRecipe {
		t.Fatal("projection replaced original capsule authority")
	}
	if lifecycle {
		child := c.ownerChildren.inits[testyardmigration.LegacyYard]
		if child.execution != nil || child.power == nil || child.sources == nil {
			t.Fatal("restoration lost lifecycle custody or minted init")
		}

	}
	currentRuntime, err := c.CaptureActivation(ctx, "core-guest-runtime", false)
	if err != nil {
		t.Fatal("restored reserved runtime capture", err)
	}
	currentRaw, err := json.Marshal(currentRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if nativeDigest(originalRuntime) != nativeDigest(currentRaw) {
		t.Fatal("restored runtime source or structure changed")
	}
	after, err := operationReplayFiles(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("projection changed protected input count")
	}
	for name, raw := range before {
		if nativeDigest(raw) != nativeDigest(after[name]) {
			t.Fatal("projection changed protected input", name)
		}
	}

	if lifecycle {
		generation := c.ownerChildren.inits[testyardmigration.LegacyYard].runtime.OwnerPublicationRecord().Consumer.Generation
		path := filepath.Join(c.context.RepositoryRoot, "config/systemd/subyard-publication-reconcile.service.in")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, append(raw, []byte("\n# changed approved publication source\n")...), info.Mode().Perm())
		if _, _, err := c.CaptureInventory(ctx); err != nil {
			t.Fatal(err)
		}
		err = c.BindInstalledRuntimeRecipe(ctx, observed.PreparedRecipe, original)
		if err == nil {
			_, err = c.ProjectActivation(ctx, observed)
		}
		if err == nil {
			t.Fatal("retained generation hid changed original publication source")
		}
		if _, _, err := c.CaptureInventory(ctx); err != nil {
			t.Fatal(err)
		}
		changed, err := c.CaptureOwner(ctx, nil)
		if err != nil {
			t.Fatal("capture changed publication source", err)
		}
		current := c.ownerChildren.inits[testyardmigration.LegacyYard].runtime.OwnerPublicationRecord().Consumer.Generation
		if changed.PreparedRecipe == observed.PreparedRecipe || generation == current {
			t.Fatal("changed publication source reused original authority")
		}
	}
	if published {
		late := *reader
		late.missing = true
		if err := c.BindOwnerResults(ports.WithNativeOperationObserver(ctx, &late), observed, token, nil); err != nil {
			t.Fatal("later absent producer", err)
		}
		if c.ownerRecoveryObserver != reader {
			t.Fatal("later absence replaced original witness")
		}
		if _, err := c.ProjectActivation(ctx, observed); err != nil {
			t.Fatal("original lineage projection", err)
		}
		for _, fault := range []string{"cross-token", "present-foreign", "present-corrupt", "incomplete-reader", "empty-token", "nonhex-token"} {
			t.Run(fault, func(t *testing.T) {
				next := *reader
				currentToken := token
				if fault == "cross-token" {
					currentToken = strings.Repeat("2", 32)
				}
				if fault == "empty-token" {
					currentToken = ""
				}
				if fault == "nonhex-token" {
					currentToken = strings.Repeat("z", 32)
				}
				if fault == "present-foreign" {
					next.source = strings.Repeat("b", 64)
				}
				next.corrupt = fault == "present-corrupt"
				var observer ports.NativeOperationObserver = &next
				if fault == "incomplete-reader" {
					observer = &ownerResultTestReader{}
				}
				snapshot, err := operationReplayFiles(home)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.BindOwnerResults(ports.WithNativeOperationObserver(ctx, observer), observed, currentToken, nil); err == nil {
					t.Fatal("unbound later producer admitted")
				}
				if c.ownerRecoveryObserver != reader || c.ownerRecoveryToken != token {
					t.Fatal("refusal replaced original recovery context")
				}
				after, err := operationReplayFiles(home)
				if err != nil {
					t.Fatal(err)
				}
				if len(after) != len(snapshot) {
					t.Fatal("producer refusal changed protected paths")
				}
				for path, raw := range snapshot {
					if nativeDigest(raw) != nativeDigest(after[path]) {
						t.Fatal("producer refusal changed protected input", path)
					}
				}
			})
		}
		reader.missing = true
		if _, err := c.ProjectActivation(ctx, observed); err == nil {
			t.Fatal("prepared descriptions ignored lost selected restoration fact")
		}
		reader.missing = false
		if reader.mutations != 0 {
			t.Fatal("projection attempted native creation")
		}
		if reader.reads < 2 {
			t.Fatal("restoration fact was not reread for projection")
		}
		for _, fault := range []string{"missing-reader", "missing-fact", "foreign-token", "foreign-source", "foreign-member"} {
			t.Run(fault, func(t *testing.T) {
				if _, _, err := c.CaptureInventory(ctx); err != nil {
					t.Fatal(err)
				}
				if c.ownerRecoveryObserver != nil || c.ownerRecoveryToken != "" || c.ownerRecoverySource != "" {
					t.Fatal("inventory retained recovery context")
				}
				if err := c.BindInstalledRuntimeRecipe(ctx, observed.PreparedRecipe, original); err != nil {
					t.Fatal(err)
				}
				bad := *reader
				bad.missing = fault == "missing-fact"
				if fault == "foreign-source" {
					bad.source = strings.Repeat("b", 64)
				}
				if fault == "foreign-member" {
					bad.member.Inode++
				}
				currentToken := token
				if fault == "foreign-token" {
					currentToken = strings.Repeat("2", 32)
				}
				var observer ports.NativeOperationObserver = &bad
				if fault == "missing-reader" {
					observer = &ownerResultTestReader{}
				}
				snapshots, err := operationReplayFiles(home)
				if err != nil {
					t.Fatal(err)
				}
				err = c.BindOwnerResults(ports.WithNativeOperationObserver(ctx, observer), observed, currentToken, nil)
				if err == nil {
					_, err = c.ProjectActivation(ctx, observed)
				}
				if bad.mutations != 0 {
					t.Fatal("refusal attempted native creation")
				}
				if err == nil {
					t.Fatal("unbound restoration admitted")
				}
				final, err := operationReplayFiles(home)
				if err != nil {
					t.Fatal(err)
				}
				if len(final) != len(snapshots) {
					t.Fatal("refusal changed protected pathset")
				}
				for name, raw := range snapshots {
					if nativeDigest(raw) != nativeDigest(final[name]) {
						t.Fatal("refusal changed protected input", name)
					}
				}
			})
		}
	}
}

// The result/session boundary has separate native fact controls. This reader
// models only its checked original registration member for the owning rename.
type ownerProjectionRestorationReader struct {
	ownerResultTestReader
	token, source string
	member        ports.NativeOwnerMember
	reads         int
	missing       bool
	corrupt       bool
	mutations     int
}

func (*ownerProjectionRestorationReader) RecordOwnerRestoration(string, string, ports.NativeOwnerMember) error {
	return errors.New("readonly original restoration")
}
func (r *ownerProjectionRestorationReader) ReadOwnerRestoration(token, source string) (ports.NativeOwnerMember, error) {
	r.reads++
	if r.corrupt {
		return ports.NativeOwnerMember{}, errors.New("noncanonical original restoration fact")
	}
	if r.missing {
		return ports.NativeOwnerMember{}, ports.ErrNativeOwnerRestorationAbsent
	}
	if token != r.token || source != r.source {
		return ports.NativeOwnerMember{}, domain.ErrPlanStale
	}
	return r.member, nil
}

func (r *ownerProjectionRestorationReader) BeforeOwnerCreate(ports.NativeOwnerCreateIntent) (ports.NativeOwnerCreateRef, error) {
	r.mutations++
	return ports.NativeOwnerCreateRef{}, errors.New("unexpected projection create")
}
func (r *ownerProjectionRestorationReader) RecordOwnerCreated(ports.NativeOwnerCreated) error {
	r.mutations++
	return errors.New("unexpected projection create result")
}

func TestNativeOwnerProjectedHostPowerUsesTargetYards(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH") != "1" {
		t.Skip("original owner graph requires matching ordinary engine")
	}
	c, _, _ := ownerOriginalGraphFixture(t)
	// Applicability is an independent host fact; use an existing fixture file.
	c.cli.env["SUBYARD_POWER_UNIT_PATH"] = filepath.Join(c.context.RepositoryRoot, "config/systemd/subyard-power-reconcile.service.in")
	if _, _, err := c.CaptureInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := c.CaptureOwner(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProjectActivation(t.Context(), before); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CaptureActivation(t.Context(), "host-power", false); err != nil {
		t.Fatal("projected host power read original canonical registration", err)
	}
	if _, err := c.CaptureActivation(t.Context(), "host-power", true); err == nil {
		t.Fatal("fresh host power hid absent actual canonical registration")
	}
}
