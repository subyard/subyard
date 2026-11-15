//go:build unix

package rpc

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A blocking stdout pipe, like `yard rpc --stdio`, cannot be interrupted by
// Close. A stalled client still has its operations cancelled, and the session
// ends once the peer closes its end of the stream.
func TestStalledBlockingPipeCancelsOperationsAndEndsWhenPeerCloses(t *testing.T) {
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	peerOutput := os.NewFile(uintptr(fds[0]), "rpc-output-read")
	output := os.NewFile(uintptr(fds[1]), "rpc-output-write")
	defer peerOutput.Close()
	cancelled := make(chan struct{})
	large := strings.Repeat("x", 512*1024)
	handler := HandlerFunc(func(ctx context.Context, call Call, _ Emit) (any, error) {
		if call.Method == "hold" {
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}
		return map[string]any{"large": large}, nil
	})
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- (Session{Handler: handler, WriteStallTimeout: 100 * time.Millisecond}).Serve(
			context.Background(), server, output)
	}()
	requests := NewCodec(nil, client)
	responses := NewCodec(peerOutput, nil)
	writeRequest(t, requests, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	_ = readResponse(t, responses)
	writeRequest(t, requests, Request{Version: 1, Type: "request", ID: "hold", Method: "hold"})
	writeRequest(t, requests, Request{Version: 1, Type: "request", ID: "large", Method: "large"})
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled client did not cancel operations")
	}
	_ = peerOutput.Close()
	select {
	case err := <-done:
		if !errors.Is(err, errWriteStall) {
			t.Fatalf("session ended with %v, want write stall", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end after the peer closed the stream")
	}
}
