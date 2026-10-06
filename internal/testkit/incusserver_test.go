package testkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestIncusExecOutputWaitsForStdin(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		for _, cancel := range []bool{false, true} {
			name := stream + "/input"
			if cancel {
				name = stream + "/cancel"
			}
			t.Run(name, func(t *testing.T) {
				operation := &incusOperation{
					id: "exec", cancelled: make(chan struct{}), stdinDone: make(chan struct{}),
					step: IncusServerExecStep{Stdout: []byte("output"), Stderr: []byte("output")},
				}
				fake := &IncusServer{
					operations: map[string]*incusOperation{operation.id: operation},
					execCalls:  []IncusServerExecCall{{}},
				}
				server := httptest.NewServer(fake)
				defer server.Close()
				defer operation.cancel.Do(func() { close(operation.cancelled) })
				url := "ws" + strings.TrimPrefix(server.URL, "http") + "/1.0/operations/exec/websocket?secret=exec-"
				output, _, err := websocket.DefaultDialer.Dial(url+stream, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer output.Close()
				if err := output.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				kind, payload, err := output.ReadMessage()
				if err != nil || kind != websocket.BinaryMessage || string(payload) != "output" {
					t.Fatalf("output payload: type=%d payload=%q err=%v", kind, payload, err)
				}

				if cancel {
					request, err := http.NewRequest(http.MethodDelete, server.URL+"/1.0/operations/exec", nil)
					if err != nil {
						t.Fatal(err)
					}
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					_ = response.Body.Close()
					if response.StatusCode != http.StatusOK {
						t.Fatalf("cancel status: %d", response.StatusCode)
					}
					kind, _, err := output.ReadMessage()
					if !websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
						t.Fatalf("output completed before stdin: type=%d err=%v", kind, err)
					}
					return
				}

				input, _, err := websocket.DefaultDialer.Dial(url+"stdin", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer input.Close()
				if err := input.WriteMessage(websocket.BinaryMessage, []byte("input")); err != nil {
					t.Fatal(err)
				}
				if err := input.WriteMessage(websocket.TextMessage, nil); err != nil {
					t.Fatal(err)
				}
				kind, payload, err = output.ReadMessage()
				if err != nil || kind != websocket.TextMessage || len(payload) != 0 {
					t.Fatalf("output completion: type=%d payload=%q err=%v", kind, payload, err)
				}
				ctx, stopWaiting := context.WithTimeout(context.Background(), time.Second)
				defer stopWaiting()
				if err := fake.WaitForExecInput(ctx, 0); err != nil {
					t.Fatal(err)
				}
				if got := string(fake.ExecCalls()[0].Stdin); got != "input" {
					t.Fatalf("stdin: %q", got)
				}
			})
		}
	}
}
