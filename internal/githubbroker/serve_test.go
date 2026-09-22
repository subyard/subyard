package githubbroker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProfileEnabledDefaultsAndExplicitSelection(t *testing.T) {
	cases := []struct {
		name string
		yard string
		env  map[string]string
		want bool
	}{
		{name: "default yard", yard: "default", want: true},
		{name: "other yard", yard: "hermes", want: false},
		{name: "explicit enable", yard: "hermes", env: map[string]string{"ENVIRONMENT_PROFILES": "base github"}, want: true},
		{name: "explicit disable", yard: "default", env: map[string]string{"ENVIRONMENT_PROFILES": "base"}, want: false},
		{name: "nested vm disabled", yard: "default", env: map[string]string{"NESTED_E2E_VMS": "1", "ENVIRONMENT_PROFILES": "github"}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := ProfileEnabled(test.yard, test.env); got != test.want {
				t.Fatalf("ProfileEnabled() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRunServerRejectsInvalidTransportConfigWithoutLeakingInput(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "transport.json")
	secret := "private-key-path-with-secret"
	data, err := json.Marshal(RuntimeConfig{AppConfig: "/" + secret, SSHPort: 0, Developer: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = RunServer(context.Background(), path)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("RunServer() error = %v", err)
	}
}

func TestGuestSocketUsesDeveloperHome(t *testing.T) {
	if got, want := GuestSocket("dev"), "/home/dev/.local/share/subyard/github.sock"; got != want {
		t.Fatalf("GuestSocket() = %q, want %q", got, want)
	}
}

func TestRunServerPinnedSSHUnixForwardServesStatus(t *testing.T) {
	fixture := newSSHForwardFixture(t, false)
	defer fixture.Close()
	root := t.TempDir()
	identityPath := writeSSHPrivateKey(t, root, fixture.userPrivate)
	knownHostsPath := filepath.Join(root, "known_hosts")
	knownHosts := fmt.Sprintf("[127.0.0.1]:%d %s", fixture.port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fixture.hostSigner.PublicKey()))))
	if err := os.WriteFile(knownHostsPath, []byte(knownHosts+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "transport.json")
	config, _ := json.Marshal(RuntimeConfig{AppConfig: filepath.Join(root, "missing-app.json"), SSHPort: fixture.port, Developer: "dev", IdentityFile: identityPath, KnownHostsFile: knownHostsPath})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunServer(ctx, configPath) }()
	select {
	case err := <-fixture.forwarded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSH stream-local forward was not established")
	}
	select {
	case err := <-fixture.httpResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no HTTP response through forwarded Unix socket")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServer() after cancellation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunServer did not stop after cancellation")
	}
}

func TestRunServerSSHDisconnectExitsWithoutLeakingError(t *testing.T) {
	fixture := newSSHForwardFixture(t, true)
	defer fixture.Close()
	root := t.TempDir()
	identityPath := writeSSHPrivateKey(t, root, fixture.userPrivate)
	knownHostsPath := filepath.Join(root, "known_hosts")
	knownHosts := fmt.Sprintf("[127.0.0.1]:%d %s\n", fixture.port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fixture.hostSigner.PublicKey()))))
	if err := os.WriteFile(knownHostsPath, []byte(knownHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "transport.json")
	config, _ := json.Marshal(RuntimeConfig{AppConfig: filepath.Join(root, "missing-app.json"), SSHPort: fixture.port, Developer: "dev", IdentityFile: identityPath, KnownHostsFile: knownHostsPath})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := RunServer(context.Background(), configPath)
	if err == nil || time.Since(start) > 5*time.Second || strings.Contains(err.Error(), "private") {
		t.Fatalf("disconnect result = %v after %s", err, time.Since(start))
	}
}

func TestRunServerRejectsPinnedHostMismatch(t *testing.T) {
	fixture := newSSHForwardFixture(t, false)
	defer fixture.Close()
	root := t.TempDir()
	identityPath := writeSSHPrivateKey(t, root, fixture.userPrivate)
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongSigner, err := ssh.NewSignerFromKey(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	knownHostsPath := filepath.Join(root, "known_hosts")
	knownHosts := fmt.Sprintf("[127.0.0.1]:%d %s\n", fixture.port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(wrongSigner.PublicKey()))))
	if err := os.WriteFile(knownHostsPath, []byte(knownHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "transport.json")
	config, _ := json.Marshal(RuntimeConfig{AppConfig: filepath.Join(root, "missing-app.json"), SSHPort: fixture.port, Developer: "dev", IdentityFile: identityPath, KnownHostsFile: knownHostsPath})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	err = RunServer(context.Background(), configPath)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || strings.Contains(err.Error(), "ssh-random") {
		t.Fatalf("host mismatch result = %v", err)
	}
}

type sshForwardFixture struct {
	listener     net.Listener
	server       *ssh.ServerConfig
	connectionMu sync.Mutex
	connection   *ssh.ServerConn
	hostSigner   ssh.Signer
	userSigner   ssh.Signer
	userPrivate  ed25519.PrivateKey
	port         int
	forwarded    chan error
	httpResult   chan error
	disconnect   bool
	closeOnce    sync.Once
}

