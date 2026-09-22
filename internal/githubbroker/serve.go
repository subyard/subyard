package githubbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// RuntimeConfig contains transport configuration only. The App key stays on the owner host.
type RuntimeConfig struct {
	AppConfig      string `json:"app_config"`
	KeyFile        string `json:"key_file"`
	SSHPort        int    `json:"ssh_port"`
	Developer      string `json:"developer"`
	IdentityFile   string `json:"identity_file"`
	KnownHostsFile string `json:"known_hosts_file"`
}

func GuestSocket(developer string) string {
	return "/home/" + developer + "/.local/share/subyard/github.sock"
}

// ProfileEnabled supplies only this profile's defaults, preserving legacy selection for all others.
func ProfileEnabled(yard string, environment map[string]string) bool {
	if environment["NESTED_E2E_VMS"] == "1" {
		return false
	}
	if profiles, explicit := environment["ENVIRONMENT_PROFILES"]; explicit {
		for _, profile := range strings.Fields(profiles) {
			if profile == "github" {
				return true
			}
		}
		return false
	}
	return yard == "default"
}

func RunServer(ctx context.Context, filename string) error {
	data, err := readProtected(filename, maxConfigBytes)
	if err != nil {
		return errors.New("cannot read GitHub broker transport configuration")
	}
	var cfg RuntimeConfig
	if len(data) > 16384 || json.Unmarshal(data, &cfg) != nil || cfg.SSHPort < 1 || cfg.SSHPort > 65535 ||
		cfg.Developer == "" || strings.ContainsAny(cfg.Developer, "/'\" \t\r\n") || !strings.HasPrefix(cfg.AppConfig, "/") {
		return errors.New("invalid GitHub broker transport configuration")
	}
	return serveSSH(ctx, cfg)
}

func serveSSH(ctx context.Context, cfg RuntimeConfig) error {
	data, err := readProtected(cfg.IdentityFile, maxKeyBytes)
	if err != nil {
		return errors.New("cannot read yard transport identity")
	}
	signer, err := ssh.ParsePrivateKey(data)
	clear(data)
	if err != nil {
		return errors.New("invalid yard transport identity")
	}
	hostKey, err := knownhosts.New(cfg.KnownHostsFile)
	if err != nil {
		return errors.New("cannot read pinned yard host key")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.SSHPort))
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return errors.New("yard SSH transport unavailable")
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: cfg.Developer,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKey, HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}})
	if err != nil {
		return errors.New("yard SSH authentication failed")
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer client.Close()
	socket := GuestSocket(cfg.Developer)
	session, err := client.NewSession()
	if err != nil {
		return errors.New("yard broker socket unavailable")
	}
	// Only the managed socket can be replaced; never unlink a file or symlink.
	err = session.Run("if [ -L '" + socket + "' ]; then exit 1; elif [ -S '" + socket + "' ]; then rm -- '" + socket + "'; elif [ -e '" + socket + "' ]; then exit 1; fi")
	session.Close()
	if err != nil {
		return errors.New("unsafe or unavailable yard broker socket")
	}
	// Register channels before the global request: ListenUnix registers its queue
	// after the reply and can miss transport shutdown racing that reply.
	incoming := client.HandleChannelOpen("forwarded-streamlocal@openssh.com")
	ready := make(chan error, 1)
	go func() {
		ok, _, e := client.SendRequest("streamlocal-forward@openssh.com", true, ssh.Marshal(struct{ Socket string }{socket}))
		if e == nil && !ok {
			e = errors.New("forward denied")
		}
		ready <- e
	}()
	select {
	case err = <-ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("yard broker socket setup timed out")
	}
	if err != nil {
		return errors.New("cannot open yard broker socket")
	}
	listener := transportListener{client: client, incoming: incoming, socket: socket}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				probe, e := client.NewSession()
				if e == nil {
					e = probe.Run("true")
					probe.Close()
				}
				if e != nil {
					client.Close()
					return
				}
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			}
		}
	}()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg, err := LoadConfig(cfg.AppConfig, cfg.KeyFile)
		var issuer *Issuer
		if err == nil {
			issuer, err = NewIssuer(cfg)
		}
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if r.Method == http.MethodGet && r.URL.Path == "/status" && r.URL.RawQuery == "" {
				_, _ = io.WriteString(w, "{\"configured\":false}\n")
				return
			}
			http.Error(w, "GitHub App is not configured on the owner host", http.StatusServiceUnavailable)
			return
		}
		NewHandler(issuer).ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 4096}
	defer server.Close()
	err = server.Serve(listener)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return errors.New("yard GitHub broker transport stopped")
	}
	return nil
}

// Closing the transport closes incoming channels without sending a new global
// cancel-forward request to an already disconnected SSH peer.
type transportListener struct {
	client   *ssh.Client
	incoming <-chan ssh.NewChannel
	socket   string
}

func (listener transportListener) Close() error { return listener.client.Close() }
func (listener transportListener) Addr() net.Addr {
	return &net.UnixAddr{Name: listener.socket, Net: "unix"}
}

func (listener transportListener) Accept() (net.Conn, error) {
	for channel := range listener.incoming {
		var payload struct{ Socket, Reserved string }
		if ssh.Unmarshal(channel.ExtraData(), &payload) != nil || payload.Socket != listener.socket {
			_ = channel.Reject(ssh.Prohibited, "unknown socket")
			continue
		}
		remote, requests, err := channel.Accept()
		if err != nil {
			return nil, err
		}
		go ssh.DiscardRequests(requests)
		// SSH channels do not implement deadlines. The pipe lets net/http enforce
		// timeouts and interrupt background reads when a response ends.
		local, server := net.Pipe()
		go func() { defer local.Close(); defer remote.Close(); _, _ = io.Copy(local, remote) }()
		go func() { defer local.Close(); defer remote.Close(); _, _ = io.Copy(remote, local) }()
		return server, nil
	}
	return nil, io.EOF
}
