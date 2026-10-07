package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerapi"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/crypto/ssh"
)

func ownerQueryFixture(t *testing.T) (*rpcHandler, *testkit.ScriptedAdapter, *testkit.Incus) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	// Keep exact fixture permissions independent of the caller's umask.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"selected", "legacy", "dedicated"} {
		writeProvisionProfile(t, root, name)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/selected/profile.json"), []byte(`{"schema_version":1,"selected_provision_only":true,"default_yards":["default"],"settings":[{"name":"SAMPLE_LIMIT","type":"integer","default":"7","scopes":["shipped","yard"],"syncable":true,"application":"next-command","minimum":1,"maximum":12}]}`), 0o644)
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/dedicated/profile.conf"), []byte("PROVISION_SCOPE=dedicated\n"), 0o644)
	resources := filepath.Join(root, "config/profiles/resource-only/resources/view")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(filepath.Dir(resources), "view.res"), []byte("COMMAND=sample-view\nHANDLER=resources/view/handler.sh\nTITLE=Sample view\nBRINGUP=start\nSHUTDOWN=stop\nACTION=\"start start host-change reversible\"\nACTION=\"stop stop host-change reversible\"\nACTION=\"status status read-only not-needed\"\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(resources, "handler.sh"), []byte("#!/bin/sh\nexit 99\n"), 0o755)
	runner := &testkit.ScriptedAdapter{}
	incus := lifecycleIncus()
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, Incus: incus, AdapterRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	return &rpcHandler{cli: program, loaded: loaded}, runner, incus
}

func TestOwnerProfileQueriesPreserveCatalogSelectionAndStoppedState(t *testing.T) {
	handler, runner, _ := ownerQueryFixture(t)
	handler.loaded.Environment["ENVIRONMENT_PROFILES"] = "selected resource-only"
	result, err := handler.profileList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Profiles) != 4 || len(runner.Requests) != 0 {
		t.Fatalf("profiles=%#v requests=%#v", result, runner.Requests)
	}
	for _, entry := range result.Profiles {
		switch entry.Name {
		case "selected":
			if !entry.Selected || entry.DescriptorVersion == nil || *entry.DescriptorVersion != 1 || entry.Convergence != "unknown" || entry.Diagnostic != "yard-not-running" {
				t.Fatalf("selected=%#v", entry)
			}
		case "resource-only":
			if !entry.Selected || entry.Provisionable || entry.Convergence != "not-applicable" || !slices.Equal(entry.Resources, []string{"sample-view"}) {
				t.Fatalf("resource=%#v", entry)
			}
		case "dedicated":
			if entry.Eligible || entry.Eligibility != "dedicated-role-required" {
				t.Fatalf("dedicated=%#v", entry)
			}
		case "legacy":
			if entry.Selected || entry.DescriptorVersion != nil {
				t.Fatalf("legacy=%#v", entry)
			}
		}
	}
	delete(handler.loaded.Environment, "ENVIRONMENT_PROFILES")
	result, err = handler.profileList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Profiles {
		if (entry.Name == "selected" || entry.Name == "legacy") && !entry.Selected {
			t.Fatalf("implicit selection=%#v", entry)
		}
	}
	handler.loaded.Environment["ENVIRONMENT_PROFILES"] = ""
	result, err = handler.profileList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Profiles {
		if entry.Selected {
			t.Fatalf("empty selection=%#v", entry)
		}
	}
	payload, _ := json.Marshal(result)
	if bytes.Contains(payload, []byte(handler.cli.options.RepositoryRoot)) || bytes.Contains(payload, []byte("handler.sh")) {
		t.Fatal("package implementation leaked")
	}
}

