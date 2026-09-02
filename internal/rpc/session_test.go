package rpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainOnEOFFlushesAcceptedResponses(t *testing.T) {
	var input bytes.Buffer
	requestCodec := NewCodec(strings.NewReader(""), &input)
	writeRequest(t, requestCodec, Request{
		Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate",
	})
	writeRequest(t, requestCodec, Request{
		Version: 1, Type: "request", ID: "ping", Method: "system.ping",
	})
	var output bytes.Buffer
	handler := HandlerFunc(func(_ context.Context, call Call, _ Emit) (any, error) {
		return map[string]any{"method": call.Method}, nil
	})
	if err := (Session{Handler: handler, DrainOnEOF: true}).Serve(
		context.Background(), &input, &output,
	); err != nil {
		t.Fatal(err)
	}
	responseCodec := NewCodec(&output, io.Discard)
	if response := readResponse(t, responseCodec); response.ID != "n" || response.Error != nil {
		t.Fatalf("negotiation response was not flushed: %#v", response)
	}
	if response := readResponse(t, responseCodec); response.ID != "ping" || response.Error != nil {
		t.Fatalf("accepted call response was not flushed: %#v", response)
	}
}

func TestSessionNegotiationEventsAndCancellation(t *testing.T) {
	started := make(chan struct{})
	handler := HandlerFunc(func(ctx context.Context, call Call, emit Emit) (any, error) {
		switch call.Method {
		case "events":
			if _, err := emit("started", map[string]any{"ok": true}); err != nil {
				return nil, err
			}
			if _, err := emit("finished", nil); err != nil {
				return nil, err
			}
			return map[string]any{"done": true}, nil
		case "slow":
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			return nil, &Error{Code: "method_not_found", Message: call.Method}
		}
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (Session{Handler: handler, Capabilities: []string{"events"}}).Serve(context.Background(), server, server)
	}()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	if response := readResponse(t, codec); response.Error != nil {
		t.Fatal(response.Error)
	}
	writeRequest(t, codec, Request{
		Version: 1, Type: "request", ID: "e", OperationID: "operation-events", Method: "events",
	})
	first := readResponse(t, codec)
	second := readResponse(t, codec)
	third := readResponse(t, codec)
	if first.Type != "event" || second.Type != "event" || third.Type != "response" ||
		first.Sequence != first.Revision || second.Sequence != second.Revision ||
		first.Sequence+1 != second.Sequence || first.OperationID != "operation-events" ||
		second.OperationID != "operation-events" || third.OperationID != "operation-events" {
		t.Fatalf("unexpected event flow: %#v %#v %#v", first, second, third)
	}
	writeRequest(t, codec, Request{
		Version: 1, Type: "request", ID: "slow", OperationID: "operation-slow", Method: "slow",
	})
	<-started
	writeRequest(t, codec, Request{Version: 1, Type: "cancel", ID: "cancel", OperationID: "operation-slow"})
	responses := []Response{readResponse(t, codec), readResponse(t, codec)}
	var cancelled bool
	for _, response := range responses {
		if response.ID == "slow" && response.OperationID == "operation-slow" &&
			response.Error != nil && response.Error.Code == "cancelled" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("slow operation was not cancelled: %#v", responses)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCompletedOperationIDCanBeReusedAfterItsResponse(t *testing.T) {
	handler := HandlerFunc(func(context.Context, Call, Emit) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- (Session{Handler: handler}).Serve(context.Background(), server, server) }()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	_ = readResponse(t, codec)
	for index := range 256 {
		requestID := fmt.Sprintf("phase-%d", index)
		writeRequest(t, codec, Request{
			Version: 1, Type: "request", ID: requestID, OperationID: "operation-phases", Method: "phase",
		})
		response := readResponse(t, codec)
		if response.ID != requestID || response.Error != nil {
			t.Fatalf("completed operation ID remained active after response %d: %#v", index, response)
		}
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionRejectsUnnegotiatedVersionAndSecrets(t *testing.T) {
	handlerCalls := atomic.Int32{}
	handler := HandlerFunc(func(context.Context, Call, Emit) (any, error) {
		handlerCalls.Add(1)
		return nil, nil
	})
	client, server := net.Pipe()
	go func() { _ = (Session{Handler: handler}).Serve(context.Background(), server, server) }()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 2, Type: "request", ID: "version", Method: "rpc.negotiate"})
	if response := readResponse(t, codec); response.Error == nil || response.Error.Code != "incompatible_version" {
		t.Fatalf("version accepted: %#v", response)
	}
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "early", Method: "read"})
	if response := readResponse(t, codec); response.Error == nil || response.Error.Code != "negotiation_required" {
		t.Fatalf("unnegotiated call accepted: %#v", response)
	}
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	_ = readResponse(t, codec)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "secret", Method: "read", Params: json.RawMessage(`{"apiToken":"x"}`)})
	if response := readResponse(t, codec); response.Error == nil || response.Error.Code != "secret_forbidden" {
		t.Fatalf("secret-bearing call accepted: %#v", response)
	}
	if handlerCalls.Load() != 0 {
		t.Fatal("rejected calls reached handler")
	}
	_ = client.Close()
}

