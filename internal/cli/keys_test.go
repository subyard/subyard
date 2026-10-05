package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

const cliCredentialPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture"

type credentialInputProbe struct {
	reader io.Reader
	reads  int
}

func (probe *credentialInputProbe) Read(buffer []byte) (int, error) {
	probe.reads++
	return probe.reader.Read(buffer)
}

func TestKeysTypedConfirmationProtectsPayloadAndUsesResolvedDefaults(t *testing.T) {
	const credentialID = "cred-0123456789abcdef0123456789abcdef"

	t.Run("add decline uses default yes without reading protected stdin", func(t *testing.T) {
		input := &credentialInputProbe{reader: strings.NewReader("protected-value")}
		prompt := &testkit.Prompt{Answers: []bool{false}}
		program, loaded, definition, keysRoot := credentialCLIFixture(t, input, prompt, true)

		if code := program.runKeys(context.Background(), loaded, definition, []string{"add", "fixture"}); code != 1 {
			t.Fatalf("declined add code=%d", code)
		}
		if input.reads != 0 {
			t.Fatalf("protected stdin was read %d time(s) before consent", input.reads)
		}
		if len(prompt.Requests) != 1 || prompt.Requests[0].Summary != "Add encrypted credential" ||
			prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
			t.Fatalf("add prompt=%#v", prompt.Requests)
		}
		assertNoCredentialRecords(t, keysRoot)
	})

	t.Run("non-terminal input cannot double as consent or protected payload", func(t *testing.T) {
		input := &credentialInputProbe{reader: strings.NewReader("y\nprotected-value")}
		program, loaded, definition, keysRoot := credentialCLIFixture(t, input, nil, true)

		if code := program.runKeys(context.Background(), loaded, definition, []string{"add", "fixture"}); code != 1 {
			t.Fatalf("non-terminal add code=%d", code)
		}
		if input.reads != 0 {
			t.Fatalf("non-terminal protected stdin was consumed %d time(s)", input.reads)
		}
		assertNoCredentialRecords(t, keysRoot)
	})

	t.Run("yes cannot bypass rotate preflight or open its source", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "replacement")
		prompt := &testkit.Prompt{}
		program, loaded, definition, _ := credentialCLIFixture(
			t, strings.NewReader("unused"), prompt, true,
		)

		if code := program.runKeys(context.Background(), loaded, definition, []string{
			"rotate", credentialID, "--file", source, "--yes",
		}); code != 1 {
			t.Fatalf("invalid rotate code=%d", code)
		}
		if len(prompt.Requests) != 0 {
			t.Fatalf("invalid rotate prompted: %#v", prompt.Requests)
		}
		stderr := program.options.Stderr.(*bytes.Buffer).String()
		if !strings.Contains(stderr, "credential") || strings.Contains(stderr, source) {
			t.Fatalf("rotate opened protected source before head preflight: %q", stderr)
		}
	})

	for _, test := range []struct {
		name           string
		command        string
		wantSummary    string
		wantDefault    domain.ConfirmationDefault
		initialState   string
		wantPrompt     bool
		wantReturnCode int
	}{
		{
			name: "revoke is recreatable default yes", command: "revoke",
			wantSummary: "Revoke encrypted credential", wantDefault: domain.ConfirmationDefaultYes,
			initialState: "active", wantPrompt: true, wantReturnCode: 1,
		},
		{
			name: "tombstone is irreversible default no", command: "delete",
			wantSummary: "Permanently delete encrypted credential", wantDefault: domain.ConfirmationDefaultNo,
			initialState: "active", wantPrompt: true, wantReturnCode: 1,
		},
		{
			name: "repeated tombstone is no-op", command: "delete",
			initialState: "tombstone", wantPrompt: false, wantReturnCode: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			prompt := &testkit.Prompt{}
			if test.wantPrompt {
				prompt.Answers = []bool{false}
			}
			program, loaded, definition, keysRoot := credentialCLIFixture(
				t, strings.NewReader("unused"), prompt, true,
			)
			recordPath := writeCLICredentialRecord(t, keysRoot, test.initialState)
			before, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}

			code := program.runKeys(context.Background(), loaded, definition, []string{test.command, credentialID})
			if code != test.wantReturnCode {
				t.Fatalf("%s code=%d", test.command, code)
			}
			after, err := os.ReadFile(recordPath)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("%s changed existing ledger record: err=%v", test.command, err)
			}
			assertCredentialRecordCount(t, keysRoot, 1)
			if !test.wantPrompt {
				if len(prompt.Requests) != 0 {
					t.Fatalf("no-op prompted: %#v", prompt.Requests)
				}
				return
			}
			if len(prompt.Requests) != 1 || prompt.Requests[0].Summary != test.wantSummary ||
				prompt.Requests[0].Default != test.wantDefault {
				t.Fatalf("%s prompt=%#v", test.command, prompt.Requests)
			}
		})
	}
}