func newSSHForwardFixture(t *testing.T, disconnect bool) *sshForwardFixture {
	t.Helper()
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	userPublic, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userPrivate)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "dev" || !bytes.Equal(key.Marshal(), sshPublicKey(userPublic).Marshal()) {
			return nil, errors.New("wrong user key")
		}
		return nil, nil
	}}
	server.AddHostKey(hostSigner)
	fixture := &sshForwardFixture{listener: listener, server: server, hostSigner: hostSigner, userSigner: userSigner, userPrivate: userPrivate, port: listener.Addr().(*net.TCPAddr).Port, forwarded: make(chan error, 1), httpResult: make(chan error, 1), disconnect: disconnect}
	go fixture.serve()
	return fixture
}

func sshPublicKey(key ed25519.PublicKey) ssh.PublicKey {
	public, err := ssh.NewPublicKey(key)
	if err != nil {
		panic(err)
	}
	return public
}

func (fixture *sshForwardFixture) serve() {
	connection, err := fixture.listener.Accept()
	if err != nil {
		return
	}
	serverConn, channels, requests, err := ssh.NewServerConn(connection, fixture.server)
	if err != nil {
		return
	}
	fixture.connectionMu.Lock()
	fixture.connection = serverConn
	fixture.connectionMu.Unlock()
	go fixture.handleRequests(serverConn, requests)
	for channel := range channels {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		stream, channelRequests, err := channel.Accept()
		if err != nil {
			continue
		}
		go fixture.handleSession(stream, channelRequests)
	}
}

func (fixture *sshForwardFixture) handleRequests(connection *ssh.ServerConn, requests <-chan *ssh.Request) {
	for request := range requests {
		switch request.Type {
		case "streamlocal-forward@openssh.com", "cancel-streamlocal-forward@openssh.com":
			_ = request.Reply(true, nil)
			if request.Type == "streamlocal-forward@openssh.com" {
				fixture.forwarded <- nil
				if fixture.disconnect {
					_ = connection.Close()
					return
				}
				go fixture.injectHTTP(connection)
			}
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
}

func (fixture *sshForwardFixture) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for request := range requests {
		if request.Type == "exec" {
			_ = request.Reply(true, nil)
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		}
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
	}
}

func (fixture *sshForwardFixture) injectHTTP(connection *ssh.ServerConn) {
	time.Sleep(50 * time.Millisecond)
	payload := struct {
		SocketPath string
		Reserved0  string
	}{SocketPath: GuestSocket("dev")}
	channel, requests, err := connection.OpenChannel("forwarded-streamlocal@openssh.com", ssh.Marshal(payload))
	if err != nil {
		fixture.httpResult <- err
		return
	}
	go ssh.DiscardRequests(requests)
	_, _ = io.WriteString(channel, "GET /status HTTP/1.1\r\nHost: github-broker\r\nConnection: close\r\n\r\n")
	response, err := io.ReadAll(channel)
	if err == nil && !strings.Contains(string(response), "200 OK") {
		err = fmt.Errorf("unexpected HTTP response: %q", response)
	}
	fixture.httpResult <- err
	_ = channel.Close()
}

func (fixture *sshForwardFixture) Close() {
	fixture.closeOnce.Do(func() {
		_ = fixture.listener.Close()
		fixture.connectionMu.Lock()
		connection := fixture.connection
		fixture.connectionMu.Unlock()
		if connection != nil {
			_ = connection.Close()
		}
	})
}

func TestClientHandlerAndFakeGitHubContractOverUnixSocket(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	github := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/7/access_tokens" {
			t.Errorf("GitHub request = %s %s", request.Method, request.URL.Path)
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil || string(body) != `{}` || strings.Contains(string(body), "permissions") || strings.Contains(string(body), "repositories") {
			t.Errorf("GitHub request body = %q", body)
		}
		_, _ = io.WriteString(writer, `{"token":"ghs_joined","expires_at":"2030-01-01T00:00:00Z"}`)
	}))
	defer github.Close()
	issuer := newTestIssuer(Config{AppID: "9", InstallationID: 7}, key, github.Client(), github.URL, func() time.Time { return time.Date(2029, 12, 31, 23, 0, 0, 0, time.UTC) })
	socket := brokerServer(t, NewHandler(issuer))
	var stdout, stderr strings.Builder
	code := RunClient(context.Background(), []string{"--socket", socket, "credential", "get"}, strings.NewReader("protocol=https\nhost=github.com\n\n"), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "password=ghs_joined\n") {
		t.Fatalf("client result code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func writeSSHPrivateKey(t *testing.T, root string, key ed25519.PrivateKey) string {
	t.Helper()
	data, err := ssh.MarshalPrivateKey(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "identity")
	if err := os.WriteFile(path, pemBytes(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func pemBytes(pemBlock *pem.Block) []byte { return pem.EncodeToMemory(pemBlock) }
