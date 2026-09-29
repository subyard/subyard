package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func profileSetupFixture(t *testing.T, input string) (*CLI, *initExecution) {
	t.Helper()
	program, loaded, _, _ := credentialCLIFixture(t, strings.NewReader(input), nil, false)
	program.promptInputTerminal = func() bool { return true }
	program.options.RepositoryRoot = testkit.TempDir(t)
	writeProfileSetupDeclaration(t, program.options.RepositoryRoot)
	loaded.Environment["ENVIRONMENT_PROFILES"] = "fixture"
	return program, &initExecution{loaded: loaded, mode: initReconcile, platform: newInitPlatformFixture()}
}

func writeProfileSetupPEM(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
}

func TestInitProfileSetupSkipsWithoutReadingInput(t *testing.T) {
	for _, name := range []string{"noninteractive", "yes", "environment-yes", "profile-disabled", "test-vms", "remote", "configs", "reset", "release", "skip"} {
		t.Run(name, func(t *testing.T) {
			input := &profileInputProbe{reader: strings.NewReader("\n")}
			cli, execution := profileSetupFixture(t, "")
			cli.options.Stdin = input
			args := []string{}
			switch name {
			case "noninteractive":
				cli.promptInputTerminal = func() bool { return false }
			case "yes":
				args = []string{"--yes"}
			case "environment-yes":
				cli.env["ASSUME_YES"] = "1"
			case "profile-disabled":
				execution.loaded.Environment["ENVIRONMENT_PROFILES"] = "unselected"
			case "test-vms":
				execution.loaded.Environment["NESTED_E2E_VMS"] = "1"
			case "remote":
				execution.loaded.Context.AccessKind = domain.AccessRemote
			case "configs":
				execution.mode = initConfigs
			case "reset":
				execution.mode = initReset
			case "release":
				cli.releaseTransitionChild = true
			}
			setup, err := cli.prepareInitProfiles(context.Background(), execution, args)
			if err != nil || setup != nil {
				t.Fatalf("setup=%v err=%v", setup, err)
			}
			if name != "skip" && input.reads != 0 {
				t.Fatalf("read input %d times", input.reads)
			}
			if _, err := os.Stat(execution.loaded.Context.Paths.ConfigHome); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preparation wrote settings: %v", err)
			}
		})
	}
}

