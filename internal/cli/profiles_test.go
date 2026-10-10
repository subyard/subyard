package cli

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type profileRuntimeFixture struct {
	installed bool
	fail      bool
	applied   []string
}
type profileRuntimeSelection struct {
	fixture  *profileRuntimeFixture
	selected bool
}

func (runtime profileRuntimeSelection) ProfileServiceConverged(context.Context, string) (bool, error) {
	return runtime.fixture.installed == runtime.selected, nil
}
func (runtime profileRuntimeSelection) ApplyProfileService(_ context.Context, name string, _ []ports.TeardownArtifact) error {
	runtime.fixture.applied = append(runtime.fixture.applied, name)
	if runtime.fixture.fail {
		return errors.New("fixture apply failure")
	}
	runtime.fixture.installed = runtime.selected
	return nil
}

func scopedProfileFixture(t *testing.T, selection string) (*CLI, *testkit.Incus, *profileRuntimeFixture, string, *bytes.Buffer) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	manifest := filepath.Join(root, "config", "commands.registry")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, manifest, string(data)+"profile||@profile||deny|mutate|dynamic|public|lifecycle|profile|profile <command> <id>|profiles|--json --yes --help|enable disable setup status\n", 0600)
	writeProfileTestFile(t, filepath.Join(root, "config", "profiles", "fixture", "profile.json"), []byte(`{"schema_version":1,"default_yards":["default"],"owner_service":"owner.sh","managed_paths":[{"root":"data","path":"profile-fixture/{yard}"}]}`), 0600)
	writeProfileTestFile(t, filepath.Join(root, "config", "profiles", "fixture", "owner.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	path := filepath.Join(root, "state", "yards", "default", "config.env")
	writeProfileTestFile(t, path, []byte(selection), 0600)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runtime := &profileRuntimeFixture{}
	platform := newInitPlatformFixture()
	var output bytes.Buffer
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, InitPlatform: platform, Stdin: strings.NewReader(""), Stdout: &output, Stderr: &output, ProfileRuntime: func(loaded config.Loaded) ProfileServiceRuntime {
		selected := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])
		_, explicit := loaded.Environment["ENVIRONMENT_PROFILES"]
		return profileRuntimeSelection{runtime, !explicit || slices.Contains(selected, "fixture")}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return cli, incus, runtime, path, &output
}

func prepareScopedProfile(t *testing.T, cli *CLI, arguments ...string) (*preparedCommand, error) {
	t.Helper()
	cli.env = maps.Clone(cli.baseEnv)
	loaded, err := cli.loadContext("default")
	if err != nil {
		return nil, err
	}
	definition, _ := cli.manifest.Lookup("profile")
	return cli.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments, ExplicitYard: true, InteractiveSetup: true})
}

