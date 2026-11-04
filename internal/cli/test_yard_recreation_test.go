package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func TestTestYardRecreationGuidanceIncludesRequiredGitMigration(t *testing.T) {
	for _, state := range []string{"no-git", "legacy", "registered", "migrated", "malformed"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			home := filepath.Join(root, "config")
			if err := os.MkdirAll(filepath.Join(home, ".sync"), 0700); err != nil {
				t.Fatal(err)
			}
			manifest := configsync.Manifest{SchemaVersion: 1, Generation: 1, SourceID: strings.Repeat("a", 64), SourceCommit: strings.Repeat("b", 40),
				HostID: "owner-a", SourceSchema: 1, SourceDigest: strings.Repeat("c", 64), Files: []configsync.ManagedFile{{Path: "yards/e2e-yard/config.env", Digest: strings.Repeat("d", 64), Mode: 0600, Generation: 1}}}
			if state == "migrated" {
				manifest.SchemaVersion = 2
				manifest.Files[0].Path = config.GitSettingsRelativePath + "/" + manifest.Files[0].Path
			}
			payload, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if state == "malformed" {
				payload = []byte("not-json")
			}
			path := filepath.Join(home, ".sync/manifest.json")
			if state != "no-git" {
				testkit.WriteFile(t, path, payload, 0600)
			}
			if state == "registered" {
				if err := configsync.RegisterSource(home, filepath.Join(root, "source")); err != nil {
					t.Fatal(err)
				}
			}
			hint, err := testyardmigration.RecreationHint(testyardmigration.Options{ConfigHome: home, DataHome: filepath.Join(root, "data")}, true)
			if state == "malformed" {
				if err == nil || hint != "" {
					t.Fatal("malformed ownership became recreation advice")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			base := "yard -Y e2e-yard teardown --reset-config, then yard service install test-vms"
			if !strings.Contains(hint, base) {
				t.Fatalf("missing executable recreation steps: %s", hint)
			}
			switch state {
			case "legacy":
				if !strings.Contains(hint, "yard config sync <checkout>, then "+base) || !strings.Contains(hint, "restore") {
					t.Fatalf("unregistered legacy source advice: %s", hint)
				}
			case "registered":
				if !strings.Contains(hint, "yard config sync, then "+base) || strings.Contains(hint, "<checkout>") {
					t.Fatalf("registered source advice: %s", hint)
				}
			default:
				if strings.Contains(hint, "config sync") {
					t.Fatalf("unnecessary settings migration: %s", hint)
				}
			}
			if state != "no-git" {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(payload) {
					t.Fatal("guidance rewrote ownership")
				}
			}
		})
	}
}

