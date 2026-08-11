package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/crypto/ssh"
)

func hostAddSSHFixture(t *testing.T, inventory any) (string, ownerinventory.SSHHostTrust) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := ownerinventory.NewSSHHostTrust(
		"owner-alias " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
	)
	if err != nil {
		t.Fatal(err)
	}
	var framed bytes.Buffer
	codec := rpc.NewCodec(bytes.NewReader(nil), &framed)
	for _, response := range []rpc.Response{
		{Version: rpc.ProtocolVersion, Type: "response", ID: "negotiate",
			Result: map[string]any{"capabilities": []string{ownerinventory.Capability}}},
		{Version: rpc.ProtocolVersion, Type: "response", ID: "inventory", Result: inventory},
	} {
		if err := codec.Write(response); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	responsePath := filepath.Join(bin, "response")
	if err := os.WriteFile(responsePath, framed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(bin, "ssh")
	script := `#!/bin/sh
assessment=0
known_hosts=
previous=
for argument do
  if [ "$previous" = -o ]; then
    case "$argument" in
      PreferredAuthentications=none) assessment=1 ;;
      UserKnownHostsFile=*) known_hosts=${argument#*=} ;;
    esac
  fi
  previous=$argument
done
if [ "$assessment" = 1 ]; then
  printf '%s\n' "$SSH_ASSESS_LINE" > "$known_hosts"
  chmod 600 "$known_hosts"
  exit 255
fi
cat "$SSH_RPC_RESPONSE"
`
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SSH_ASSESS_LINE", trust.KnownHostsLine)
	t.Setenv("SSH_RPC_RESPONSE", responsePath)
	return bin, trust
}

func writeHostRPCFixture(t *testing.T, path string, inventory any) {
	t.Helper()
	var framed bytes.Buffer
	codec := rpc.NewCodec(bytes.NewReader(nil), &framed)
	for _, response := range []rpc.Response{
		{Version: rpc.ProtocolVersion, Type: "response", ID: "negotiate", Result: map[string]any{"capabilities": []string{ownerinventory.Capability}}},
		{Version: rpc.ProtocolVersion, Type: "response", ID: "inventory", Result: inventory},
	} {
		if err := codec.Write(response); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, framed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostAddPinsFingerprintAndRegistersInitialSnapshotTogether(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	inventory := inventoryResult("remote-owner", "default", "").inventory
	_, trust := hostAddSSHFixture(t, inventory)
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "add", "owner-alias"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("host add failed: code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Seen) != 1 || !strings.Contains(prompt.Seen[0], trust.Fingerprint) ||
		!strings.Contains(prompt.Seen[0], "remote-owner") {
		t.Fatalf("host add omitted exact trust/identity plan: %#v", prompt.Seen)
	}
	dataRoot := filepath.Join(home, ".subyard", "owner-inventory")
	records, err := (ownerinventory.Connections{Root: dataRoot}).List()
	if err != nil || len(records) != 1 || records[0].Trust == nil ||
		records[0].Trust.Fingerprint != trust.Fingerprint {
		t.Fatalf("registered connection = %#v, %v", records, err)
	}
	snapshot, err := (ownerinventory.Cache{Root: dataRoot}).Read("remote-owner")
	if err != nil || snapshot.FetchedAt.IsZero() ||
		time.Since(snapshot.FetchedAt) > time.Minute {
		t.Fatalf("initial snapshot = %#v, %v", snapshot, err)
	}
}

func TestHostAddUpgradesLegacyDiscoveryToManagedTrust(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	inventory := inventoryResult("remote-owner", "default", "").inventory
	_, trust := hostAddSSHFixture(t, inventory)
	dataRoot := filepath.Join(home, ".subyard", "owner-inventory")
	store := ownerinventory.Connections{Root: dataRoot}
	if err := store.RegisterLegacy(ownerinventory.Connection{
		HostID: "remote-owner", Destination: "owner-alias",
	}, ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: inventory}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"host", "add", "owner-alias", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("legacy upgrade failed: code=%d stderr=%s", code, stderr.String())
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].Trust == nil ||
		records[0].Trust.Fingerprint != trust.Fingerprint {
		t.Fatalf("legacy upgrade = %#v, %v", records, err)
	}
}

func TestHostAddEOFDoesNotMutateState(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	hostAddSSHFixture(t, inventoryResult("remote-owner", "default", "").inventory)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "add", "owner-alias"},
		Environment: environment, WorkingDir: root, Stdin: bytes.NewReader(nil),
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("EOF registration returned %d: %s", code, stderr.String())
	}
	records, err := (ownerinventory.Connections{Root: filepath.Join(home, ".subyard", "owner-inventory")}).List()
	if err != nil || len(records) != 0 {
		t.Fatalf("EOF registration mutated state: %#v, %v", records, err)
	}
}

func TestHostAddCollisionIsRejectedBeforePrompt(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	inventory := inventoryResult("remote-owner", "default", "").inventory
	hostAddSSHFixture(t, inventory)
	dataRoot := filepath.Join(home, ".subyard", "owner-inventory")
	if err := os.MkdirAll(filepath.Join(dataRoot, "routing", "remote-owner"), 0o700); err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments: []string{"host", "add", "owner-alias"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || len(prompt.Seen) != 0 {
		t.Fatalf("collision reached prompt: code=%d prompts=%#v stderr=%s", code, prompt.Seen, stderr.String())
	}
}

func TestHostRepairShowsKeyAndHostIDChangeBeforeApplying(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	dataRoot := filepath.Join(home, ".subyard", "owner-inventory")
	oldInventory := inventoryResult("owner-a", "default", "").inventory
	_, oldTrust := hostAddSSHFixture(t, oldInventory)
	store := ownerinventory.Connections{Root: dataRoot}
	if err := store.Register(ownerinventory.Connection{
		HostID: "owner-a", Destination: "owner-alias", Trust: &oldTrust,
	}, ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: oldInventory}); err != nil {
		t.Fatal(err)
	}
	newInventory := inventoryResult("owner-b", "default", "").inventory
	_, newTrust := hostAddSSHFixture(t, newInventory)
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "repair", "owner-a"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("cancelled repair returned %d: %s", code, stderr.String())
	}
	if len(prompt.Seen) != 1 || !strings.Contains(prompt.Seen[0], "owner-a") ||
		!strings.Contains(prompt.Seen[0], "owner-b") ||
		!strings.Contains(prompt.Seen[0], oldTrust.Fingerprint) ||
		!strings.Contains(prompt.Seen[0], newTrust.Fingerprint) {
		t.Fatalf("repair omitted exact identity/key transition: %#v", prompt.Seen)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].HostID != "owner-a" ||
		records[0].Trust.Fingerprint != oldTrust.Fingerprint {
		t.Fatalf("cancelled repair mutated connection: %#v, %v", records, err)
	}

	prompt = &testkit.Prompt{Answers: []bool{true}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "repair", "owner-a"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("confirmed repair failed: code=%d stderr=%s", code, stderr.String())
	}
	records, err = store.List()
	if err != nil || len(records) != 1 || records[0].HostID != "owner-b" ||
		records[0].Trust.Fingerprint != newTrust.Fingerprint {
		t.Fatalf("confirmed repair did not migrate identity/trust: %#v, %v", records, err)
	}
}