func TestScopedProfileEnableDisablePreservesNeighborsAndSkipsInit(t *testing.T) {
	cli, _, runtime, path, output := scopedProfileFixture(t, "ENVIRONMENT_PROFILES='neighbor'\nSSH_PORT=2222\n")
	for _, verb := range []string{"enable", "enable", "disable", "disable", "enable"} {
		prepared, err := prepareScopedProfile(t, cli, verb, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		code := cli.runPreparedCommand(context.Background(), prepared, true)
		prepared.Close()
		if code != 0 {
			t.Fatalf("%s exit=%d: %s", verb, code, output)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "neighbor fixture") || !strings.Contains(string(data), "SSH_PORT=2222") {
		t.Fatalf("selection lost: %s", data)
	}
	if strings.Join(runtime.applied, " ") != "fixture fixture fixture" {
		t.Fatalf("scoped apply=%v", runtime.applied)
	}
	if platform := cli.options.InitPlatform.(*initPlatformFixture); len(platform.applied) != 0 {
		t.Fatalf("entered init: %+v", platform)
	}
	prepared, err := prepareScopedProfile(t, cli, "status", "fixture", "--json")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 || !strings.Contains(output.String(), `"ready":true`) {
		t.Fatalf("status=%d %s", code, output)
	}
}

func TestScopedProfileDisabledGuardRejectsEnableBeforeWrite(t *testing.T) {
	for _, selection := range []string{"", "fixture"} {
		t.Run("selection="+selection, func(t *testing.T) {
			cli, _, runtime, path, _ := scopedProfileFixture(t, "ENVIRONMENT_PROFILES="+selection+"\nSSH_PORT=2222\n")
			writeProfileTestFile(t, filepath.Join(cli.options.RepositoryRoot, "config", "profiles", "fixture", "profile.json"), []byte(`{"schema_version":1,"owner_service":"owner.sh","disabled_when":{"SSH_PORT":"2222"}}`), 0600)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepareScopedProfile(t, cli, "enable", "fixture"); err == nil {
				t.Fatal("enable accepted a guard-disabled profile")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || len(runtime.applied) != 0 {
				t.Fatalf("rejected enable mutated the yard: err=%v apply=%v", err, runtime.applied)
			}
		})
	}
}

func TestScopedProfileStoppedAndOverrideRejectBeforeWrite(t *testing.T) {
	for _, condition := range []string{"stopped", "missing", "override", "unmanaged", "uninitialized", "wrong-yard", "inherited-ownership"} {
		t.Run(condition, func(t *testing.T) {
			cli, incus, runtime, path, _ := scopedProfileFixture(t, "ENVIRONMENT_PROFILES=neighbor\n")
			if condition == "missing" {
				delete(incus.Instances, "subyard/yard")
			}
			if condition == "stopped" {
				instance := incus.Instances["subyard/yard"]
				instance.Status = "Stopped"
				incus.Instances["subyard/yard"] = instance
			}
			if condition == "override" {
				cli.baseEnv["ENVIRONMENT_PROFILES"] = "neighbor"
				cli.env["ENVIRONMENT_PROFILES"] = "neighbor"
			}
			instance := incus.Instances["subyard/yard"]
			switch condition {
			case "unmanaged":
				delete(instance.Config, "user.subyard.managed")
			case "uninitialized":
				delete(instance.Config, "user.subyard.initialized")
			case "wrong-yard":
				instance.Config["user.subyard.name"] = "other"
			case "inherited-ownership":
				instance.LocalConfig = map[string]string{}
			}
			if condition != "missing" {
				incus.Instances["subyard/yard"] = instance
			}
			before, _ := os.ReadFile(path)
			if _, err := prepareScopedProfile(t, cli, "enable", "fixture"); err == nil {
				t.Fatal("precondition accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || len(runtime.applied) != 0 {
				t.Fatal("changed target before rejection")
			}
		})
	}
}

func TestScopedProfileFailureRetainsSelectionAndRetryRepairs(t *testing.T) {
	cli, _, runtime, path, output := scopedProfileFixture(t, "ENVIRONMENT_PROFILES=neighbor\n")
	runtime.fail = true
	prepared, err := prepareScopedProfile(t, cli, "enable", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code == 0 {
		t.Fatal("failed apply succeeded")
	}
	prepared.Close()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "neighbor fixture") || !strings.Contains(output.String(), "runtime remains pending") {
		t.Fatal("desired selection not retained")
	}
	runtime.fail = false
	prepared, err = prepareScopedProfile(t, cli, "enable", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 || !runtime.installed {
		t.Fatalf("retry=%d %s", code, output)
	}
}

func TestScopedProfileStaleAndDeclinePreserveSelection(t *testing.T) {
	for _, condition := range []string{"stale", "decline", "stopped-after-prepare", "replaced-after-prepare"} {
		t.Run(condition, func(t *testing.T) {
			cli, incus, runtime, path, _ := scopedProfileFixture(t, "ENVIRONMENT_PROFILES=neighbor\n")
			cli.options.Prompt = &testkit.Prompt{Answers: []bool{false}}
			prepared, err := prepareScopedProfile(t, cli, "enable", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if condition == "stale" {
				writeCLIFile(t, path, "ENVIRONMENT_PROFILES=other\n", 0600)
			}
			if condition == "stopped-after-prepare" {
				instance := incus.Instances["subyard/yard"]
				instance.Status = "Stopped"
				incus.Instances["subyard/yard"] = instance
			}
			if condition == "replaced-after-prepare" {
				instance := incus.Instances["subyard/yard"]
				instance.Config["volatile.uuid"] = "replacement"
				incus.Instances["subyard/yard"] = instance
			}
			before, _ := os.ReadFile(path)
			if code := cli.runPreparedCommand(context.Background(), prepared, condition != "decline"); code == 0 {
				t.Fatal("unapproved or stale apply succeeded")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || len(runtime.applied) != 0 {
				t.Fatal("rejected plan mutated target")
			}
		})
	}
}

func TestScopedProfileImplicitNoOpKeepsDefaults(t *testing.T) {
	cli, _, runtime, path, output := scopedProfileFixture(t, "SSH_PORT=2222\n")
	runtime.installed = true
	before, _ := os.ReadFile(path)
	prepared, err := prepareScopedProfile(t, cli, "enable", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 {
		t.Fatalf("implicit noop=%d %s", code, output)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("no-op replaced implicit defaults")
	}
}

func TestScopedProfileDisableRejectsArtifactReplacement(t *testing.T) {
	cli, _, runtime, path, _ := scopedProfileFixture(t, "ENVIRONMENT_PROFILES='neighbor fixture'\n")
	runtime.installed = true
	artifact := filepath.Join(cli.options.RepositoryRoot, "data", "profile-fixture", "default")
	writeProfileTestFile(t, artifact, []byte("original"), 0600)
	prepared, err := prepareScopedProfile(t, cli, "disable", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	writeCLIFile(t, artifact, "replacement", 0600)
	before, _ := os.ReadFile(path)
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code == 0 {
		t.Fatal("accepted replacement artifact")
	}
	after, _ := os.ReadFile(path)
	data, _ := os.ReadFile(artifact)
	if !bytes.Equal(before, after) || string(data) != "replacement" || len(runtime.applied) != 0 {
		t.Fatal("stale disable changed target")
	}
}

// Setup is owner-local and must not require a running yard or inspect init stages
// when the existing protected connection can be reused.
func scopedProfileSetupFixture(t *testing.T) (*CLI, *testkit.Incus, string, *bytes.Buffer) {
	t.Helper()
	cli, incus, _, path, output := scopedProfileFixture(t, "ENVIRONMENT_PROFILES=neighbor\n")
	writeProfileSetupDeclaration(t, cli.options.RepositoryRoot)
	declaration := filepath.Join(cli.options.RepositoryRoot, "config", "profiles", "fixture", "profile.json")
	data, _ := os.ReadFile(declaration)
	writeCLIFile(t, declaration, strings.Replace(string(data), `"schema_version":1,`, `"schema_version":1,"owner_service":"owner.sh",`, 1), 0600)
	var err error
	cli, err = New(cli.options)
	if err != nil {
		t.Fatal(err)
	}
	cli.promptInputTerminal = func() bool { return true }
	return cli, incus, path, output
}

func TestScopedProfileSetupSkipsYardAndInit(t *testing.T) {
	cli, incus, path, output := scopedProfileSetupFixture(t)
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	writeProfileSetupPEM(t, filepath.Join(root, "generated", "fixture", "key.pem"))
	writeCLIFile(t, filepath.Join(root, "fixture.json"), `{"account_id":"42","installation_id":987}`, 0600)
	delete(incus.Instances, "subyard/yard")
	before, _ := os.ReadFile(path)
	prepared, err := prepareScopedProfile(t, cli, "setup", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 {
		t.Fatalf("setup=%d %s", code, output)
	}
	platform := cli.options.InitPlatform.(*initPlatformFixture)
	if len(platform.applied) != 0 {
		t.Fatalf("setup applied %v", platform.applied)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("connection setup selected profile")
	}
}

func TestScopedProfileSetupPromptDeclineAndCredentialOnlyApply(t *testing.T) {
	for _, condition := range []string{"skip", "decline", "invalid-pem", "reuse-pem", "yes"} {
		t.Run(condition, func(t *testing.T) {
			cli, _, path, output := scopedProfileSetupFixture(t)
			root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
			source := filepath.Join(testkit.TempDir(t), "download.pem")
			writeCLIFile(t, source, "invalid PEM: preparation must only inspect metadata", 0644)
			input := "42\n987\n" + source + "\n"
			if condition == "skip" {
				input = "\n"
			}
			if condition == "reuse-pem" {
				writeProfileSetupPEM(t, filepath.Join(root, "generated", "fixture", "key.pem"))
				input = "42\n987\n"
			}
			cli.options.Stdin = strings.NewReader(input)
			cli.options.Prompt = &testkit.Prompt{Answers: []bool{condition != "decline"}}
			args := []string{"setup", "fixture"}
			if condition == "yes" {
				args = append(args, "--yes")
			}
			prepared, err := prepareScopedProfile(t, cli, args...)
			if condition == "skip" || condition == "yes" {
				if err == nil {
					t.Fatal("missing setup was accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.Close()
				code := cli.runPreparedCommand(context.Background(), prepared, false)
				if condition == "reuse-pem" && code != 0 {
					t.Fatalf("setup failed: %s", output)
				}
				if condition != "reuse-pem" && code == 0 {
					t.Fatal("invalid or declined setup succeeded")
				}
			}
			info, _ := os.Stat(source)
			if condition != "invalid-pem" && info.Mode().Perm() != 0644 {
				t.Fatal("source protected before consent")
			}
			platform := cli.options.InitPlatform.(*initPlatformFixture)
			if condition == "invalid-pem" {
				// If initialization was already converged, no stage is applied.
				for _, stage := range platform.applied {
					if stage != ports.ReconcileStageKeys {
						t.Fatalf("broad init: %v", platform.applied)
					}
				}
			} else if len(platform.applied) != 0 {
				t.Fatalf("unexpected init stages: %v", platform.applied)
			}
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "fixture") {
				t.Fatal("setup changed yard selection")
			}
		})
	}
}
