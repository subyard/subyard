package remotecontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/shellquote"
)

const fixturePublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture"

func TestLookupAndListReadRegistryAndCache(t *testing.T) {
	runtime := remoteFixture(t)
	contextPath := filepath.Join(runtime.ConfigHome, "yards", "demo", "config.env")
	writeRemoteFile(t, contextPath, strings.Join([]string{
		"ACCESS_KIND=remote",
		"OWNER_ENDPOINT=owner.example",
		"OWNER_YARD_NAME=inner",
		"REMOTE_SSH_PORT=2233",
		"",
	}, "\n"), 0o600)
	writeRemoteFile(t, filepath.Join(runtime.ConfigHome, "yards", "local", "config.env"),
		"ACCESS_KIND=local\nSSH_PORT=2244\n", 0o600)
	writeRemoteFile(t, runtime.cachePath("demo"),
		"100\n{\"state\":\"RUNNING\",\"sshPort\":2233}\n", 0o600)

	record, exists, err := runtime.Lookup(context.Background(), "demo")
	if err != nil || !exists || !record.Remote || record.Path != contextPath ||
		record.Spec.OwnerEndpoint != "owner.example" || record.Spec.OwnerYardName != "inner" ||
		record.SSHPort != 2233 || !record.LastProbe.Equal(time.Unix(100, 0)) {
		t.Fatalf("unexpected lookup: record=%#v exists=%v err=%v", record, exists, err)
	}
	records, err := runtime.List(context.Background())
	if err != nil || len(records) != 1 || records[0].Spec.LegacyAlias != "demo" {
		t.Fatalf("unexpected remote list: %#v err=%v", records, err)
	}
	defaultRecord, exists, err := runtime.Lookup(context.Background(), "default")
	if err != nil || !exists || defaultRecord.Spec.LegacyAlias != "default" {
		t.Fatalf("default lookup failed: %#v exists=%v err=%v", defaultRecord, exists, err)
	}
}

func TestLookupFallsBackFromSymlinkedHigherPrecedenceEntry(t *testing.T) {
	runtime := remoteFixture(t)
	target := filepath.Join(t.TempDir(), "target.env")
	writeRemoteFile(t, target, "ACCESS_KIND=remote\nOWNER_ENDPOINT=unsafe.example\n", 0o600)
	nested := filepath.Join(runtime.ConfigHome, "yards", "demo", "config.env")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, nested); err != nil {
		t.Fatal(err)
	}
	privateFlat := filepath.Join(runtime.ConfigDir, "..", "private", "yards", "demo.env")
	writeRemoteFile(t, privateFlat, "ACCESS_KIND=remote\nOWNER_ENDPOINT=owner.example\n", 0o600)

	record, exists, err := runtime.Lookup(context.Background(), "demo")
	if err != nil || !exists || record.Path != privateFlat ||
		record.Spec.OwnerEndpoint != "owner.example" {
		t.Fatalf("unexpected fallback lookup: record=%#v exists=%v err=%v", record, exists, err)
	}
}

