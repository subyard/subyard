package sshrelay

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestReadyRequiresSafeExactActiveRelay(t *testing.T) {
	root := t.TempDir()
	unitDir := filepath.Join(root, "systemd")
	bin := filepath.Join(root, "systemctl")
	if err := os.Mkdir(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit \"${SYSTEMCTL_STATUS:-0}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	socket := filepath.Join(unitDir, "subyard-ssh-relay-2222.socket")
	service := filepath.Join(unitDir, "subyard-ssh-relay-2222.service")
	write := func(path, contents string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(socket, "[Socket]\nListenStream=127.0.0.1:2222\nAccept=no\n", 0o644)
	write(service, "[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 10.0.0.2:22\n", 0o644)
	if !Ready(context.Background(), unitDir, uid, 2222, "10.0.0.2", bin) {
		t.Fatal("exact active relay was rejected")
	}

	for name, mutate := range map[string]func(){
		"unsafe mode": func() { _ = os.Chmod(service, 0o666) },
		"wrong target": func() {
			write(service, "[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 10.0.0.3:22\n", 0o644)
		},
		"unsafe executable": func() { write(service, "[Service]\nExecStart=/tmp/proxyd 10.0.0.2:22\n", 0o644) },
		"duplicate listen": func() {
			write(socket, "[Socket]\nListenStream=127.0.0.1:2222\nListenStream=0.0.0.0:2222\nAccept=no\n", 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			write(socket, "[Socket]\nListenStream=127.0.0.1:2222\nAccept=no\n", 0o644)
			write(service, "[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 10.0.0.2:22\n", 0o644)
			mutate()
			if Ready(context.Background(), unitDir, uid, 2222, "10.0.0.2", bin) {
				t.Fatal("unsafe relay was accepted")
			}
		})
	}
	t.Setenv("SYSTEMCTL_STATUS", strconv.Itoa(1))
	write(socket, "[Socket]\nListenStream=127.0.0.1:2222\nAccept=no\n", 0o644)
	write(service, "[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 10.0.0.2:22\n", 0o644)
	if Ready(context.Background(), unitDir, uid, 2222, "10.0.0.2", bin) {
		t.Fatal("inactive relay was accepted")
	}
}
