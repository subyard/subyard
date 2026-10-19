package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func shippedPreparedSourcesFixture(t *testing.T) Runtime {
	t.Helper()
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	original := Runtime{RepositoryRoot: source}
	_, paths, err := original.hostInstallerSourcePaths("host-power")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	for _, relative := range paths {
		payload, err := os.ReadFile(filepath.Join(source, relative))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(source, relative))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, payload, info.Mode().Perm())
	}
	definitions, err := profile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		for _, native := range definition.Native {
			relative := filepath.Join("config/profiles", definition.Name, native.Path)
			if _, err := os.Lstat(filepath.Join(source, relative)); os.IsNotExist(err) {
				relative = filepath.Join(".build/profiles", definition.Name, native.Path)
			}
			body, err := os.ReadFile(filepath.Join(source, relative))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, body, 0755)
		}
	}
	// This source is an explicit owning shared dependency outside config/scripts.
	relative := "internal/adapters/configmaterial/tomli_w.py"
	body, err := os.ReadFile(filepath.Join(source, relative))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, relative), body, 0644)
	home := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(home, "tools/bin"), 0700); err != nil {
		t.Fatal(err)
	}
	return Runtime{RepositoryRoot: root, Profiles: definitions, Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: home, ConfigHome: home, OperatorHome: home}}, Environment: []string{"ALLOWS_CODING_TOOLS=false", "SUBYARD_KEYS_TOOLS_DIR=" + filepath.Join(home, "tools")}}
}

func TestShippedPreparedSourceBoundsAndExactRebind(t *testing.T) {
	runtime := shippedPreparedSourcesFixture(t)
	constructors := map[string]func() (*PreparedInstalledRuntime, error){}
	for _, owner := range []string{"core-guest-runtime", "key-tools-runtime"} {
		constructors[owner] = func() (*PreparedInstalledRuntime, error) {
			return runtime.PrepareInstalledRuntime(context.Background(), owner)
		}
	}
	for _, owner := range []string{"host-power", "test-vms-host-sink"} {
		constructors[owner] = func() (*PreparedInstalledRuntime, error) {
			return runtime.PrepareHostInstallerSources(context.Background(), owner)
		}
	}
	for _, definition := range runtime.Profiles {
		if definition.Runtime != nil {
			owner := definition.Runtime.ActivationID
			constructors[owner] = func() (*PreparedInstalledRuntime, error) {
				return runtime.PrepareProfileRuntimeSources(context.Background(), owner)
			}
		}
	}
	for name, constructor := range constructors {
		t.Run(name, func(t *testing.T) {
			p, err := constructor()
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			total := 0
			for _, payload := range p.files {
				total += len(payload)
			}
			raw := p.Recipe()
			t.Logf("files=%d raw=%d canonical=%d sha256=%s", len(p.files), total, len(raw), p.Binding())
			if total > 32<<20 || len(raw) > MaxInstalledRecipe {
				t.Fatal("owning source bound exceeded")
			}
			restored, err := runtime.BindInstalledRuntime(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if restored.Binding() != p.Binding() {
				t.Fatal("recipe identity changed")
			}
		})
	}
}

func TestPreparedHostSourceRetainsDeclarationsAndRejectsDrift(t *testing.T) {
	runtime := shippedPreparedSourcesFixture(t)
	p, err := runtime.PrepareHostInstallerSources(context.Background(), "host-power")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.materialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerpublication.Load(p.directory); err != nil {
		t.Fatal("cached declaration reader cannot load", err)
	}
	original := p.Binding()
	// Installed effects are outside immutable source scope.
	testkit.WriteFile(t, filepath.Join(runtime.Yard.Paths.DataHome, "installed-unit"), []byte("installed"), 0644)
	current, err := runtime.PrepareHostInstallerSources(context.Background(), "host-power")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if current.Binding() != original {
		t.Fatal("installed state changed immutable recipe")
	}
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "config/release-transition.json"), []byte("changed"), 0600)
	if _, err := runtime.BindInstalledRuntime(p.Recipe()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed original declaration rebound", err)
	}
	if _, err := ownerpublication.Load(p.directory); err != nil {
		t.Fatal("original declaration bytes not retained", err)
	}
}