func TestApplyAddWritesIsolatedContextAndVerifiesPinnedKey(t *testing.T) {
	runtime := remoteFixture(t)
	writeRemoteFile(t, runtime.sshConfigPath(), "Host local\n", 0o600)
	writeRemoteFile(t, runtime.knownHostsPath(), "local.example "+fixturePublicKey+"\n", 0o600)
	key := remoteFixtureKey(t, "subyard-remote-demo")
	var calls [][]string
	runtime.processCall = func(_ context.Context, _ string, arguments []string, stdin []byte) ([]byte, error) {
		calls = append(calls, append([]string(nil), arguments...))
		joined := strings.Join(arguments, " ")
		switch {
		case strings.Contains(joined, "_authorize"):
			if string(stdin) != strings.TrimSuffix(fixturePublicKey, " fixture")+"\n" {
				t.Fatalf("unexpected authorization key: %q", stdin)
			}
		case strings.Contains(joined, "yard-demo") && strings.HasSuffix(joined, " -- true"):
			payload, err := os.ReadFile(runtime.knownHostsPath())
			if err != nil {
				t.Fatal(err)
			}
			payload = append(payload, []byte("subyard-remote-demo "+fixturePublicKey+"\n")...)
			if err := os.WriteFile(runtime.knownHostsPath(), payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil, nil
	}
	projects := 2
	prepared := domain.RemotePrepared{
		Action: domain.RemoteAdd,
		Spec: domain.RemoteSpec{
			LegacyAlias: "demo", OwnerEndpoint: "owner.example", OwnerYardName: "inner",
		},
		Owner: domain.RemoteInfo{
			State: "RUNNING", SSHPort: 2233, DevUser: "dev", Projects: &projects,
		},
		Scanned: []domain.RemoteKey{key},
	}
	result, err := runtime.Apply(context.Background(), prepared)
	if err != nil || !strings.Contains(result.Message, "yard -Y demo sync") {
		t.Fatalf("remote add failed: result=%#v err=%v", result, err)
	}
	if len(calls) != 2 {
		t.Fatalf("unexpected remote process calls: %#v", calls)
	}
	ownerCall := strings.Join(calls[0], " ")
	if !strings.Contains(ownerCall, "bash -lc") ||
		!strings.Contains(ownerCall, "yard") || !strings.Contains(ownerCall, "-Y") ||
		!strings.Contains(ownerCall, "inner") || !strings.Contains(ownerCall, "_authorize") {
		t.Fatalf("owner call did not use the login-shell contract: %#v", calls)
	}
	assertRemoteFileContains(t, filepath.Join(runtime.ConfigHome, "yards", "demo", "config.env"),
		"OWNER_ENDPOINT=owner.example")
	assertRemoteFileContains(t, filepath.Join(runtime.ConfigHome, "yards", "demo", "config.env"),
		"OWNER_YARD_NAME=inner")
	assertRemoteFileContains(t, runtime.snippetPath("demo"), "HostKeyAlias subyard-remote-demo")
	assertRemoteFileContains(t, runtime.sshConfigPath(), "Include subyard-demo.config")
	assertRemoteFileContains(t, runtime.knownHostsPath(), "local.example ")
	assertRemoteFileContains(t, runtime.knownHostsPath(), "subyard-remote-demo ")
	if _, _, err := runtime.readCache("demo"); err != nil {
		t.Fatalf("remote cache was not committed: %v", err)
	}
}

func TestApplyAddRollsBackEveryLocalFileOnProbeFailure(t *testing.T) {
	runtime := remoteFixture(t)
	spec := domain.RemoteSpec{LegacyAlias: "demo", OwnerEndpoint: "owner.example"}
	envPath := filepath.Join(runtime.ConfigHome, "yards", "demo", "config.env")
	existing := domain.RemoteRecord{Spec: spec, Remote: true, Path: envPath, SSHPort: 2222}
	paths := []string{
		envPath,
		runtime.snippetPath("demo"),
		runtime.sshConfigPath(),
		runtime.knownHostsPath(),
		runtime.cachePath("demo"),
	}
	original := make(map[string][]byte, len(paths))
	for index, path := range paths {
		payload := []byte("original-" + string(rune('a'+index)) + "\n")
		if path == envPath {
			payload = renderContext(domain.RemotePrepared{
				Spec: spec, Owner: domain.RemoteInfo{SSHPort: 2222, DevUser: "dev"},
			}, nil)
		}
		writeRemoteFile(t, path, string(payload), 0o600)
		original[path] = append([]byte(nil), payload...)
	}
	runtime.processCall = func(_ context.Context, _ string, arguments []string, _ []byte) ([]byte, error) {
		if strings.Contains(strings.Join(arguments, " "), "yard-demo") {
			return nil, errors.New("Permission denied")
		}
		return nil, nil
	}
	projects := 1
	_, err := runtime.Apply(context.Background(), domain.RemotePrepared{
		Action: domain.RemoteAdd, Spec: spec, Existing: &existing,
		Owner:   domain.RemoteInfo{State: "RUNNING", SSHPort: 2222, DevUser: "dev", Projects: &projects},
		Scanned: []domain.RemoteKey{remoteFixtureKey(t, "subyard-remote-demo")},
	})
	if err == nil || !strings.Contains(err.Error(), "rejected the controller key") {
		t.Fatalf("probe failure was not classified: %v", err)
	}
	for _, path := range paths {
		payload, readErr := os.ReadFile(path)
		if readErr != nil || string(payload) != string(original[path]) {
			t.Fatalf("transaction did not restore %s: payload=%q err=%v", path, payload, readErr)
		}
	}
}

func TestApplySerializesMutationsForTheSameOperatorHome(t *testing.T) {
	first := remoteFixture(t)
	second := first
	firstEntered := make(chan struct{}, 1)
	secondEntered := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	first.processCall = func(context.Context, string, []string, []byte) ([]byte, error) {
		firstEntered <- struct{}{}
		<-releaseFirst
		return nil, errors.New("first authorization stopped")
	}
	second.processCall = func(context.Context, string, []string, []byte) ([]byte, error) {
		secondEntered <- struct{}{}
		return nil, errors.New("second authorization stopped")
	}
	prepared := func(alias string) domain.RemotePrepared {
		return domain.RemotePrepared{
			Action: domain.RemoteAdd,
			Spec:   domain.RemoteSpec{LegacyAlias: alias, OwnerEndpoint: "owner.example"},
		}
	}
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		_, err := first.Apply(context.Background(), prepared("first"))
		firstDone <- err
	}()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first mutation did not reach owner authorization")
	}
	go func() {
		_, err := second.Apply(context.Background(), prepared("second"))
		secondDone <- err
	}()
	early := false
	select {
	case <-secondEntered:
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	for index, done := range []<-chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "authorization stopped") {
				t.Fatalf("mutation %d returned %v", index, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("mutation %d did not finish", index)
		}
	}
	if early {
		t.Fatal("second mutation entered owner authorization before the first mutation finished")
	}
}