func TestOwnerProfileQueryChecksOnlyRunningSelectedEligibleHooks(t *testing.T) {
	handler, runner, incus := ownerQueryFixture(t)
	handler.loaded.Environment["ENVIRONMENT_PROFILES"] = "selected legacy dedicated resource-only"
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner.Steps = []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Stderr: "converged\n"}, {Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Stderr: "changed\n"}}
	result, err := handler.profileList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.Requests) != 2 {
		t.Fatalf("requests=%#v", runner.Requests)
	}
	for _, request := range runner.Requests {
		if request.Adapter != "provision" || request.Action != "profile-check" || len(request.Arguments) != 2 || request.Arguments[0] != "--check" {
			t.Fatalf("mutation request=%#v", request)
		}
	}
	for _, entry := range result.Profiles {
		if entry.Name == "legacy" && entry.Convergence != "current" || entry.Name == "selected" && entry.Convergence != "changes-required" {
			t.Fatalf("checks=%#v", result)
		}
	}
	runner.Steps = []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "error"}, Stderr: "private hook diagnostic"}, {Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Stderr: "invalid private output"}}
	runner.Requests = nil
	result, err = handler.profileList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Profiles {
		if (entry.Name == "legacy" || entry.Name == "selected") && (entry.Convergence != "unknown" || entry.Diagnostic != "check-unavailable") {
			t.Fatalf("unavailable=%#v", entry)
		}
	}
}

