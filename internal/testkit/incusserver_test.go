package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestIncusOperationExactResourceAndFinalReadback(t *testing.T) {
	fake, err := NewIncusServer(TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	fake.QueueExec(IncusServerExecStep{})
	post := httptest.NewRecorder()
	fake.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/1.0/instances/yard/exec?project=scope", bytes.NewBufferString(`{"command":["true"]}`)))
	if post.Code != http.StatusAccepted {
		t.Fatal("exec acceptance", post.Code)
	}
	get := func(path string, want int) map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		fake.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != want {
			t.Fatal("operation response", response.Code)
		}
		var body struct {
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Metadata
	}
	running := get("/1.0/operations/operation-1", http.StatusOK)
	if running["status_code"] != float64(103) {
		t.Fatal("uncompleted operation was final")
	}
	resources := running["resources"].(map[string]any)["instances"].([]any)
	if len(resources) != 1 || resources[0] != "/1.0/instances/yard?project=scope" {
		t.Fatal("operation lost original resource")
	}
	close(fake.operations["operation-1"].stdinDone)
	get("/1.0/operations/operation-1/wait", http.StatusOK)
	final := get("/1.0/operations/operation-1", http.StatusOK)
	if final["id"] != "operation-1" || final["status_code"] != float64(200) {
		t.Fatal("exact terminal was not retained")
	}
	get("/1.0/operations/missing", http.StatusNotFound)
}

func TestIncusInventoryReplacementReturnsAuthoritativeAbsence(t *testing.T) {
	fake := &IncusServer{instances: map[string]map[string]any{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	get := func(name string, status int) map[string]any {
		t.Helper()
		response, err := server.Client().Get(server.URL + "/1.0/instances/" + name + "?project=subyard")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("instance %s: status %d, want %d", name, response.StatusCode, status)
		}
		if status == http.StatusNotFound && (body["type"] != "error" || body["error_code"] != float64(http.StatusNotFound)) {
			t.Fatalf("absence is not an authoritative Incus error: %#v", body)
		}
		return body
	}
	fake.SetInstance("subyard", "old", map[string]any{"name": "old", "status": "Running"})
	get("old", http.StatusOK)
	replacement := map[string]map[string]any{"subyard/new": {"name": "new", "status": "Running"}}
	fake.ReplaceInstances(replacement)
	replacement["subyard/new"]["status"] = "Stopped"
	get("old", http.StatusNotFound)
	if body := get("new", http.StatusOK); body["metadata"].(map[string]any)["status"] != "Running" {
		t.Fatal("replacement retained caller-owned inventory data")
	}
	fake.ReplaceInstances(nil)
	get("new", http.StatusNotFound)
}

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

func TestIncusInventorySnapshotRemovesStaleResourcesAndPreservesExecution(t *testing.T) {
	fake := &IncusServer{execSteps: []IncusServerExecStep{{Stdout: []byte("queued")}}, execCalls: []IncusServerExecCall{{Name: "prior-call"}}}
	fake.SetInventory(map[string]map[string]any{"prior-project": {"name": "prior-project"}},
		map[string]map[string]any{"prior-project/prior-yard": {"name": "prior-yard", "status": "Stopped"}})
	check := func(path string, expected int) {
		t.Helper()
		response := httptest.NewRecorder()
		fake.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != expected {
			t.Fatalf("%s: status=%d, want %d", path, response.Code, expected)
		}
	}
	check("/1.0/projects/prior-project", http.StatusOK)
	check("/1.0/instances/prior-yard?project=prior-project", http.StatusOK)
	fake.SetInventory(nil, nil)
	check("/1.0/projects/prior-project", http.StatusNotFound)
	check("/1.0/instances/prior-yard?project=prior-project", http.StatusNotFound)
	if len(fake.execSteps) != 1 || string(fake.execSteps[0].Stdout) != "queued" || len(fake.execCalls) != 1 || fake.execCalls[0].Name != "prior-call" {
		t.Fatal("replacing inventory changed queued execution or history")
	}
}

func TestIncusInstanceInventoryRespectsProjectScope(t *testing.T) {
	fake := &IncusServer{}
	fake.SetInventory(map[string]map[string]any{"left": {"name": "left"}, "right": {"name": "right"}, "default": {"name": "default"}, "empty": {"name": "empty"}}, map[string]map[string]any{"left/yard": {"name": "yard"}, "right/yard": {"name": "yard"}, "default/plain": {"name": "plain"}})
	for _, check := range []struct {
		query    string
		projects []string
	}{
		{"?recursion=1&project=left", []string{"left"}},
		{"?recursion=1&project=right", []string{"right"}},
		{"?recursion=1&project=empty", nil},
		{"?recursion=1", []string{"default"}},
		{"?recursion=1&all-projects=true", []string{"default", "left", "right"}},
	} {
		response := httptest.NewRecorder()
		fake.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/1.0/instances"+check.query, nil))
		var body struct{ Metadata []struct{ Project string } }
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
			t.Fatalf("list %s: %d %v", check.query, response.Code, err)
		}
		got := []string{}
		for _, instance := range body.Metadata {
			got = append(got, instance.Project)
		}
		slices.Sort(got)
		if !slices.Equal(got, check.projects) {
			t.Fatalf("%s: projects %v, want %v", check.query, got, check.projects)
		}
	}
	for _, check := range []struct {
		path   string
		status int
	}{
		{"/1.0/instances/yard/snapshots?project=left", http.StatusOK},
		{"/1.0/instances?recursion=1&project=absent", http.StatusNotFound},
		{"/1.0/instances/plain/snapshots", http.StatusOK},
		{"/1.0/instances/plain/snapshots?project=left", http.StatusNotFound},
		{"/1.0/instances/yard/snapshots?project=absent", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		fake.ServeHTTP(response, httptest.NewRequest(http.MethodGet, check.path, nil))
		if response.Code != check.status {
			t.Fatalf("%s: status %d", check.path, response.Code)
		}
	}
}

func TestIncusExecResponderDefersOnlyQueuedCallsToSteps(t *testing.T) {
	fake, err := NewIncusServer(TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	fake.QueueExec(IncusServerExecStep{Stdout: []byte("queued")})
	fake.SetExecResponder(func(call IncusServerExecCall) IncusServerExecStep {
		if slices.Equal(call.Command, []string{"answered"}) {
			return IncusServerExecStep{Stdout: []byte("answered")}
		}
		return IncusServerExecStep{Queued: true}
	})
	start := func(command string) *incusOperation {
		t.Helper()
		post := httptest.NewRecorder()
		fake.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/1.0/instances/yard/exec?project=scope", bytes.NewBufferString(`{"command":["`+command+`"]}`)))
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if post.Code != http.StatusAccepted {
			t.Fatalf("%s exec: %d", command, post.Code)
		}
		return fake.operations["operation-"+string(rune('0'+fake.nextOp))]
	}
	if operation := start("answered"); string(operation.step.Stdout) != "answered" {
		t.Fatalf("responder answer replaced: %q", operation.step.Stdout)
	}
	if operation := start("deferred"); string(operation.step.Stdout) != "queued" {
		t.Fatalf("deferred call not served from the queue: %q", operation.step.Stdout)
	}
	post := httptest.NewRecorder()
	fake.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/1.0/instances/yard/exec?project=scope", bytes.NewBufferString(`{"command":["deferred"]}`)))
	if post.Code != http.StatusInternalServerError {
		t.Fatalf("deferred call without a queued step accepted: %d", post.Code)
	}
}
