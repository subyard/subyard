package projectruntime

import (
	"net"
	"strings"
	"testing"
)

func TestPreviewPortPreflightRejectsBusyListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().String()
	err = previewPortAvailable(address)
	if err == nil || !strings.Contains(err.Error(), address) {
		t.Fatalf("busy port accepted: err=%v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := previewPortAvailable(address); err != nil {
		t.Fatalf("free port rejected: err=%v", err)
	}
}