func TestOwnerProfileRPCChecksUseRealAdapterWithIsolatedOperationIdentity(t *testing.T) {
	handler, _, incus := ownerQueryFixture(t)
	root := handler.cli.options.RepositoryRoot
	yards := filepath.Join(handler.loaded.Context.Paths.ConfigHome, "yards", "default")
	if err := os.MkdirAll(yards, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(yards, "config.env"), []byte("ENVIRONMENT_PROFILES=selected\n"), 0o600)
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts", "provision-profile.sh"), []byte("#!/bin/sh\nset -eu\ncat <&3 >/dev/null\n[ \"$SUBYARD_OPERATION_ID\" = rpc-profile-query ]\n[ \"$1\" = --check ] && [ \"$2\" = selected ]\nprintf 'converged\\n'\n"), 0o755)
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	handler.cli.options.AdapterRunner = nil
	var diagnostics bytes.Buffer
	handler.cli.options.Stderr = &diagnostics
	before := make(map[string]string, len(handler.cli.env))
	for name, value := range handler.cli.env {
		before[name] = value
	}
	value, err := handler.Handle(context.Background(), rpc.Call{Method: "profile.list", OperationID: "rpc-profile-query", Params: json.RawMessage(`{}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range value.(ownerapi.ProfileList).Profiles {
		if profile.Name == "selected" && (profile.Convergence != "current" || profile.Diagnostic != "") {
			t.Fatalf("native probe=%#v", profile)
		}
	}
	if !reflect.DeepEqual(before, handler.cli.env) || diagnostics.Len() != 0 {
		t.Fatal("query changed session environment or streamed native probe output")
	}
}

func TestOwnerSettingsQueryUsesCatalogAndRedactsPathsAndSensitiveValues(t *testing.T) {
	handler, _, _ := ownerQueryFixture(t)
	handler.loaded.Environment["SAMPLE_LIMIT"] = "9"
	handler.loaded.Settings["SAMPLE_LIMIT"] = config.SettingTrace{Name: "SAMPLE_LIMIT", Resolutions: []config.SettingResolution{
		{Scope: "default", Role: "shipped", Status: "overridden", Value: "7", Path: "/private/runtime/profile.json"},
		{Scope: "yard", Role: "local", Status: "effective", Value: "9", Path: "/private/settings/config.env"},
	}}
	handler.loaded.Environment["AGENT_codex_PROVISION"] = "/private/package/hook.sh"
	handler.loaded.Environment["YARD_IMAGE"] = "password=private-fixture-value"
	result, err := handler.settingsList()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, setting := range result.Settings {
		if setting.Name == "SAMPLE_LIMIT" {
			found = true
			if setting.Value == nil || *setting.Value != "9" || setting.Default == nil || *setting.Default != "7" || !setting.Editable || setting.Owner != "profile:selected" || setting.Maximum != 12 || len(setting.Provenance) != 2 {
				t.Fatalf("setting=%#v", setting)
			}
		}
		if setting.Name == "EXCLUSIVE_ENVIRONMENT_PROFILE" && setting.Editable {
			t.Fatalf("shipped role is editable: %#v", setting)
		}
		if setting.Name == "YARD_IMAGE" && setting.ValueAvailable {
			t.Fatal("secret-looking effective value leaked")
		}
	}
	if !found {
		t.Fatal("profile setting was omitted")
	}
	payload, _ := json.Marshal(result)
	for _, forbidden := range []string{"/private/", "private-fixture-value", handler.cli.options.RepositoryRoot} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("private value leaked: %s", forbidden)
		}
	}
}

func TestOwnerHostSyncStatusIsOfflineAndMissingRootsStayMissing(t *testing.T) {
	handler, runner, _ := ownerQueryFixture(t)
	handler.loaded.Context.Paths.ConfigHome = filepath.Join(testkit.TempDir(t), "absent")
	result, err := handler.hostSyncStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || !result.Offline || result.Automation != "manual" || result.Registration != "not-configured" || result.Git != nil || len(runner.Requests) != 0 {
		t.Fatalf("status=%#v", result)
	}
	if _, err := os.Lstat(handler.loaded.Context.Paths.ConfigHome); !os.IsNotExist(err) {
		t.Fatalf("reader created config root: %v", err)
	}
}

type ownerCredentialReader struct {
	testkit.CredentialMetadataReader
	status domain.CredentialStatus
}

func (reader ownerCredentialReader) ReadCredentialStatus(context.Context) (domain.CredentialStatus, error) {
	return reader.status, nil
}

func TestOwnerHostSyncStatusProjectsRegisteredGitAndCredentialsReadOnly(t *testing.T) {
	handler, _, _ := ownerQueryFixture(t)
	checkout := filepath.Join(testkit.TempDir(t), "source")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(checkout, "subyard-config.json"), []byte(`{"schemaVersion":1}`), 0o600)
	runConfigSyncGit(t, checkout, "init", "-q", "-b", "main")
	runConfigSyncGit(t, checkout, "add", "subyard-config.json")
	runConfigSyncGit(t, checkout, "-c", "user.name=Subyard Test", "-c", "user.email=test@invalid", "commit", "-qm", "Initialize fixture")
	const remote = "https://test-user:private-fixture-password@example.invalid/config.git?private-query=yes#private-fragment"
	runConfigSyncGit(t, checkout, "remote", "add", "origin", remote)
	runConfigSyncGit(t, checkout, "update-ref", "refs/remotes/origin/main", "HEAD")
	runConfigSyncGit(t, checkout, "config", "branch.main.remote", "origin")
	runConfigSyncGit(t, checkout, "config", "branch.main.merge", "refs/heads/main")
	if err := configsync.RegisterSourceOrigin(handler.loaded.Context.Paths.ConfigHome, checkout, remote); err != nil {
		t.Fatal(err)
	}
	handler.cli.options.Credentials = &ownerCredentialReader{status: domain.CredentialStatus{
		Credentials: []domain.CredentialSummary{{CredentialID: "sample", Label: "private-fixture-label", Conflict: true}},
		Peers:       []domain.CredentialPeerStatus{{Name: "peer-a", Role: "trusted", Trusted: true, LastSuccess: 100}},
	}}
	before := snapshotConfigCheckoutRaw(t, checkout)
	result, err := handler.hostSyncStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Registration != "configured" || result.Git == nil || !result.Git.Available || result.Git.Relation != "up-to-date" || result.Git.Remote != "https://example.invalid/config.git" || result.Credentials.Records != 1 || result.Credentials.Conflicts != 1 || len(result.Credentials.Peers) != 1 {
		t.Fatalf("status=%#v", result)
	}
	if !reflect.DeepEqual(before, snapshotConfigCheckoutRaw(t, checkout)) {
		t.Fatal("status changed registered checkout")
	}
	if _, err := os.Lstat(filepath.Join(checkout, ".git/FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatal("offline status fetched upstream")
	}
	payload, _ := json.Marshal(result)
	for _, forbidden := range []string{"private-fixture-password", "private-query", "private-fragment", "private-fixture-label", checkout} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("protected input leaked: %s", forbidden)
		}
	}
	runConfigSyncGit(t, checkout, "remote", "set-url", "origin", checkout)
	result, err = handler.hostSyncStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Git == nil || result.Git.Remote != "<local-source>" {
		t.Fatalf("local remote=%#v", result.Git)
	}
	payload, _ = json.Marshal(result)
	if bytes.Contains(payload, []byte(checkout)) {
		t.Fatal("local source path leaked")
	}
}

func TestOwnerQueriesValidateParamsOwnerAndBounds(t *testing.T) {
	handler, _, _ := ownerQueryFixture(t)
	for _, method := range []string{"profile.list", "settings.list", "host.sync.status"} {
		_, err := handler.Handle(context.Background(), rpc.Call{Method: method, Params: json.RawMessage(`{"unknown":true}`)}, nil)
		var fault *rpc.Error
		if !errors.As(err, &fault) || fault.Code != "invalid_params" {
			t.Fatalf("method=%s err=%v", method, err)
		}
	}
	handler.loaded.Context.AccessKind = domain.AccessRemote
	for _, method := range []string{"profile.list", "settings.list", "host.sync.status"} {
		_, err := handler.Handle(context.Background(), rpc.Call{Method: method, Params: json.RawMessage(`{}`)}, nil)
		fault, ok := err.(*rpc.Error)
		if !ok || fault.Code != "remote_owner_required" {
			t.Fatalf("method=%s err=%v", method, err)
		}
	}
	if _, err := boundedOwnerQuery(map[string]any{"oversized": strings.Repeat("x", 8193)}, nil); err == nil {
		t.Fatal("oversized string accepted")
	}
	if _, err := boundedOwnerQuery(make([]int, 1025), nil); err == nil {
		t.Fatal("oversized list accepted")
	}
}

func TestOwnerRPCNegotiatesAndDispatchesTypedQueries(t *testing.T) {
	handler, runner, _ := ownerQueryFixture(t)
	var input, output, diagnostics bytes.Buffer
	codec := rpc.NewCodec(nil, &input)
	for index, method := range []string{"rpc.negotiate", "profile.list", "settings.list", "host.sync.status"} {
		if err := codec.Write(rpc.Request{Version: 1, Type: "request", ID: []string{"hello", "profiles", "settings", "sync"}[index], Method: method, Params: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	handler.cli.options.Arguments = []string{"rpc", "--stdio"}
	handler.cli.options.Stdin, handler.cli.options.Stdout = &input, &output
	handler.cli.options.Stderr = &diagnostics
	program, err := New(handler.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("RPC exit=%d diagnostics=%s", code, &diagnostics)
	}
	reader := rpc.NewCodec(&output, nil)
	seen := map[string]bool{}
	for range 4 {
		response, err := reader.ReadResponse()
		if err != nil || response.Error != nil {
			t.Fatalf("response=%#v err=%v", response, err)
		}
		seen[response.ID] = true
		if response.ID == "hello" {
			encoded, _ := json.Marshal(response.Result)
			var negotiated struct {
				Version       int      `json:"version"`
				EngineVersion string   `json:"engineVersion"`
				Capabilities  []string `json:"capabilities"`
			}
			if err := json.Unmarshal(encoded, &negotiated); err != nil {
				t.Fatal(err)
			}
			if negotiated.Version != 1 || negotiated.EngineVersion != Version {
				t.Fatalf("negotiation=%#v", negotiated)
			}
			for _, capability := range []string{profileListCapability, settingsListCapability, hostSyncStatusCapability, sessionPrepareCapability, exactPlanCapability, operationStepsCapability} {
				if !slices.Contains(negotiated.Capabilities, capability) {
					t.Fatalf("capability missing: %s", capability)
				}
			}
		}
	}
	if len(seen) != 4 || len(runner.Requests) != 0 {
		t.Fatalf("responses=%v checks=%v", seen, runner.Requests)
	}
}

func TestOwnerQueriesReloadPersistedSettingsWithoutChangingSession(t *testing.T) {
	handler, _, _ := ownerQueryFixture(t)
	beforeEnvironment := make(map[string]string, len(handler.cli.env))
	for name, value := range handler.cli.env {
		beforeEnvironment[name] = value
	}
	query := func(method string) any {
		t.Helper()
		value, err := handler.Handle(context.Background(), rpc.Call{Method: method, Params: json.RawMessage(`{}`)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	query("settings.list")
	yardRoot := filepath.Join(handler.loaded.Context.Paths.ConfigHome, "yards", "default")
	if err := os.MkdirAll(yardRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(yardRoot, "config.env"), []byte("SAMPLE_LIMIT=9\nENVIRONMENT_PROFILES=\n"), 0o600)
	settings := query("settings.list").(ownerapi.SettingsList)
	for _, setting := range settings.Settings {
		if setting.Name == "SAMPLE_LIMIT" && (setting.Value == nil || *setting.Value != "9") {
			t.Fatalf("stale setting=%#v", setting)
		}
	}
	profiles := query("profile.list").(ownerapi.ProfileList)
	for _, profile := range profiles.Profiles {
		if profile.Selected {
			t.Fatalf("stale selection=%#v", profile)
		}
	}
	if !reflect.DeepEqual(handler.cli.env, beforeEnvironment) {
		t.Fatal("query changed session environment")
	}
	if handler.loaded.Environment["SAMPLE_LIMIT"] != "7" {
		t.Fatal("query changed bound session config")
	}
}

func TestOwnerSessionPreparationUsesOwnerContextAndNativeProjectFacts(t *testing.T) {
	handler, runner, incus := ownerQueryFixture(t)
	tools := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(tools, "htop"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", tools)
	params := ownerapi.SessionParams{Kind: "shell", Scope: "host"}
	host, err := handler.prepareSession(context.Background(), params)
	if err != nil || !slices.Equal(host.OwnerArguments, []string{"bash", "-l"}) {
		t.Fatalf("host=%#v err=%v", host, err)
	}
	params.Kind = "resources"
	host, err = handler.prepareSession(context.Background(), params)
	if err != nil || !slices.Equal(host.OwnerArguments, []string{"htop"}) {
		t.Fatalf("resources=%#v err=%v", host, err)
	}
	if _, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "shell"}); err == nil {
		t.Fatal("stopped yard session was prepared")
	}
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	instance.Devices = map[string]map[string]string{"ssh": {"type": "proxy"}}
	incus.Instances["subyard/yard"] = instance
	yard, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "resources"})
	if err != nil || !slices.Equal(yard.OwnerArguments, []string{"yard", "-Y", "default", "shell", "--", "htop"}) {
		t.Fatalf("yard resources=%#v err=%v", yard, err)
	}
	store, err := state.NewFileStore(handler.loaded.Context.Paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	record := domain.ProjectRecord{Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo", Mode: domain.ProjectSync, HostPath: "/private/project-source", YardPath: state.YardPath("Demo"), SSHHost: handler.loaded.Context.SSHHost}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	project, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "shell", ProjectID: "Demo"})
	if err != nil || !slices.Equal(project.OwnerArguments, []string{"yard", "-Y", "default", "shell", "Demo"}) {
		t.Fatalf("project=%#v err=%v", project, err)
	}
	if _, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "vscode", ProjectID: "Demo"}); err == nil {
		t.Fatal("VS Code launch lacked protected guest pin")
	}
	key, err := ssh.NewPublicKey(ed25519.PublicKey(bytes.Repeat([]byte{1}, ed25519.PublicKeySize)))
	if err != nil {
		t.Fatal(err)
	}
	keyRoot := filepath.Join(handler.loaded.Context.Paths.DataHome, "ssh")
	if err := os.MkdirAll(keyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(keyRoot, "known_hosts")
	testkit.WriteFile(t, known, append([]byte("[127.0.0.1]:2222 "), ssh.MarshalAuthorizedKey(key)...), 0o600)
	code, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "vscode", ProjectID: "Demo"})
	if err != nil || code.VSCode == nil || code.VSCode.FolderPath != record.YardPath || code.VSCode.HostKeyFingerprint != ssh.FingerprintSHA256(key) || code.VSCode.HostKey != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) || code.VSCode.Address != "127.0.0.1" || code.VSCode.Port != 2222 || code.VSCode.RemoteAuthentication != "already-authorized-desktop-agent-key" || len(code.OwnerArguments) != 0 || !slices.Equal(code.LocalArguments, []string{"yard", "-Y", "default", "code", "Demo"}) {
		t.Fatalf("code=%#v err=%v", code, err)
	}
	payload, _ := json.Marshal(code)
	for _, forbidden := range []string{record.HostPath, keyRoot, "id_ed25519", handler.cli.options.RepositoryRoot} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("session leaked protected path: %s", forbidden)
		}
	}
	if len(runner.Requests) != 0 || len(incus.ExecCalls) != 0 || len(incus.PowerUpdates) != 0 {
		t.Fatal("session preparation performed an adapter action")
	}
	testkit.WriteFile(t, known, []byte("[127.0.0.1]:2222 invalid-key\n"), 0o600)
	if _, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "vscode", ProjectID: "Demo"}); err == nil {
		t.Fatal("invalid public host key was accepted")
	}
	handler.loaded.Environment["ALLOWS_PROJECTS"] = "false"
	if _, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "shell", ProjectID: "Demo"}); err == nil {
		t.Fatal("project role restriction bypassed")
	}
}

func TestOwnerSessionPreparationRejectsInvalidSelectorsAndKeepsAbsentRootsAbsent(t *testing.T) {
	handler, _, _ := ownerQueryFixture(t)
	for _, params := range []ownerapi.SessionParams{{Kind: "command"}, {Kind: "shell", Scope: "other"}, {Kind: "shell", ProjectID: "../outside"}, {Kind: "shell", Scope: "host", ProjectID: "Demo"}, {Kind: "vscode", Scope: "host", ProjectID: "Demo"}, {Kind: "vscode"}, {Kind: "resources", ProjectID: "Demo"}} {
		_, err := handler.prepareSession(context.Background(), params)
		fault, ok := err.(*rpc.Error)
		if !ok || fault.Code != "invalid_params" {
			t.Fatalf("params=%#v err=%v", params, err)
		}
	}
	absent := filepath.Join(testkit.TempDir(t), "absent")
	handler.loaded.Context.Paths.StateDir = absent
	if _, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "shell", Scope: "host"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Fatal("session preparation created project root")
	}
	handler.loaded.Context.AccessKind = domain.AccessRemote
	_, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "command", Scope: "host"})
	fault, ok := err.(*rpc.Error)
	if !ok || fault.Code != "invalid_params" {
		t.Fatalf("invalid parameters lost precedence over owner routing: %v", err)
	}
	if _, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "shell", Scope: "host"}); err == nil {
		t.Fatal("controller session masqueraded as owner")
	}
}

func TestOwnerHostResourcesRefusesMissingOwnerToolWithoutMutation(t *testing.T) {
	handler, runner, incus := ownerQueryFixture(t)
	tools := testkit.TempDir(t)
	t.Setenv("PATH", tools)
	_, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "resources", Scope: "host"})
	fault, ok := err.(*rpc.Error)
	if !ok || fault.Code != "session_tool_unavailable" || strings.Contains(fault.Message, tools) {
		t.Fatalf("missing host tool rejection=%v", err)
	}
	if len(runner.Requests) != 0 || len(incus.ExecCalls) != 0 || len(incus.PowerUpdates) != 0 {
		t.Fatal("host tool preflight performed a guest or adapter action")
	}
	if _, err := handler.prepareSession(context.Background(), ownerapi.SessionParams{Kind: "shell", Scope: "host"}); err != nil {
		t.Fatal("missing resources tool blocked the owner shell")
	}
}

func TestOwnerExactConfigPlansKeepPrivateInputsOutOfPublicOutput(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	program := configExactProgram(t, root, environment, nil, nil)
	prepared := configExactPlan(t, program, loaded, []string{"set", "SSH_HOST", "safe-unique-input-marker", "--scope", "host"})
	public, err := json.Marshal(bindExactOperationPlan(prepared, prepared.Plan.CreatedAt))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"safe-unique-input-marker", root, "Arguments", "exactState", "Content"} {
		if bytes.Contains(public, []byte(forbidden)) {
			t.Fatalf("private plan binding leaked: %s", forbidden)
		}
	}
	definition, _ := program.manifest.Lookup("config")
	_, err = program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{"set", "SSH_PORT", "password=synthetic-protected-value", "--scope", "host"}, ReadOnly: true})
	if err == nil || strings.Contains(err.Error(), "synthetic-protected-value") {
		t.Fatal("invalid protected value leaked through planning diagnostics")
	}
}

func TestOwnerSettingsGoldenPreservesNativeMultilineValuesAndProvenance(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), "api/yard-rpc/v1/fixtures/settings-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Result ownerapi.SettingsList `json:"result"`
	}
	if err := json.Unmarshal(content, &golden); err != nil {
		t.Fatal(err)
	}
	handler, _, _ := ownerQueryFixture(t)
	var expected []ownerapi.Setting
	for _, setting := range golden.Result.Settings {
		if setting.Type != config.SettingMultiline && setting.Type != config.SettingLinkList {
			continue
		}
		if setting.Value == nil || !strings.Contains(*setting.Value, "\n") {
			t.Fatal("golden lacks representative native multiline content")
		}
		if err := handler.loaded.Catalog.ValidateSetting(config.ScopeShipped, setting.Name, *setting.Value, false); err != nil {
			t.Fatal(err)
		}
		handler.loaded.Environment[setting.Name] = *setting.Value
		handler.loaded.Settings[setting.Name] = config.SettingTrace{Name: setting.Name, Resolutions: []config.SettingResolution{{
			Scope: "default", Role: "shipped", Status: "effective", Value: *setting.Value,
		}}}
		expected = append(expected, setting)
	}
	if len(expected) != 2 {
		t.Fatal("golden lacks both multiline and native link-list cases")
	}
	actual, err := handler.settingsList()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range expected {
		index := slices.IndexFunc(actual.Settings, func(setting ownerapi.Setting) bool { return setting.Name == want.Name })
		if index < 0 || !reflect.DeepEqual(actual.Settings[index], want) {
			t.Fatalf("golden differs from native setting projection: %s", want.Name)
		}
	}
}

func TestYardRPCGoldenFrames(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repositoryRoot(t), "api/yard-rpc/v1/fixtures/*.json"))
	if err != nil || len(files) < 4 {
		t.Fatalf("fixtures=%v err=%v", files, err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err := json.Unmarshal(payload, &value); err != nil {
				t.Fatal(err)
			}
			var frame bytes.Buffer
			if err := rpc.NewCodec(nil, &frame).Write(value); err != nil {
				t.Fatal(err)
			}
			golden, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".frame")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frame.Bytes(), golden) || int(binary.BigEndian.Uint32(golden[:4])) != len(golden)-4 {
				t.Fatal("golden frame differs from production codec")
			}
			decoded, err := rpc.NewCodec(bytes.NewReader(golden), nil).ReadResponse()
			if err != nil || decoded.Version != 1 {
				t.Fatalf("response=%#v err=%v", decoded, err)
			}
			var typed any
			switch filepath.Base(path) {
			case "profile-list.json":
				typed = &ownerapi.ProfileList{}
			case "settings-list.json":
				typed = &ownerapi.SettingsList{}
			case "host-sync-status.json":
				typed = &ownerapi.HostSyncStatus{}
			case "context.json":
				typed = &domain.Context{}
			case "owner-inventory.json":
				typed = &domain.OwnerInventory{}
			case "operation-exact.json":
				typed = &exactOperationPlan{}
			case "session-shell.json", "session-vscode.json":
				typed = &ownerapi.SessionPreparation{}
			}
			if typed != nil {
				encoded, _ := json.Marshal(decoded.Result)
				decoder := json.NewDecoder(bytes.NewReader(encoded))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(typed); err != nil {
					t.Fatal(err)
				}
				roundTrip, err := json.Marshal(typed)
				if err != nil {
					t.Fatal(err)
				}
				var shape any
				if err := json.Unmarshal(roundTrip, &shape); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(shape, decoded.Result) {
					t.Fatal("golden result differs from native DTO shape")
				}
				if exact, ok := typed.(*exactOperationPlan); ok {
					if exact.StepSchema != 1 || len(exact.Plan.Steps) == 0 {
						t.Fatal("exact golden lacks complete steps")
					}
					if err := domain.ValidateOperationSteps(exact.Plan.Steps); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
