package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func init() {
	StrictErrorCodes = true
}

func TestGeneratedVocabulary(t *testing.T) {
	root := filepath.Join("..", "..")
	command := exec.Command("python3", filepath.Join(root, "dev/generate-rpc-vocabulary.py"), "--check")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(root, "api/yard-rpc/v1/vocabulary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Revision, RPCVersion int
		Capabilities         map[string]struct {
			Since    int
			Sessions []string
		}
		Methods    map[string]struct{ Capability string }
		ErrorCodes map[ErrorCode]struct{ Since int }
		Events     map[string]struct{ Since int }
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Revision != ContractRevision || manifest.RPCVersion != ProtocolVersion {
		t.Fatal("version drift")
	}
	for name, method := range manifest.Methods {
		if _, ok := manifest.Capabilities[method.Capability]; !ok {
			t.Errorf("%s: undeclared capability %s", name, method.Capability)
		}
	}
	for code, entry := range manifest.ErrorCodes {
		if !code.Valid() || entry.Since > manifest.Revision {
			t.Errorf("invalid error %s", code)
		}
	}
	for event, entry := range manifest.Events {
		if entry.Since > manifest.Revision {
			t.Errorf("invalid event %s", event)
		}
	}
	for _, session := range []string{"workspace", "owner-services"} {
		var want []string
		for name, capability := range manifest.Capabilities {
			if capability.Since > manifest.Revision {
				t.Errorf("invalid capability %s", name)
			}
			if slices.Contains(capability.Sessions, session) {
				want = append(want, name)
			}
		}
		// Exercise the actual negotiation path rather than only its generated helper.
		var input, output bytes.Buffer
		codec := NewCodec(nil, &input)
		writeRequest(t, codec, Request{Version: ProtocolVersion, Type: "request", ID: "hello", Method: MethodRpcNegotiate})
		err := (Session{Handler: HandlerFunc(func(context.Context, Call, Emit) (any, error) { t.Fatal("negotiation dispatched"); return nil, nil }), Capabilities: CapabilitiesForSession(session), DrainOnEOF: true}).Serve(context.Background(), &input, &output)
		if err != nil {
			t.Fatal(err)
		}
		response := readResponse(t, NewCodec(&output, nil))
		encoded, _ := json.Marshal(response.Result)
		var negotiated struct{ Capabilities []string }
		if err := json.Unmarshal(encoded, &negotiated); err != nil {
			t.Fatal(err)
		}
		slices.Sort(want)
		slices.Sort(negotiated.Capabilities)
		if !reflect.DeepEqual(want, negotiated.Capabilities) {
			t.Fatalf("%s: got %v want %v", session, negotiated.Capabilities, want)
		}
	}
}

func TestSessionCapabilityGateAndErrorVocabulary(t *testing.T) {
	for _, capabilities := range [][]string{nil, {CapabilityContext}} {
		var input, output bytes.Buffer
		codec := NewCodec(nil, &input)
		for _, method := range []string{MethodRpcNegotiate, MethodContextGet} {
			writeRequest(t, codec, Request{Version: ProtocolVersion, Type: "request", ID: method, Method: method})
		}
		calls := 0
		err := (Session{Capabilities: capabilities, DrainOnEOF: true, Handler: HandlerFunc(func(context.Context, Call, Emit) (any, error) { calls++; return "ok", nil })}).Serve(context.Background(), &input, &output)
		if err != nil {
			t.Fatal(err)
		}
		reader := NewCodec(&output, nil)
		_ = readResponse(t, reader)
		response := readResponse(t, reader)
		if len(capabilities) == 0 {
			if calls != 0 || response.Error == nil || response.Error.Code != CodeCapabilityMissing {
				t.Fatalf("missing gate: calls=%d response=%#v", calls, response)
			}
		} else if calls != 1 || response.Error != nil {
			t.Fatalf("negotiated call refused: %#v", response)
		}
	}
	for method := range MethodCapabilities() {
		if method == MethodRpcNegotiate {
			continue
		}
		t.Run(method, func(t *testing.T) {
			var input, output bytes.Buffer
			codec := NewCodec(nil, &input)
			for _, name := range []string{MethodRpcNegotiate, method} {
				writeRequest(t, codec, Request{Version: ProtocolVersion, Type: "request", ID: name, Method: name})
			}
			err := (Session{DrainOnEOF: true, Handler: HandlerFunc(func(context.Context, Call, Emit) (any, error) {
				t.Error("method dispatched without capability")
				return nil, nil
			})}).Serve(context.Background(), &input, &output)
			if err != nil {
				t.Fatal(err)
			}
			reader := NewCodec(&output, nil)
			_ = readResponse(t, reader)
			response := readResponse(t, reader)
			if response.Error == nil || response.Error.Code != CodeCapabilityMissing {
				t.Fatalf("%s: %#v", method, response)
			}
		})
	}

}

func TestSessionRejectsUndeclaredMethodsBeforeDispatch(t *testing.T) {
	for _, name := range []string{"workspace", "owner-services"} {
		t.Run(name, func(t *testing.T) {
			var input, output bytes.Buffer
			codec := NewCodec(nil, &input)
			for _, method := range []string{MethodRpcNegotiate, "debug.undeclared"} {
				writeRequest(t, codec, Request{Version: ProtocolVersion, Type: "request", ID: method, Method: method})
			}
			err := (Session{Capabilities: CapabilitiesForSession(name), DrainOnEOF: true,
				Handler: HandlerFunc(func(context.Context, Call, Emit) (any, error) {
					t.Error("undeclared method reached dispatch")
					return "executed", nil
				})}).Serve(context.Background(), &input, &output)
			if err != nil {
				t.Fatal(err)
			}
			reader := NewCodec(&output, nil)
			_ = readResponse(t, reader)
			response := readResponse(t, reader)
			if response.Error == nil || response.Error.Code != CodeMethodNotFound || response.Error.Message != "debug.undeclared" {
				t.Fatalf("undeclared method: %#v", response)
			}
		})
	}
}

func TestRelayedUnknownErrorKeepsSessionAndConcurrentOperationAlive(t *testing.T) {
	remoteFault := &Error{Code: "future_owner_code", Message: "original owner message\nwith detail"}
	var wire bytes.Buffer
	if err := NewCodec(nil, &wire).Write(Response{Version: ProtocolVersion, Type: "response", ID: "owner", Error: remoteFault}); err != nil {
		t.Fatal(err)
	}
	decoded, err := NewCodec(&wire, nil).ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	remoteFault = decoded.Error
	started, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := HandlerFunc(func(ctx context.Context, call Call, _ Emit) (any, error) {
		switch call.Method {
		case MethodOperationExecute:
			close(started)
			select {
			case <-release:
				return "completed", nil
			case <-ctx.Done():
				close(cancelled)
				return nil, ctx.Err()
			}
		case MethodContextGet:
			return nil, remoteFault // Like ownerRPCSession.call, relay the remote error unchanged.
		default:
			return "alive", nil
		}
	})
	client, server := net.Pipe()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- (Session{Handler: handler, Capabilities: CapabilitiesForSession("workspace")}).Serve(context.Background(), server, server)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("session did not stop")
		}
		if remoteFault.Code != "future_owner_code" {
			t.Error("relay mutated the remote error")
		}
	})
	codec := NewCodec(client, client)
	send := func(id, method string) {
		writeRequest(t, codec, Request{Version: ProtocolVersion, Type: "request", ID: id, Method: method})
	}
	send("negotiate", MethodRpcNegotiate)
	_ = readResponse(t, codec)
	send("running", MethodOperationExecute)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent operation did not start")
	}
	send("relay", MethodContextGet)
	response := readResponse(t, codec)
	if response.ID != "relay" || response.Error == nil || response.Error.Code != CodeInternal || response.Error.Message != remoteFault.Message {
		t.Fatalf("relayed fault: %#v", response)
	}
	send("after", MethodSystemPing)
	if response := readResponse(t, codec); response.ID != "after" || response.Error != nil || response.Result != "alive" {
		t.Fatalf("session did not survive: %#v", response)
	}
	select {
	case <-cancelled:
		t.Fatal("unrelated operation was cancelled")
	default:
	}
	close(release)
	if response := readResponse(t, codec); response.ID != "running" || response.Error != nil || response.Result != "completed" {
		t.Fatalf("concurrent operation did not survive: %#v", response)
	}
}

// This handler returns an ordinary local error. The assertion deliberately does
// not inspect its code, so the strict session guard owns vocabulary validation.
func TestSessionLocalFault(t *testing.T) {
	var input, output bytes.Buffer
	codec := NewCodec(nil, &input)
	for _, method := range []string{MethodRpcNegotiate, MethodContextGet} {
		writeRequest(t, codec, Request{Version: ProtocolVersion, Type: "request", ID: method, Method: method})
	}
	err := (Session{Capabilities: CapabilitiesForSession("workspace"), DrainOnEOF: true,
		Handler: HandlerFunc(func(context.Context, Call, Emit) (any, error) {
			return nil, errors.New("local failure")
		})}).Serve(context.Background(), &input, &output)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewCodec(&output, nil)
	_ = readResponse(t, reader)
	if response := readResponse(t, reader); response.Error == nil || response.Error.Message != "local failure" {
		t.Fatalf("local fault: %#v", response)
	}
}