func TestKeysReadOperationSkipsConfirmation(t *testing.T) {
	prompt := &testkit.Prompt{}
	var output bytes.Buffer
	program, loaded, definition, _ := credentialCLIFixture(t, strings.NewReader(""), prompt, false)
	program.options.Stdout = &output

	if code := program.runKeys(context.Background(), loaded, definition, []string{"status"}); code != 0 {
		t.Fatalf("status code=%d output=%q", code, output.String())
	}
	if len(prompt.Requests) != 0 || !strings.Contains(output.String(), "not initialized") {
		t.Fatalf("status prompt=%#v output=%q", prompt.Requests, output.String())
	}
}

func TestRPCKeysPrepareBuildsOwnerPlanWithoutReadingProtectedInput(t *testing.T) {
	input := &credentialInputProbe{reader: strings.NewReader("protected-value")}
	program, loaded, _, _ := credentialCLIFixture(t, input, &testkit.Prompt{}, true)
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	params, err := json.Marshal(map[string]any{"arguments": []string{"add", "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "keys.prepare", OperationID: "remote-credential-operation", Params: params,
	}, nil)
	plan, ok := result.(domain.OperationPlan)
	if err != nil || !ok || plan.OperationID != "remote-credential-operation" ||
		plan.Command != "keys add" || plan.Assessment == nil || plan.Assessment.Action != "keys.add" ||
		plan.Target != domain.TargetLocalOwner || plan.ConfirmationRequest == nil {
		t.Fatalf("owner keys plan=%#v ok=%t err=%v", result, ok, err)
	}
	if input.reads != 0 {
		t.Fatalf("owner preparation read protected stdin %d time(s)", input.reads)
	}
}

func TestRemoteKeysProtectedInputRequiresOwnerLocalTransport(t *testing.T) {
	for _, arguments := range [][]string{{"add", "fixture"}, {"--yes", "add", "fixture"}, {"import", "owner-file"}, {"rotate", "credential"}, {"resolve", "credential", "--rotate"}} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			input := &credentialInputProbe{reader: strings.NewReader("protected-value")}
			prompt := &testkit.Prompt{Answers: []bool{true}}
			program, loaded, definition, _ := credentialCLIFixture(t, input, prompt, true)
			loaded.Context.AccessKind = domain.AccessRemote
			loaded.Context.OwnerEndpoint = "unreachable.invalid"
			if code := program.runRemoteKeys(context.Background(), loaded, definition, arguments); code != 1 {
				t.Fatalf("protected remote code=%d", code)
			}
			if input.reads != 0 || len(prompt.Requests) != 0 {
				t.Fatal("protected remote action consumed payload or prompted")
			}
		})
	}
}

func TestCredentialPreparedActionsReachCoreRegistry(t *testing.T) {
	program, _, _, _ := credentialCLIFixture(t, strings.NewReader(""), &testkit.Prompt{}, false)
	for _, action := range []domain.ActionID{
		"keys.help", "keys.list", "keys.status", "keys.history", "keys.check-exclusive",
		"keys.import-dry-run", "keys.auto-sync-status", "keys.exchange.identity",
		"keys.exchange.bare-path", "keys.add", "keys.import", "keys.rotate", "keys.rollback",
		"keys.resolve-choose", "keys.resolve-rotate", "keys.materialize", "keys.sync",
		"keys.auto-sync-pause", "keys.auto-sync-resume", "keys.trust", "keys.untrust", "keys.move",
		"keys.revoke", "keys.delete-tombstone", "keys.exchange.trust-import",
		"keys.exchange.untrust-import", "keys.exchange.stop-consumer", "keys.exchange.refresh", "keys.auto-worker", "keys.init-store",
	} {
		changed := !strings.Contains(string(action), ".help") &&
			!strings.Contains(string(action), ".list") &&
			!strings.Contains(string(action), ".status") &&
			!strings.Contains(string(action), ".history") &&
			action != "keys.check-exclusive" && action != "keys.import-dry-run" &&
			action != "keys.exchange.identity" && action != "keys.exchange.bare-path"
		delta := domain.ActionDelta{Changed: changed}
		if changed {
			delta.Consequences = []string{"apply credential action"}
		}
		if _, err := program.coreActions.Assess(action, delta); err != nil {
			t.Fatalf("Prepared action %q cannot reach core registry: %v", action, err)
		}
	}
}