func TestRemoteMutationLockStopsWaitingWhenContextEnds(t *testing.T) {
	runtime := remoteFixture(t)
	unlock, err := runtime.lockMutation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		secondUnlock, err := runtime.lockMutation(ctx)
		if err == nil {
			secondUnlock()
		}
		done <- err
	}()
	var waitErr error
	returnedBeforeUnlock := false
	select {
	case waitErr = <-done:
		returnedBeforeUnlock = true
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if !returnedBeforeUnlock {
		select {
		case waitErr = <-done:
		case <-time.After(time.Second):
			t.Fatal("lock waiter did not finish after release")
		}
	}
	if !returnedBeforeUnlock || !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("lock wait ignored context: returned=%v err=%v", returnedBeforeUnlock, waitErr)
	}
}

func TestApplyRejectsCanceledMutationBeforeChangingFiles(t *testing.T) {
	runtime := remoteFixture(t)
	spec := domain.RemoteSpec{LegacyAlias: "demo", OwnerEndpoint: "owner.example"}
	envPath := filepath.Join(runtime.ConfigHome, "yards", "demo", "config.env")
	payload := renderContext(domain.RemotePrepared{
		Spec: spec, Owner: domain.RemoteInfo{SSHPort: 2222, DevUser: "dev"},
	}, nil)
	writeRemoteFile(t, envPath, string(payload), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runtime.Apply(ctx, domain.RemotePrepared{
		Action: domain.RemoteRemove,
		Spec:   spec,
		Existing: &domain.RemoteRecord{
			Spec: spec, Remote: true, Path: envPath, SSHPort: 2222,
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation returned %v", err)
	}
	current, readErr := os.ReadFile(envPath)
	if readErr != nil || string(current) != string(payload) {
		t.Fatalf("canceled mutation changed the remote context: payload=%q err=%v", current, readErr)
	}
}

func TestApplyRemoveDeletesOnlyTheSelectedRemote(t *testing.T) {
	runtime := remoteFixture(t)
	spec := domain.RemoteSpec{LegacyAlias: "demo", OwnerEndpoint: "owner.example"}
	envPath := filepath.Join(runtime.ConfigHome, "yards", "demo", "config.env")
	existing := domain.RemoteRecord{Spec: spec, Remote: true, Path: envPath, SSHPort: 2222}
	writeRemoteFile(t, envPath, string(renderContext(domain.RemotePrepared{
		Spec: spec, Owner: domain.RemoteInfo{SSHPort: 2222, DevUser: "dev"},
	}, nil)), 0o600)
	writeRemoteFile(t, runtime.snippetPath("demo"), "demo\n", 0o600)
	writeRemoteFile(t, runtime.sshConfigPath(),
		"Include subyard-demo.config\nInclude subyard-other.config\n", 0o600)
	writeRemoteFile(t, runtime.knownHostsPath(),
		"subyard-remote-demo "+fixturePublicKey+"\nsubyard-remote-other "+fixturePublicKey+"\n", 0o600)
	writeRemoteFile(t, runtime.cachePath("demo"), "cache\n", 0o600)

	if _, err := runtime.Apply(context.Background(), domain.RemotePrepared{
		Action: domain.RemoteRemove, Spec: spec, Existing: &existing,
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{envPath, runtime.snippetPath("demo"), runtime.cachePath("demo")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("selected remote file survived: %s err=%v", path, err)
		}
	}
	assertRemoteFileContains(t, runtime.sshConfigPath(), "Include subyard-other.config")
	assertRemoteFileNotContains(t, runtime.sshConfigPath(), "Include subyard-demo.config")
	assertRemoteFileContains(t, runtime.knownHostsPath(), "subyard-remote-other ")
	assertRemoteFileNotContains(t, runtime.knownHostsPath(), "subyard-remote-demo ")
}

func TestTransactionalRestoresExistingFilesAndRemovesNewFiles(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	created := filepath.Join(root, "created")
	writeRemoteFile(t, existing, "before", 0o640)
	sentinel := errors.New("apply failed")
	err := transactional([]string{existing, created}, func() error {
		writeRemoteFile(t, existing, "after", 0o600)
		writeRemoteFile(t, created, "new", 0o600)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("transaction lost the primary error: %v", err)
	}
	payload, err := os.ReadFile(existing)
	if err != nil || string(payload) != "before" {
		t.Fatalf("existing file was not restored: %q err=%v", payload, err)
	}
	info, err := os.Stat(existing)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("existing mode was not restored: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new file survived rollback: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := transactional([]string{filepath.Join(root, "directory")}, func() error { return nil }); err == nil {
		t.Fatal("transaction accepted a non-regular target")
	}
}

func TestRenderAndKeyHelpersPreserveUserState(t *testing.T) {
	prepared := domain.RemotePrepared{
		Spec:  domain.RemoteSpec{LegacyAlias: "demo", OwnerEndpoint: "owner", OwnerYardName: "inner"},
		Owner: domain.RemoteInfo{SSHPort: 2233, DevUser: "dev"},
	}
	current := []byte("ACCESS_KIND=remote\nREMOTE_DEST=old\nFORWARD_SSH_AGENT=1\nCUSTOM=value\n")
	rendered := string(renderContext(prepared, current))
	for _, expected := range []string{
		"OWNER_ENDPOINT=owner", "OWNER_YARD_NAME=inner", "REMOTE_SSH_PORT=2233",
		"FORWARD_SSH_AGENT=0", "FORWARD_SSH_AGENT=1", "CUSTOM=value",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered context omitted %q:\n%s", expected, rendered)
		}
	}
	if strings.Count(rendered, "CUSTOM=value") != 1 || strings.Contains(rendered, "OWNER_ENDPOINT=old") {
		t.Fatalf("rendered context did not separate managed state from overrides:\n%s", rendered)
	}

	payload := []byte(strings.Join([]string{
		"host-a,subyard-remote-demo " + fixturePublicKey,
		"subyard-remote-demo " + fixturePublicKey,
		"subyard-remote-other " + fixturePublicKey,
		"malformed",
		"",
	}, "\n"))
	keys, err := parseKeys(payload, "subyard-remote-demo")
	if err != nil || len(keys) != 1 || keys[0].Fingerprint == "" {
		t.Fatalf("known-host parsing drifted: %#v err=%v", keys, err)
	}
	remaining := string(removeKnownHost(payload, "subyard-remote-demo"))
	if strings.Contains(remaining, "host-a,subyard-remote-demo") ||
		!strings.Contains(remaining, "subyard-remote-other ") {
		t.Fatalf("known-host removal crossed an alias boundary:\n%s", remaining)
	}
}

func TestCachePreservesLastNumericProjectCount(t *testing.T) {
	runtime := remoteFixture(t)
	projects := 7
	if err := runtime.writeCache(runtime.cachePath("demo"),
		domain.RemoteInfo{State: "RUNNING", Projects: &projects}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.writeCache(runtime.cachePath("demo"),
		domain.RemoteInfo{State: "RUNNING"}); err != nil {
		t.Fatal(err)
	}
	info, _, err := runtime.readCache("demo")
	if err != nil || info.Projects == nil || *info.Projects != projects {
		t.Fatalf("cached project count was lost: %#v err=%v", info, err)
	}
}

func TestProbeDiagnosticsAndShellQuoting(t *testing.T) {
	for message, expected := range map[string]string{
		"Permission denied":                           "rejected the controller key",
		"REMOTE HOST IDENTIFICATION HAS CHANGED":      "host key changed",
		"stdio forwarding failed: Connection refused": "loopback proxy",
		"unexpected": "data-plane probe failed",
	} {
		if got := classifyProbe(errors.New(message)).Error(); !strings.Contains(got, expected) {
			t.Fatalf("classifyProbe(%q) = %q, want %q", message, got, expected)
		}
	}
	command := shellquote.Command([]string{"yard", "-Y", "owner's-yard", "_info"})
	if command != "'yard' '-Y' 'owner'\\''s-yard' '_info'" {
		t.Fatalf("unsafe shell command: %q", command)
	}
}

func remoteFixture(t *testing.T) Runtime {
	t.Helper()
	root := t.TempDir()
	public := filepath.Join(root, "controller.pub")
	writeRemoteFile(t, public, fixturePublicKey+"\n", 0o600)
	return Runtime{
		Home:       filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config-home"),
		ConfigDir:  filepath.Join(root, "config"),
		DataHome:   filepath.Join(root, "data"),
		PublicKey:  public,
		Timeout:    time.Second,
	}
}

func remoteFixtureKey(t *testing.T, host string) domain.RemoteKey {
	t.Helper()
	keys, err := parseKeys([]byte(host+" "+fixturePublicKey+"\n"), host)
	if err != nil || len(keys) != 1 {
		t.Fatalf("create fixture key: %#v err=%v", keys, err)
	}
	return keys[0]
}

func writeRemoteFile(t *testing.T, path, payload string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(payload), mode); err != nil {
		t.Fatal(err)
	}
}

func assertRemoteFileContains(t *testing.T, path, expected string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(payload), expected) {
		t.Fatalf("%s does not contain %q: %q err=%v", path, expected, payload, err)
	}
}

func assertRemoteFileNotContains(t *testing.T, path, expected string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(payload), expected) {
		t.Fatalf("%s unexpectedly contains %q: %q err=%v", path, expected, payload, err)
	}
}
