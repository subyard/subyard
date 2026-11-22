package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestLiveSSHRejectsStalePasswordPolicy(t *testing.T) {
	for _, test := range []struct {
		name           string
		password, none bool
	}{
		{name: "public key only"},
		{name: "running daemon still offers passwords", password: true},
		{name: "running daemon allows no authentication", none: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize)))
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				config := &ssh.ServerConfig{
					NoClientAuth: test.none,
					PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
						return nil, errors.New("cannot authenticate")
					},
				}
				if test.password {
					config.PasswordCallback = func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
						return nil, errors.New("unexpected password request")
					}
				}
				config.AddHostKey(signer)
				if server, _, _, err := ssh.NewServerConn(conn, config); err == nil {
					server.Close()
				}
			}()
			err = checkLiveSSH(bytes.NewReader(ssh.MarshalAuthorizedKey(signer.PublicKey())), "dev", listener.Addr().String())
			if test.password || test.none {
				if err == nil || !strings.Contains(err.Error(), "public-key authentication only") {
					t.Fatalf("live policy accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			listener.Close()
			<-done
		})
	}
}