func TestPreparedHostSourceRealSealedPublicationReader(t *testing.T) {
	enginePath := os.Getenv("SUBYARD_NATIVE_REAL_ENGINE")
	if enginePath == "" {
		t.Skip("explicit actual Go engine preflight")
	}
	runtime := shippedPreparedSourcesFixture(t)
	p, err := runtime.PrepareHostInstallerSources(context.Background(), "host-power")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.materialize(); err != nil {
		t.Fatal(err)
	}
	engine, err := os.Open(enginePath)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	// SealPreparedEngine authenticates the exact bytes using the existing owning digest.
	raw, err := os.ReadFile(enginePath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := testvmsruntime.SealPreparedEngine(engine, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	home := runtime.Yard.Paths.ConfigHome
	run := func() error {
		command := exec.Command("/proc/self/fd/3", "_publication-contracts")
		command.ExtraFiles = []*os.File{sealed}
		command.Env = append(os.Environ(), "SUBYARD_REPOSITORY_ROOT="+p.directory, "SUBYARD_CONFIG_HOME="+home)
		output, err := command.CombinedOutput()
		if err == nil {
			if _, err = ownerpublication.Decode(string(bytes.TrimSpace(output))); err != nil {
				return err
			}
		}
		return err
	}
	if err := run(); err == nil {
		t.Fatal("absent protected epoch admitted")
	}
	registryRaw, err := os.ReadFile(filepath.Join(p.directory, "config/release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := releasetransition.ParseRegistryV2(registryRaw, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(registry)
	for _, migration := range registry.Migrations {
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "release-transition/v2/ledger.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	payload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, payload, 0600)
	if err := run(); err != nil {
		t.Fatal("actual sealed reader rejected complete cached declarations", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("actual sealed reader accepted unsafe protected epoch")
	}
}

func TestPreparedHostExecutionUsesApprovedWrapperAndTemplate(t *testing.T) {
	runtime := shippedPreparedSourcesFixture(t)
	home := runtime.Yard.Paths.DataHome
	source := filepath.Join(home, "source-engine")
	installed := filepath.Join(home, "installed-engine")
	unit := filepath.Join(home, "installed-unit")
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, source, []byte("approved sealed engine"), 0755)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncase \"$1\" in show) printf 'LoadState=loaded\\nNeedDaemonReload=no\\n';; esac\n"), 0755)
	contracts, err := ownerpublication.Load(runtime.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ownerpublication.Encode(contracts)
	if err != nil {
		t.Fatal(err)
	}
	originalScript := filepath.Join(runtime.RepositoryRoot, "scripts/install-power-reconciler.sh")
	originalTemplate := filepath.Join(runtime.RepositoryRoot, "config/systemd/subyard-power-reconcile.service.in")
	script := `#!/bin/bash
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
printf 'unapproved source changed after guard' >"$ORIGINAL_TEMPLATE"
printf '#!/bin/sh\nexit 91\n' >"$ORIGINAL_SCRIPT"
cp "$SUBYARD_POWER_ENGINE_SOURCE" "$SUBYARD_POWER_RECONCILER_PATH"
chmod 0755 "$SUBYARD_POWER_RECONCILER_PATH"
sed "s|@SUBYARD_POWER_RECONCILER@|$SUBYARD_POWER_RECONCILER_PATH|g" "$SCRIPT_DIR/../config/systemd/subyard-power-reconcile.service.in" >"$SUBYARD_POWER_UNIT_PATH"
printf '\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=%s\n' "$CONTRACTS" >>"$SUBYARD_POWER_UNIT_PATH"
`
	testkit.WriteFile(t, originalScript, []byte(script), 0755)
	runtime.Environment = append(runtime.Environment, "PATH="+bin+":/usr/bin:/bin", "SUBYARD_USER="+account.Username, "SUBYARD_POWER_ENGINE_SOURCE="+source, "SUBYARD_POWER_RECONCILER_PATH="+installed, "SUBYARD_POWER_UNIT_PATH="+unit, "ORIGINAL_TEMPLATE="+originalTemplate, "ORIGINAL_SCRIPT="+originalScript, "CONTRACTS="+encoded)
	prepared, err := runtime.PrepareHostPowerRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	sources, err := runtime.PrepareHostInstallerSources(context.Background(), "host-power")
	if err != nil {
		t.Fatal(err)
	}
	defer sources.Close()
	expected, err := runtime.CaptureHostPowerCustody()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyPreparedHostPowerRuntimeCustodySources(context.Background(), prepared, sources, expected, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustReadPreparedTest(t, unit)), "SUBYARD_PUBLICATION_CONTRACTS=") {
		t.Fatal("cached approved template not consumed")
	}
	if bytes.Equal(mustReadPreparedTest(t, unit), mustReadPreparedTest(t, originalTemplate)) {
		t.Fatal("changed template executed")
	}
	if _, err := os.Stat(sources.directory); !os.IsNotExist(err) {
		t.Fatal("drained execution stage retained", err)
	}
}

func mustReadPreparedTest(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