func TestSessionAppliesRequestDeadline(t *testing.T) {
	handler := HandlerFunc(func(ctx context.Context, _ Call, _ Emit) (any, error) {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- (Session{Handler: handler}).Serve(context.Background(), server, server) }()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	_ = readResponse(t, codec)
	expired := time.Unix(1, 0).UTC()
	writeRequest(t, codec, Request{
		Version: 1, Type: "request", ID: "expired", Method: "slow", Deadline: &expired,
	})
	response := readResponse(t, codec)
	if response.Error == nil || response.Error.Code != "deadline_exceeded" {
		t.Fatalf("expired deadline was not enforced: %#v", response)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCodecHandlesPartialIOAndOversize(t *testing.T) {
	writer := &oneByteWriter{}
	codec := NewCodec(strings.NewReader(""), writer)
	if err := codec.Write(Response{Version: 1, Type: "response", ID: "one"}); err != nil {
		t.Fatal(err)
	}
	reader := NewCodec(&oneByteReader{value: writer.value}, io.Discard)
	response, err := reader.ReadResponse()
	if err != nil || response.ID != "one" {
		t.Fatalf("partial frame failed: %#v, %v", response, err)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, MaxFrameSize+1)
	if _, err := NewCodec(strings.NewReader(string(header)), io.Discard).ReadRequest(); err == nil {
		t.Fatal("oversized frame was accepted")
	}
}

func TestDisconnectCancelsOperation(t *testing.T) {
	cancelled := make(chan struct{})
	handler := HandlerFunc(func(ctx context.Context, _ Call, _ Emit) (any, error) {
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- (Session{Handler: handler}).Serve(context.Background(), server, server) }()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	_ = readResponse(t, codec)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "work", Method: "slow"})
	_ = client.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel operation")
	}
	if err := <-done; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestBackpressureDisconnectsSlowClient(t *testing.T) {
	handler := HandlerFunc(func(_ context.Context, _ Call, emit Emit) (any, error) {
		for index := 0; index < 100; index++ {
			if _, err := emit("progress", map[string]any{"index": index}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- (Session{Handler: handler, Buffer: 1}).Serve(context.Background(), server, server) }()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	_ = readResponse(t, codec)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "flood", Method: "events"})
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = client.Close()
		t.Fatal("slow RPC client blocked session shutdown")
	}
	_ = client.Close()
}

func TestContextCancellationInterruptsBlockedRead(t *testing.T) {
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (Session{Handler: HandlerFunc(func(context.Context, Call, Emit) (any, error) {
			return nil, nil
		})}).Serve(ctx, server, server)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = client.Close()
		t.Fatal("context cancellation did not interrupt RPC read")
	}
	_ = client.Close()
}

func TestSessionsGiveSecondClientIndependentOrderedStream(t *testing.T) {
	handler := HandlerFunc(func(_ context.Context, call Call, emit Emit) (any, error) {
		revision, err := emit("snapshot.ready", map[string]any{"client": call.ID})
		if err != nil {
			return nil, err
		}
		return map[string]any{"revision": revision}, nil
	})
	for _, clientID := range []string{"first", "second"} {
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- (Session{Handler: handler}).Serve(context.Background(), server, server) }()
		codec := NewCodec(client, client)
		writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
		_ = readResponse(t, codec)
		writeRequest(t, codec, Request{Version: 1, Type: "request", ID: clientID, Method: "system.snapshot"})
		event := readResponse(t, codec)
		response := readResponse(t, codec)
		if event.Type != "event" || event.Sequence != 1 || event.Revision != 1 ||
			response.Type != "response" || response.ID != clientID {
			t.Fatalf("client %s inherited session state: %#v %#v", clientID, event, response)
		}
		_ = client.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrderedEventSequencerSerializesRevisionAndEnqueue(t *testing.T) {
	const eventCount = 64
	var sequencer orderedEventSequencer
	var frames []Response
	start := make(chan struct{})
	errs := make(chan error, eventCount)
	var workers sync.WaitGroup
	for range eventCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := sequencer.publish(Response{Type: "event"}, func(response Response) error {
				frames = append(frames, response)
				return nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(frames) != eventCount {
		t.Fatalf("published %d events, want %d", len(frames), eventCount)
	}
	for index, frame := range frames {
		want := uint64(index + 1)
		if frame.Sequence != want || frame.Revision != want {
			t.Fatalf("event %d has out-of-order revision: %#v", index, frame)
		}
	}
}

func TestSessionConcurrentEventsFollowRevisionOrder(t *testing.T) {
	const operationCount = 64
	ready := atomic.Int32{}
	release := make(chan struct{})
	handler := HandlerFunc(func(_ context.Context, _ Call, emit Emit) (any, error) {
		if ready.Add(1) == operationCount {
			close(release)
		}
		<-release
		_, err := emit("progress", map[string]any{"ok": true})
		return map[string]any{"done": true}, err
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (Session{
			Handler:      handler,
			Capabilities: []string{"ordered-events"},
			Buffer:       operationCount * 2,
		}).Serve(context.Background(), server, server)
	}()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	if response := readResponse(t, codec); response.Error != nil {
		t.Fatal(response.Error)
	}
	for index := range operationCount {
		operationID := fmt.Sprintf("operation-%d", index)
		writeRequest(t, codec, Request{
			Version: 1, Type: "request", ID: operationID, OperationID: operationID, Method: "events",
		})
	}
	events := 0
	responses := 0
	for range operationCount * 2 {
		response := readResponse(t, codec)
		switch response.Type {
		case "event":
			want := uint64(events + 1)
			if response.Sequence != want || response.Revision != want {
				t.Fatalf("wire event %d has out-of-order revision: %#v", events, response)
			}
			events++
		case "response":
			if response.Error != nil {
				t.Fatalf("operation failed: %#v", response)
			}
			responses++
		default:
			t.Fatalf("unexpected frame: %#v", response)
		}
	}
	if events != operationCount || responses != operationCount {
		t.Fatalf("received %d events and %d responses, want %d of each", events, responses, operationCount)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAndFollowingEventsShareMonotonicSessionRevision(t *testing.T) {
	handler := HandlerFunc(func(_ context.Context, call Call, emit Emit) (any, error) {
		switch call.Method {
		case "snapshot":
			revision, err := emit("snapshot.ready", map[string]any{"complete": true})
			return map[string]any{"revision": revision}, err
		case "events":
			_, err := emit("incus.lifecycle", map[string]any{"action": "started"})
			return map[string]any{"closed": true}, err
		default:
			return nil, &Error{Code: "method_not_found", Message: call.Method}
		}
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- (Session{Handler: handler}).Serve(context.Background(), server, server) }()
	codec := NewCodec(client, client)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"})
	_ = readResponse(t, codec)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "s", Method: "snapshot"})
	snapshotEvent := readResponse(t, codec)
	_ = readResponse(t, codec)
	writeRequest(t, codec, Request{Version: 1, Type: "request", ID: "e", Method: "events"})
	lifecycleEvent := readResponse(t, codec)
	_ = readResponse(t, codec)
	if snapshotEvent.Sequence != 1 || snapshotEvent.Revision != 1 || lifecycleEvent.Sequence != 2 ||
		lifecycleEvent.Revision != 2 {
		t.Fatalf("snapshot/event revision regressed: snapshot=%#v event=%#v", snapshotEvent, lifecycleEvent)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func writeRequest(t *testing.T, codec *Codec, request Request) {
	t.Helper()
	if err := codec.Write(request); err != nil {
		t.Fatal(err)
	}
}

func readResponse(t *testing.T, codec *Codec) Response {
	t.Helper()
	response, err := codec.ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type oneByteWriter struct{ value []byte }

func (writer *oneByteWriter) Write(value []byte) (int, error) {
	writer.value = append(writer.value, value[0])
	return 1, nil
}

type oneByteReader struct{ value []byte }

func (reader *oneByteReader) Read(target []byte) (int, error) {
	if len(reader.value) == 0 {
		return 0, io.EOF
	}
	target[0] = reader.value[0]
	reader.value = reader.value[1:]
	return 1, nil
}