func TestInitProfileSetupReusesProtectedKeyAndIsIdempotent(t *testing.T) {
	cli, execution := profileSetupFixture(t, "not-an-id\n42\n-1\n987\ny\n")
	path := filepath.Join(execution.loaded.Environment["SUBYARD_KEYS_CONSUMER_ROOT"], "fixture", "key.pem")
	writeProfileSetupPEM(t, path)
	setup, err := cli.prepareInitProfiles(context.Background(), execution, nil)
	if err != nil || setup == nil || setup.items[0].key != nil {
		t.Fatalf("setup=%v err=%v", setup, err)
	}
	// Text input must leave the subsequent confirmation answer untouched.
	tail, _ := io.ReadAll(cli.options.Stdin)
	if string(tail) != "y\n" {
		t.Fatalf("confirmation input lost: %q", tail)
	}
	execution.profileSetup = setup
	action, delta, err := execution.actionPlan()
	if err != nil || action != "yard.init.reconcile" || !delta.Changed || execution.hooksOnly() {
		t.Fatalf("setup missing from init assessment: %s %#v %v", action, delta, err)
	}
	if !strings.Contains(strings.Join(delta.Consequences, "\n"), "Account ID: 42") {
		t.Fatal("missing concrete account assessment")
	}
	if _, err := os.Stat(setup.items[0].path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config written before approval: %v", err)
	}
	if err := setup.apply(context.Background(), execution, cli.options.Stdout); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(setup.items[0].path)
	var cfg map[string]any
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["account_id"] != "42" || cfg["installation_id"] != float64(987) {
		t.Fatalf("config=%#v", cfg)
	}

	info, _ := os.Stat(setup.items[0].path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	before, _ := os.ReadFile(setup.items[0].path)
	again, err := cli.prepareInitProfiles(context.Background(), execution, nil)
	after, _ := os.ReadFile(setup.items[0].path)
	if err != nil || again != nil || !bytes.Equal(before, after) {
		t.Fatalf("repeat setup=%v err=%v", again, err)
	}
}

func TestInitProfileSetupDeclinePreservesSourceAndSettings(t *testing.T) {
	cli, execution := profileSetupFixture(t, "")
	path := filepath.Join(testkit.TempDir(t), "download.pem")
	writeProfileTestFile(t, path, []byte("not even a key: preparation must not parse it"), 0o644)
	cli.options.Stdin = strings.NewReader("42\n987\n" + path + "\nn\n")
	setup, err := cli.prepareInitProfiles(context.Background(), execution, nil)
	if err != nil || setup == nil || setup.items[0].key == nil {
		t.Fatalf("setup=%v err=%v", setup, err)
	}
	execution.profileSetup = setup
	_, delta, err := execution.actionPlan()
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := (streamPrompt{input: cli.options.Stdin, output: cli.options.Stdout, interactive: func() bool { return true }}).Confirm(context.Background(), domain.ConfirmationRequest{Summary: "Initialize yard", Consequences: delta.Consequences, Default: domain.ConfirmationDefaultYes})
	if confirmed || !errors.Is(err, domain.ErrOperationDeclined) {
		t.Fatalf("confirmation=%t err=%v", confirmed, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Fatal("PEM mode changed before confirmation")
	}
	if _, err := os.Stat(setup.items[0].path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("config changed before confirmation")
	}
}

func TestInitProfileSetupRejectsConcurrentConfigAndEOF(t *testing.T) {
	cli, execution := profileSetupFixture(t, "42\n987\n")
	path := filepath.Join(execution.loaded.Environment["SUBYARD_KEYS_CONSUMER_ROOT"], "fixture", "key.pem")
	writeProfileSetupPEM(t, path)
	setup, err := cli.prepareInitProfiles(context.Background(), execution, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("operator-owned\n")
	writeProfileTestFile(t, setup.items[0].path, replacement, 0o600)
	if err := setup.apply(context.Background(), execution, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("stale error=%v", err)
	}
	current, _ := os.ReadFile(setup.items[0].path)
	if !bytes.Equal(current, replacement) {
		t.Fatal("overwrote concurrent settings")
	}
	cli, execution = profileSetupFixture(t, "42\n")
	if _, err := cli.prepareInitProfiles(context.Background(), execution, nil); err == nil {
		t.Fatal("EOF accepted")
	}
}

func TestInitProfileSetupPreservesExistingCustomConfig(t *testing.T) {
	cli, execution := profileSetupFixture(t, "")
	path := filepath.Join(testkit.TempDir(t), "key.pem")
	writeProfileSetupPEM(t, path)
	cfgPath := filepath.Join(execution.loaded.Context.Paths.ConfigHome, "fixture.json")
	cfg := []byte(`{"account_id":"42","installation_id":987,"credential_file":"` + path + `"}`)
	writeProfileTestFile(t, cfgPath, cfg, 0o400)
	setup, err := cli.prepareInitProfiles(context.Background(), execution, nil)
	if err != nil || setup != nil {
		t.Fatalf("setup=%v err=%v", setup, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	setup, err = cli.prepareInitProfiles(context.Background(), execution, nil)
	if err != nil || setup != nil {
		t.Fatalf("missing custom key: setup=%v err=%v", setup, err)
	}
	current, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(current, cfg) {
		t.Fatal("custom config changed")
	}
}

func TestPreparedInitOnlyOffersProfileAtInteractiveBoundary(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProfileSetupDeclaration(t, root)
	platform := newInitPlatformFixture()
	var output bytes.Buffer
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform, Stdin: strings.NewReader(""), Stdout: &output, Stderr: &output})
	if err != nil {
		t.Fatal(err)
	}
	cli.promptInputTerminal = func() bool { return true }
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := cli.manifest.Lookup("init")
	for _, readOnly := range []bool{false, true} {
		prepared, err := cli.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, ReadOnly: readOnly})
		if err != nil {
			t.Fatal(err)
		}
		prepared.Close()
	}
	if strings.Contains(output.String(), "Connect fixture") {
		t.Fatal("RPC preparation tried to read setup input")
	}
	// An exact/read-only direct plan must also stay noninteractive.
	prepared, err := cli.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, ReadOnly: true, InteractiveSetup: true})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Close()
}

type profileInputProbe struct {
	reader io.Reader
	reads  int
}

func (p *profileInputProbe) Read(b []byte) (int, error) { p.reads++; return p.reader.Read(b) }

func writeProfileTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, data, mode)
}

func TestInitProfileSetupInitializesKeysBeforeIncus(t *testing.T) {
	cli, execution := profileSetupFixture(t, "")
	// The synthetic ledger stands in for the Keys stage's initialization. The
	// rejected PEM then proves that no later host provisioning ran first.
	installCLICredentialStore(t, execution.loaded.Environment["SUBYARD_KEYS_ROOT"])
	source := filepath.Join(testkit.TempDir(t), "download.pem")
	writeProfileTestFile(t, source, []byte("invalid PEM"), 0o644)
	cli.options.Stdin = strings.NewReader("42\n987\n" + source + "\n")
	setup, err := cli.prepareInitProfiles(context.Background(), execution, nil)
	if err != nil {
		t.Fatal(err)
	}
	platform := execution.platform.(*initPlatformFixture)
	platform.converged[ports.ReconcileStageKeys] = false
	platform.converged[ports.ReconcileStageIncus] = false
	if err := setup.apply(context.Background(), execution, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid RSA private key") {
		t.Fatalf("error=%v", err)
	}
	if len(platform.applied) != 1 || platform.applied[0] != ports.ReconcileStageKeys {
		t.Fatalf("applied=%v", platform.applied)
	}
	if _, err := os.Stat(setup.items[0].path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid key produced profile settings")
	}
}