func TestHostRemoveUsesAuthoritativeInventoryBeforePrompt(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	empty := inventoryResult("owner-a", "default", "").inventory
	_, trust := hostAddSSHFixture(t, empty)
	dataRoot := filepath.Join(home, ".subyard", "owner-inventory")
	store := ownerinventory.Connections{Root: dataRoot}
	if err := store.Register(ownerinventory.Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}, ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: empty}); err != nil {
		t.Fatal(err)
	}
	withProject := inventoryResult("owner-a", "default", "Demo").inventory
	writeHostRPCFixture(t, os.Getenv("SSH_RPC_RESPONSE"), withProject)
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "remove", "owner-a"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || len(prompt.Seen) != 0 {
		t.Fatalf("authoritative project reached removal prompt: code=%d prompts=%#v stderr=%s", code, prompt.Seen, stderr.String())
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("rejected removal mutated registration: %#v, %v", records, err)
	}
}

func TestHostRemoveAllowsMissingCacheAfterAuthoritativeEmptyInventory(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	empty := inventoryResult("owner-a", "default", "").inventory
	_, trust := hostAddSSHFixture(t, empty)
	dataRoot := filepath.Join(home, ".subyard", "owner-inventory")
	store := ownerinventory.Connections{Root: dataRoot}
	if err := store.Register(ownerinventory.Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}, ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: empty}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dataRoot, "owners", "owner-a.json")); err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "remove", "owner-a"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("removal failed: code=%d stderr=%s", code, stderr.String())
	}
	if records, err := store.List(); err != nil || len(records) != 0 {
		t.Fatalf("registration remains: %#v, %v", records, err)
	}
}

func TestHostRemoveFindsUnmergedLegacyRouteByEndpointBeforePrompt(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	empty := inventoryResult("owner-a", "default", "").inventory
	_, trust := hostAddSSHFixture(t, empty)
	store := ownerinventory.Connections{Root: filepath.Join(home, ".subyard", "owner-inventory")}
	if err := store.Register(ownerinventory.Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}, ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: empty}); err != nil {
		t.Fatal(err)
	}
	control := &remoteControlStub{records: []domain.RemoteRecord{{Spec: domain.RemoteSpec{LegacyAlias: "old-yard", OwnerEndpoint: "owner-alias"}}}}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "remove", "owner-a"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt, RemoteControl: control})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || len(prompt.Seen) != 0 || !strings.Contains(stderr.String(), "yard remote remove old-yard") {
		t.Fatalf("unmerged legacy route was not rejected: code=%d prompts=%#v stderr=%s", code, prompt.Seen, stderr.String())
	}
}

func TestHostRenamePrintsExactPlanAndAppliesAfterConfirmation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "owner-a\n", 0o600)
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "rename", "owner-b"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("host rename failed: code=%d stderr=%s", code, stderr.String())
	}
	content, err := os.ReadFile(filepath.Join(configHome, "host-id"))
	if err != nil || string(content) != "owner-b\n" {
		t.Fatalf("renamed identity = %q err=%v", content, err)
	}
	if len(prompt.Seen) != 1 || !strings.Contains(prompt.Seen[0], "owner-a") ||
		!strings.Contains(prompt.Seen[0], "owner-b") {
		t.Fatalf("rename did not present exact identity transition: %#v", prompt.Seen)
	}
	if !strings.Contains(stdout.String(), "owner-a -> owner-b") {
		t.Fatalf("rename output omitted transition: %s", stdout.String())
	}
}

func TestHostRenameRefusalLeavesIdentityUntouched(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "owner-a\n", 0o600)
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "rename", "owner-b"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("refused host rename returned %d, stderr=%s", code, stderr.String())
	}
	content, err := os.ReadFile(filepath.Join(configHome, "host-id"))
	if err != nil || string(content) != "owner-a\n" {
		t.Fatalf("refused rename mutated identity = %q err=%v", content, err)
	}
}
