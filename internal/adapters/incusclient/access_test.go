package incusclient

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSocketPermissionIsTypedAccessDenial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses socket permissions")
	}
	socket := filepath.Join(testkit.TempDir(t), "unix.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatal(err)
	}
	client := New(socket, "projects")
	_, err = client.Instance(context.Background(), "subyard", "yard")
	if !errors.Is(err, domain.ErrIncusAccessDenied) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("instance read = %v, want typed access denial", err)
	}
	if errors.Is(err, ports.ErrIncusUnavailable) || errors.Is(err, ports.ErrInstanceNotFound) {
		t.Fatalf("access denial must not look unavailable or absent: %v", err)
	}
	if client.LocalInstallationAbsent() {
		t.Fatal("a custom socket never proves an absent installation")
	}
}

func TestAbsentClientNeverDials(t *testing.T) {
	t.Setenv("INCUS_SOCKET", filepath.Join(testkit.TempDir(t), "missing.socket"))
	client := NewAbsent()
	if !client.LocalInstallationAbsent() || !ports.LocalInstallationAbsent(client) {
		t.Fatal("an absent client proves an absent local installation")
	}
	if _, err := client.Instance(context.Background(), "subyard", "yard"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent instance read = %v, want not-exist", err)
	}
	if _, err := client.Server(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent server read = %v, want not-exist", err)
	}
}

// Only the socket dial is access denial; other permission errors keep their meaning.
func TestOnlySocketDialPermissionIsAccessDenial(t *testing.T) {
	if err := normalizeError("read state", os.ErrPermission); errors.Is(err, domain.ErrIncusAccessDenied) {
		t.Fatalf("non-dial permission error was typed as socket access: %v", err)
	}
	dial := &net.OpError{Op: "dial", Net: "unix", Err: os.ErrPermission}
	if err := normalizeError("get server", dial); !errors.Is(err, domain.ErrIncusAccessDenied) {
		t.Fatalf("socket dial permission error was not typed: %v", err)
	}
}