func credentialCLIFixture(
	t *testing.T,
	input io.Reader,
	prompt *testkit.Prompt,
	initialized bool,
) (*CLI, config.Loaded, command.Definition, string) {
	t.Helper()
	root := repositoryRoot(t)
	temp := t.TempDir()
	keysRoot := filepath.Join(temp, "keys")
	toolsRoot := filepath.Join(temp, "tools")
	for _, name := range []string{"sops", "age-keygen"} {
		writeConfigCommandFile(t, filepath.Join(toolsRoot, "bin", name), "#!/bin/sh\nexit 0\n", 0o700)
	}
	environment := []string{
		"HOME=" + filepath.Join(temp, "home"),
		"SUBYARD_HOME=" + filepath.Join(temp, "subyard-home"),
		"SUBYARD_NO_AUDIT=1",
		"SUBYARD_OPERATION_ID=credential-operation",
	}
	var stdout, stderr bytes.Buffer
	options := Options{
		RepositoryRoot: root, Program: "yard", Environment: environment,
		Stdin: input, Stdout: &stdout, Stderr: &stderr,
	}
	if prompt != nil {
		options.Prompt = prompt
	}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.manifest.Lookup("keys")
	if !ok {
		t.Fatal("keys command is missing from manifest")
	}
	hostBase := filepath.Join(temp, "yards")
	loaded := config.Loaded{
		Context: domain.Context{
			YardName: "default", AccessKind: domain.AccessLocal,
			Paths: domain.RuntimePaths{
				RepositoryRoot: root,
				OperatorHome:   filepath.Join(temp, "home"),
				ConfigHome:     filepath.Join(temp, "config"),
				DataHome:       filepath.Join(temp, "data"),
				HostBase:       hostBase,
			},
		},
		Environment: map[string]string{
			"SUBYARD_KEYS_ROOT":          keysRoot,
			"SUBYARD_KEYS_CONSUMER_ROOT": filepath.Join(temp, "consumers"),
			"SUBYARD_KEYS_TOOLS_DIR":     toolsRoot,
			"SUBYARD_GIT_BIN":            "/bin/true",
			"SUBYARD_SSH_KEYGEN_BIN":     "/bin/true",
		},
	}
	if initialized {
		installCLICredentialStore(t, keysRoot)
	}
	return program, loaded, definition, keysRoot
}

func installCLICredentialStore(t *testing.T, keysRoot string) {
	t.Helper()
	identity := map[string]any{
		"schemaVersion": 1,
		"actorId":       "actor-a",
		"identityScope": "host",
		"ageRecipient":  "age1fixture",
		"signingPublic": cliCredentialPublicKey,
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, filepath.Join(keysRoot, "identity.json"), string(payload), 0o600)
	writeConfigCommandFile(t, filepath.Join(keysRoot, "identity", "age.txt"), "AGE-SECRET-KEY-fixture\n", 0o600)
	writeConfigCommandFile(t, filepath.Join(keysRoot, "identity", "signing_ed25519"), "private\n", 0o600)
	for _, directory := range []string{
		filepath.Join(keysRoot, "shared", ".git"),
		filepath.Join(keysRoot, "shared.git"),
		filepath.Join(keysRoot, "local", ".git"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func writeCLICredentialRecord(t *testing.T, keysRoot, state string) string {
	t.Helper()
	metadata := domain.CredentialMetadata{
		SchemaVersion: 1,
		CredentialID:  "cred-0123456789abcdef0123456789abcdef",
		RevisionID:    "actor-a-000000000001-aaaaaaaa",
		Label:         "fixture",
		Kind:          "token",
		Zone:          "qa",
		Scope:         "staging",
		Consumer:      "none",
		State:         state,
		RecipientActors: []string{
			"actor-a",
		},
		Syncable:     true,
		ActorID:      "actor-a",
		ActorCounter: 1,
		Timestamp:    time.Unix(100, 0).UTC(),
	}
	envelope := struct {
		domain.CredentialMetadata
		Payload string          `json:"payload"`
		SOPS    json.RawMessage `json:"sops,omitempty"`
	}{
		CredentialMetadata: metadata,
		Payload:            "encrypted-fixture",
		SOPS:               json.RawMessage(`{"age":[{"recipient":"age1fixture"}]}`),
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(keysRoot, "shared", "records", metadata.CredentialID, metadata.RevisionID+".json")
	writeConfigCommandFile(t, path, string(payload), 0o600)
	return path
}

func assertNoCredentialRecords(t *testing.T, keysRoot string) {
	t.Helper()
	assertCredentialRecordCount(t, keysRoot, 0)
}

func assertCredentialRecordCount(t *testing.T, keysRoot string, want int) {
	t.Helper()
	count := 0
	for _, ledger := range []string{"shared", "local"} {
		recordsRoot := filepath.Join(keysRoot, ledger, "records")
		_ = filepath.WalkDir(recordsRoot, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				count++
			}
			return nil
		})
	}
	if count != want {
		t.Fatalf("credential record count=%d, want %d", count, want)
	}
}
