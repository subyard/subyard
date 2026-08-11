package ownerinventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/rpc"
)

type errorTransport struct{ err error }

func (transport errorTransport) Call(context.Context, string, []byte) ([]byte, error) {
	return nil, transport.err
}

type responseTransport struct {
	payload []byte
	calls   int
}

func (transport *responseTransport) Call(context.Context, string, []byte) ([]byte, error) {
	transport.calls++
	return transport.payload, nil
}

func framedResponses(t *testing.T, capabilities []string, result any) []byte {
	t.Helper()
	var output bytes.Buffer
	codec := rpc.NewCodec(bytes.NewReader(nil), &output)
	if err := codec.Write(rpc.Response{
		Version: rpc.ProtocolVersion, Type: "response", ID: "negotiate",
		Result: map[string]any{"capabilities": capabilities},
	}); err != nil {
		t.Fatal(err)
	}
	if err := codec.Write(rpc.Response{
		Version: rpc.ProtocolVersion, Type: "response", ID: "inventory", Result: result,
	}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestClientValidatesCapabilityAndExpectedHostID(t *testing.T) {
	inventory := fixtureInventory("owner-a", time.Now())
	transport := &responseTransport{payload: framedResponses(t, []string{Capability}, inventory)}
	result, err := (Client{Transport: transport}).Fetch(context.Background(), "owner-a")
	if err != nil || result.HostID != "owner-a" || transport.calls != 1 {
		t.Fatalf("valid fetch failed: result=%#v err=%v calls=%d", result, err, transport.calls)
	}
	if _, err := (Client{Transport: transport}).Fetch(context.Background(), "owner-b"); err == nil || !strings.Contains(err.Error(), "HostID mismatch") {
		t.Fatalf("HostID mismatch was accepted: %v", err)
	}
}

func TestClientRequiresVerifiedConnectionTrustForRenameCandidate(t *testing.T) {
	inventory := fixtureInventory("owner-b", time.Now())
	transport := &responseTransport{payload: framedResponses(t, []string{Capability}, inventory)}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{
		HostID: "owner-a", Destination: "dev@owner.example", Trust: &trust,
	}
	if _, err := (Client{Transport: transport}).FetchForConnection(context.Background(), connection); err == nil || !strings.Contains(err.Error(), "verified SSH host trust") {
		t.Fatalf("unverified transport produced a rename candidate: %v", err)
	}
	result, err := (Client{
		Transport: transport, verifiedFingerprint: trust.Fingerprint,
	}).FetchForConnection(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if result.Inventory.HostID != "owner-b" || result.Rename == nil ||
		result.Rename.ExpectedHostID != "owner-a" || result.Rename.ObservedHostID != "owner-b" ||
		result.Rename.Destination != "dev@owner.example" ||
		result.Rename.TrustFingerprint != trust.Fingerprint {
		t.Fatalf("unexpected trusted fetch result: %#v", result)
	}
}

func TestRefreshConnectionAdoptsTrustedOwnerRename(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{HostID: "owner-a", Destination: "dev@owner.example", Trust: &trust}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	inventory := fixtureInventory("owner-b", time.Now())
	transport := &responseTransport{payload: framedResponses(t, []string{Capability}, inventory)}
	result, err := RefreshConnection(
		context.Background(), Client{
			Transport: transport, verifiedFingerprint: trust.Fingerprint,
		}, store, connection, time.Unix(789, 0).UTC(),
	)
	if err != nil || result.HostID != "owner-b" {
		t.Fatalf("refresh = %#v err=%v", result, err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].HostID != "owner-b" {
		t.Fatalf("refresh did not adopt rename: %#v err=%v", records, err)
	}
}

func TestConnectionsBuildTrustedSSHClientFromPersistedTrust(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{
		HostID: "owner-a", Destination: "owner-alias", Trust: &trust,
	}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "ssh")
	arguments := filepath.Join(t.TempDir(), "arguments")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SSH_ARGUMENT_LOG\"\nexit 17\n"
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := store.TrustedSSHClient(connection, program, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport.(*managedTrustTransport).environment = append(
		os.Environ(), "SSH_ARGUMENT_LOG="+arguments,
	)
	if _, err := client.Transport.Call(context.Background(), "", nil); err == nil {
		t.Fatal("fake SSH failure was hidden")
	}
	payload, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, required := range []string{
		"StrictHostKeyChecking=yes", "GlobalKnownHostsFile=/dev/null", "UpdateHostKeys=no",
	} {
		if !strings.Contains(text, required+"\n") {
			t.Fatalf("trusted client omitted %q: %q", required, text)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(root, "tmp", ".ssh-trust-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary trust state leaked: %v, %v", matches, err)
	}
}

func TestManagedSSHTransportClassifiesChangedHostKeyAsIntegrityFailure(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(program, []byte(
		"#!/bin/sh\nprintf '%s\\n' 'REMOTE HOST IDENTIFICATION HAS CHANGED' >&2\nexit 255\n",
	), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := store.TrustedSSHClient(connection, program, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Transport.Call(context.Background(), "", nil); !errors.Is(err, ErrIntegrity) ||
		!strings.Contains(err.Error(), trust.Fingerprint) || !strings.Contains(err.Error(), "yard host repair") {
		t.Fatalf("changed SSH key was not classified as integrity failure: %v", err)
	}
}

func TestChangedSSHKeyRefreshDoesNotMutateConnectionCacheOrRouting(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
	if err := store.Register(connection, snapshot); err != nil {
		t.Fatal(err)
	}
	routing := store.routingPath("owner-a")
	if err := os.MkdirAll(routing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routing, "state"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connectionBefore, err := os.ReadFile(filepath.Join(root, "connections", "owner-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	cacheBefore, err := os.ReadFile((Cache{Root: root}).path("owner-a"))
	if err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(program, []byte(
		"#!/bin/sh\nprintf '%s\\n' 'REMOTE HOST IDENTIFICATION HAS CHANGED' >&2\nexit 255\n",
	), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := store.TrustedSSHClient(connection, program, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshConnection(context.Background(), client, store, connection, time.Now()); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), trust.Fingerprint) ||
		!strings.Contains(err.Error(), "yard host repair") {
		t.Fatalf("changed key refresh = %v", err)
	}
	connectionAfter, err := os.ReadFile(filepath.Join(root, "connections", "owner-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	cacheAfter, err := os.ReadFile((Cache{Root: root}).path("owner-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(connectionBefore, connectionAfter) || !bytes.Equal(cacheBefore, cacheAfter) {
		t.Fatal("changed SSH key mutated connection or cache")
	}
	if payload, err := os.ReadFile(filepath.Join(routing, "state")); err != nil || string(payload) != "kept\n" {
		t.Fatalf("changed SSH key mutated routing: %q, %v", payload, err)
	}
}

func TestAssessSSHHostKeyReadsTheKeyCapturedByOpenSSH(t *testing.T) {
	trust := testSSHHostTrust(t, "owner-alias")
	program := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
known_hosts=
previous=
for argument do
  if [ "$previous" = -o ]; then
    case "$argument" in UserKnownHostsFile=*) known_hosts=${argument#*=} ;; esac
  fi
  previous=$argument
done
[ -n "$known_hosts" ] || exit 99
printf '%s\n' "$ASSESS_LINE" > "$known_hosts"
[ "$(stat -c %a "$known_hosts")" = 600 ] || exit 98
exit 255
`
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSESS_LINE", trust.KnownHostsLine)
	assessed, err := AssessSSHHostKey(
		context.Background(), t.TempDir(), program, "owner-alias", time.Second,
	)
	if err != nil || assessed.Fingerprint != trust.Fingerprint {
		t.Fatalf("assessment = %#v, %v", assessed, err)
	}
}

func TestClientRejectsMissingCapabilityAndMalformedInventory(t *testing.T) {
	inventory := fixtureInventory("owner-a", time.Now())
	missing := &responseTransport{payload: framedResponses(t, []string{"yard-status"}, inventory)}
	if _, err := (Client{Transport: missing}).Fetch(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "update Subyard") {
		t.Fatalf("missing capability was accepted: %v", err)
	}
	malformed := map[string]any{}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &malformed); err != nil {
		t.Fatal(err)
	}
	malformed["hostId"] = "../escape"
	bad := &responseTransport{payload: framedResponses(t, []string{Capability}, malformed)}
	if _, err := (Client{Transport: bad}).Fetch(context.Background(), ""); err == nil {
		t.Fatal("malformed owner inventory was accepted")
	}
}

func TestClientRejectsTimeoutAndOversizedResponse(t *testing.T) {
	if _, err := (Client{Transport: errorTransport{err: context.DeadlineExceeded}}).
		Fetch(context.Background(), "owner-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transport timeout was not preserved: %v", err)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, rpc.MaxFrameSize+1)
	if _, err := (Client{Transport: &responseTransport{payload: header}}).
		Fetch(context.Background(), "owner-a"); err == nil ||
		!strings.Contains(err.Error(), "outside the allowed range") {
		t.Fatalf("oversized RPC response was accepted: %v", err)
	}
}
