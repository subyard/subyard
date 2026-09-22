package sshagentruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/agent"
)

func TestFilteredAgentSignsButRejectsMutationsAndExtensions(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
	upstream, upstreamServer := net.Pipe()
	defer upstreamServer.Close()
	go agent.ServeAgent(ring, upstreamServer)
	guest, proxy := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveFiltered(ctx, proxy, upstream)
	defer guest.Close()
	client := agent.NewClient(guest)
	keys, err := client.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("identities: %v, %d", err, len(keys))
	}
	data := []byte("synthetic signing challenge")
	sig, err := client.Sign(keys[0], data)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys[0].Verify(data, sig); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveAll(); err == nil {
		t.Fatal("guest removed identities")
	}
	if err := client.Add(agent.AddedKey{PrivateKey: key}); err == nil {
		t.Fatal("guest added identity")
	}
	if err := client.Lock([]byte("synthetic")); err == nil {
		t.Fatal("guest locked agent")
	}
	if _, err := client.Extension("session-bind@openssh.com", nil); err == nil {
		t.Fatal("guest accessed extension")
	}
	keys, err = ring.List()
	if err != nil || len(keys) != 1 {
		t.Fatal("restricted requests changed private agent")
	}
	cancel()
	guest.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.List(); err == nil {
		t.Fatal("existing connection survived revoke")
	}
}

func TestFilteredAgentRejectsOversizedFrame(t *testing.T) {
	upstream, server := net.Pipe()
	defer server.Close()
	guest, proxy := net.Pipe()
	defer guest.Close()
	go serveFiltered(context.Background(), proxy, upstream)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 2<<20)
	if _, err := guest.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	guest.SetReadDeadline(time.Now().Add(time.Second))
	var response [1]byte
	if _, err := guest.Read(response[:]); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