func TestInitProfileSetupDetectsProfileDrift(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProfileSetupDeclaration(t, root)
	path := filepath.Join(root, "state", "yards", "default", "config.env")
	writeProfileTestFile(t, path, []byte("ENVIRONMENT_PROFILES=fixture\n"), 0o600)
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: newInitPlatformFixture()})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := cli.prepareInitExecution(context.Background(), loaded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := profile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	execution.profileSetup = &initProfileSet{root: root, definitions: definitions, items: []*initProfileSetup{{definition: definitions[0]}}}
	// ENVIRONMENT_PROFILES alone need not be in the integration baseline.
	writeProfileTestFile(t, path, []byte("ENVIRONMENT_PROFILES=\n"), 0o600)
	if err := execution.checkIntegrationBaseline(cli); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("profile drift accepted: %v", err)
	}
}

func TestInitProfileDirectCommandConfirmsOnce(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(strconv.FormatBool(approve), func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			writeProfileSetupDeclaration(t, root)
			keyPath := filepath.Join(root, "state", "generated", "fixture", "key.pem")
			writeProfileSetupPEM(t, keyPath)
			prompt := &testkit.Prompt{Answers: []bool{approve}}
			platform := newInitPlatformFixture()
			var output bytes.Buffer
			cli, err := New(Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"init"}, InitPlatform: platform, Prompt: prompt, Stdin: strings.NewReader("42\n987\n"), Stdout: &output, Stderr: &output})
			if err != nil {
				t.Fatal(err)
			}
			cli.promptInputTerminal = func() bool { return true }
			code := cli.Run(context.Background())
			if (code == 0) != approve {
				t.Fatalf("code=%d output=%s", code, output.String())
			}
			if len(prompt.Requests) != 1 || !strings.Contains(strings.Join(prompt.Requests[0].Consequences, "\n"), "Account ID: 42") {
				t.Fatalf("prompts=%v output=%s", prompt.Requests, output.String())
			}
			_, err = os.Stat(filepath.Join(root, "state", "fixture.json"))
			if approve && err != nil || !approve && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("config write on approve=%t: %v", approve, err)
			}
			if !approve && len(platform.applied) != 0 {
				t.Fatalf("declined init applied stages: %v", platform.applied)
			}
		})
	}
}

func writeProfileSetupDeclaration(t *testing.T, root string) {
	t.Helper()
	writeProfileTestFile(t, filepath.Join(root, "config", "profiles", "fixture", "profile.json"), []byte(`{
 "schema_version":1,
 "default_yards":["default"],
 "disabled_when":{"NESTED_E2E_VMS":"1"},
 "consumers":[{"id":"fixture-key","zone":"global","path":"fixture/key.pem","format":"rsa-private-key"}],
 "setup":{
  "title":"Connect fixture", "instructions":["Configure a synthetic credential consumer."],
  "config_file":"fixture.json",
  "fields":[{"name":"account_id","label":"Account ID","kind":"numeric-string"},{"name":"installation_id","label":"Installation ID","kind":"positive-integer"}],
  "consumer":"fixture-key","zone":"global","label":"fixture","key_override_field":"credential_file"
 }
}`), 0o600)
}

func TestInitProfileSetupRejectsDeclarationDrift(t *testing.T) {
	cli, execution := profileSetupFixture(t, "42\n987\n")
	keyPath := filepath.Join(execution.loaded.Environment["SUBYARD_KEYS_CONSUMER_ROOT"], "fixture", "key.pem")
	writeProfileSetupPEM(t, keyPath)
	setup, err := cli.prepareInitProfiles(context.Background(), execution, nil)
	if err != nil || setup == nil {
		t.Fatalf("setup=%v err=%v", setup, err)
	}
	path := filepath.Join(cli.options.RepositoryRoot, "config", "profiles", "fixture", "profile.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, path, bytes.ReplaceAll(data, []byte("Connect fixture"), []byte("Changed fixture")), 0o600)
	if err := setup.apply(context.Background(), execution, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("declaration drift accepted: %v", err)
	}
	if _, err := os.Stat(setup.items[0].path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("settings written after declaration changed")
	}
}