func TestReleasePreservesLegacyTestOwnerAndOrdinaryTargetScope(t *testing.T) {
	root, env, _ := nativeFixture(t)
	home := environmentValue(env, "SUBYARD_CONFIG_HOME")
	writeConfigCommandFile(t, filepath.Join(root, "config/yards/profiles/test-vms.env"), "NESTED_E2E_VMS=1\n")
	legacy := filepath.Join(home, "yards/e2e-yard/config.env")
	content := "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"
	writeConfigCommandFile(t, legacy, content)
	writeConfigCommandFile(t, filepath.Join(home, "yards/named/config.env"), "SSH_PORT=2233\n")
	program, err := New(Options{RepositoryRoot: root, Environment: env})
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{ConfigHome: home}
	warning, err := program.ownerRegistrationTransition(request).(*ownerRegistrationTransition).FreshWarning(context.Background(), nil)
	if err != nil || !strings.Contains(warning, "yard -Y e2e-yard teardown --reset-config, then yard service install test-vms") || strings.Contains(warning, "service remove") {
		t.Fatalf("unexecutable legacy guidance: %q %v", warning, err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	targets, err := program.releaseConfigTargets(context.Background(), loaded, true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, target := range targets {
		names = append(names, target.Name)
	}
	if !slices.Equal(names, []string{"default", "named"}) {
		t.Fatalf("release changed ordinary target scope: %v", names)
	}
	configTargets, err := program.localConfigTargets(loaded, true)
	if err != nil || len(configTargets) != 3 {
		t.Fatalf("ordinary config inspection hid retained owner: %v %v", configTargets, err)
	}
	actor := program.routeConsumerActivationReconciler(request)
	observation, err := actor.Observe(context.Background(), releasetransition.ReleasePair{Target: "release-b"}, releasetransition.ReleaseLinks{})
	if err != nil || !observation.Converged || len(observation.Warnings) != 1 {
		t.Fatalf("legacy owner blocked activation: %+v %v", observation, err)
	}
	if err := actor.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(legacy); err != nil || string(got) != content {
		t.Fatal("release changed legacy registration", err)
	}
	if _, err := os.Stat(filepath.Join(home, "yards/test-yard")); !os.IsNotExist(err) {
		t.Fatal("release created replacement owner", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := program.releaseTestYardRecreation(ctx, home, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation suppressed: %v", err)
	}
}

func TestCanonicalTestOwnerStaysApplicableWithAndWithoutServiceRegistration(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "unregistered", true: "registered"}[registered], func(t *testing.T) {
			root, env, _ := nativeFixture(t)
			home := environmentValue(env, "SUBYARD_CONFIG_HOME")
			path := filepath.Join(home, "yards/test-yard/config.env")
			writeConfigCommandFile(t, path, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
			incus := filepath.Join(root, "incus")
			writeCLIFile(t, incus, `#!/bin/sh
case "$*" in
 'project list --format=json') printf '[{"name":"subyard-test-yard"}]\n';;
 'list --all-projects --format=json') printf '[{"name":"yard-test-yard","project":"subyard-test-yard","status":"RUNNING","config":{"user.subyard.name":"test-yard","user.subyard.managed":"true"}}]\n';;
 *) exit 92;;
esac
`, 0700)
			options := testyardmigration.Options{Executable: filepath.Join(root, "yard"), ConfigHome: home, DataHome: filepath.Join(root, "data"), Incus: incus}
			if err := os.MkdirAll(options.DataHome, 0700); err != nil {
				t.Fatal(err)
			}
			if registered {
				record := ownerservice.Registration{Public: ownerservice.Public{SchemaVersion: 1, Owner: "local", Service: "test-vms", Pool: "main", Yard: "test-yard", Project: "subyard-test-yard", Instance: "yard-test-yard", UUID: "uuid-current", Generation: strings.Repeat("a", 64)}, Ownership: strings.Repeat("b", 64)}
				if err := (ownerservice.Registry{Root: filepath.Join(options.DataHome, "services")}).Save(record); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			warning, err := testYardRecreation(context.Background(), options)
			if err != nil || warning != "" {
				t.Fatalf("canonical owner excluded: %q %v", warning, err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("canonical owner changed", err)
			}
			// A confirmed identity conflict warns; an uncertain transport remains blocking.
			writeCLIFile(t, incus, "#!/bin/sh\nexit 93\n", 0700)
			if warning, err := testYardRecreation(context.Background(), options); err == nil || warning != "" {
				t.Fatalf("transport suppressed: %q %v", warning, err)
			}
		})
	}
}

func installCanonicalOwnerInventory(t *testing.T, root string) {
	t.Helper()
	bin := filepath.Join(root, "test-owner-bin")
	writeConfigCommandFile(t, filepath.Join(bin, "incus"), `#!/bin/sh
case "$*" in
 'project list --format=json') printf '[{"name":"subyard-test-yard"}]\n';;
 'list --all-projects --format=json') printf '[{"name":"yard-test-yard","project":"subyard-test-yard","status":"RUNNING","config":{"user.subyard.name":"test-yard","user.subyard.managed":"true"}}]\n';;
 *) exit 94;;
esac
`, 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestReservedYardInitNamesConfigurationAndRepair(t *testing.T) {
	for _, yard := range []string{"e2e-yard", "test-yard"} {
		t.Run(yard, func(t *testing.T) {
			root, env, _ := nativeFixture(t)
			home := environmentValue(env, "SUBYARD_CONFIG_HOME")
			path := filepath.Join(home, "yards", yard, "config.env")
			writeConfigCommandFile(t, path, "SSH_PORT=2240\n")
			program, err := New(Options{RepositoryRoot: root, Environment: env})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext(yard)
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("init")
			prepared := &preparedCommand{CLI: program, Loaded: loaded, Definition: definition}
			err = prepared.prepareInit(context.Background(), nil)
			for _, required := range []string{"reserved yard name " + yard, "yards/" + yard + "/config.env", "another yard name", "set YARD_TEMPLATE=test-vms"} {
				if err == nil || !strings.Contains(err.Error(), required) {
					t.Fatalf("init lost public refusal %q: %v", required, err)
				}
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "SSH_PORT=2240\n" {
				t.Fatal("refusal changed registration", err)
			}
		})
	}
}
