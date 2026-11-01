package incusclient

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/gorilla/websocket"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

type agentProbeObserver struct {
	custodyObserver
	before int
}

func (o *agentProbeObserver) Before(ports.NativeOperation) error { o.before++; return nil }
func (o *agentProbeObserver) empty() bool {
	return o.before == 0 && len(o.accepted) == 0 && len(o.terminal) == 0 && !o.uncertain
}
func setProbeVM(server *testkit.IncusServer, uuid string) {
	setProbeKind(server, uuid, domain.YardVM)
}
func setProbeKind(server *testkit.IncusServer, uuid string, kind domain.YardKind) {
	apiKind := "virtual-machine"
	if kind == domain.YardContainer {
		apiKind = "container"
	}
	cfg := map[string]string{"volatile.uuid": uuid}
	server.SetInstance("p", "yard", map[string]any{"name": "yard", "type": apiKind, "status": "Running", "config": cfg, "expanded_config": maps.Clone(cfg)})
}

func TestProbeInstanceAgentIfRequiresExplicitZeroAndOriginalUUID(t *testing.T) {
	probeAgentReturnMatrix(t, domain.YardVM, false)
}
func TestProbeOriginalInstanceAgentIfRequiresExplicitZeroAndOriginalKind(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardVM, domain.YardContainer} {
		t.Run(string(kind), func(t *testing.T) { probeAgentReturnMatrix(t, kind, true) })
	}
}
func probeAgentReturnMatrix(t *testing.T, kind domain.YardKind, original bool) {
	for _, mode := range []string{"zero", "nonzero", "negative", "missing", "string", "fractional", "malformed", "overflow", "remote-failure", "before-uuid", "after-uuid", "missing-uuid", "before-kind", "after-kind"} {
		t.Run(mode, func(t *testing.T) {
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			setProbeKind(server, "original", kind)
			metadata := map[string]any{"return": 0}
			switch mode {
			case "nonzero":
				metadata["return"] = 17
			case "negative":
				metadata["return"] = -1
			case "missing":
				delete(metadata, "return")
			case "string":
				metadata["return"] = "0"
			case "fractional":
				metadata["return"] = 0.5
			case "malformed":
				metadata["return"] = []any{0}
			case "overflow":
				metadata["return"] = 1e100
			}
			step := testkit.IncusServerExecStep{ReturnMetadata: metadata}
			if mode == "remote-failure" {
				step.OperationError = "agent failed"
			}
			if mode == "before-uuid" {
				setProbeKind(server, "replacement", kind)
			}
			if mode == "after-uuid" {
				step.AfterInput = func(testkit.IncusServerExecCall) int { setProbeKind(server, "replacement", kind); return 0 }
			}
			otherKind := domain.YardContainer
			if kind == domain.YardContainer {
				otherKind = domain.YardVM
			}
			if mode == "before-kind" {
				setProbeKind(server, "original", otherKind)
			}
			if mode == "after-kind" {
				step.AfterInput = func(testkit.IncusServerExecCall) int { setProbeKind(server, "original", otherKind); return 0 }
			}
			server.QueueExec(step)
			observer := &agentProbeObserver{}
			ctx, cancel := context.WithTimeout(ports.WithNativeOperationObserver(context.Background(), observer), time.Second)
			defer cancel()
			uuid := "original"
			if mode == "missing-uuid" {
				uuid = ""
			}
			client := New(server.SocketPath)
			var ready bool
			if original {
				ready, err = client.ProbeOriginalInstanceAgentIf(ctx, "p", "yard", uuid, kind)
			} else {
				ready, err = client.ProbeInstanceAgentIf(ctx, "p", "yard", uuid)
			}
			if ready != (mode == "zero") || (err == nil) != (mode == "zero") {
				t.Fatalf("unproved agent readiness: ready=%v err=%v", ready, err)
			}
			calls := server.ExecCalls()
			want := 1
			if mode == "before-uuid" || mode == "missing-uuid" || mode == "before-kind" {
				want = 0
			}
			if len(calls) != want || want == 1 && (len(calls[0].Command) != 1 || calls[0].Command[0] != "true" || len(calls[0].Environment) != 0 || len(calls[0].Stdin) != 0) {
				t.Fatal("probe accepted an unbound command", calls)
			}
			if (strings.Contains(mode, "uuid") || strings.Contains(mode, "kind")) && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("original UUID was not retained", err)
			}
			if !observer.empty() || len(server.PowerCalls()) != 0 {
				t.Fatal("read-only probe created mutation facts", observer)
			}
		})
	}
}

// Existing SDK/API fixture plus withheld output websockets exercises the real
// DataDone path. Test-owned release closes the sockets after bounded refusal.
func TestProbeInstanceAgentIfBoundsWithheldSDKStreams(t *testing.T) {
	probeAgentStreams(t, domain.YardVM, false)
}
func TestProbeOriginalInstanceAgentIfBoundsContainerSDKStreams(t *testing.T) {
	probeAgentStreams(t, domain.YardContainer, false)
}
func TestOfficialClientExecBoundsWithheldSDKStreams(t *testing.T) {
	probeAgentStreams(t, domain.YardContainer, true)
}
func probeAgentStreams(t *testing.T, kind domain.YardKind, ordinaryExec bool) {
	boundaries := []string{"streams", "handshake"}
	if ordinaryExec {
		boundaries = append(boundaries, "native-handshake")
	}
	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			fixture, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer fixture.Close()
			setProbeKind(fixture, "original", kind)
			fixture.QueueExec(testkit.IncusServerExecStep{})
			release := make(chan struct{})
			blocked := make(chan struct{})
			waited := make(chan struct{})
			var once, waitOnce sync.Once
			var handlers sync.WaitGroup
			listener, err := net.Listen("unix", filepath.Join(testkit.TempDir(t), "probe.socket"))
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/websocket") && (strings.HasSuffix(r.URL.Query().Get("secret"), "-stdout") || strings.HasSuffix(r.URL.Query().Get("secret"), "-stderr")) {
					handlers.Add(1)
					defer handlers.Done()
					once.Do(func() { close(blocked) })
					if strings.HasSuffix(boundary, "handshake") {
						<-release
						return
					}
					conn, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if e != nil {
						return
					}
					defer conn.Close()
					_ = conn.WriteMessage(websocket.BinaryMessage, []byte("discarded probe output"))
					<-release
					return
				}
				if strings.HasSuffix(r.URL.Path, "/wait") {
					waitOnce.Do(func() { close(waited) })
				}
				fixture.ServeHTTP(w, r)
			})}
			go server.Serve(listener)
			defer func() { close(release); server.Close(); listener.Close(); handlers.Wait() }()
			observer := &agentProbeObserver{}
			parent := context.Background()
			if !ordinaryExec || boundary == "native-handshake" {
				parent = ports.WithNativeOperationObserver(parent, observer)
			}
			ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
			defer cancel()
			start := time.Now()
			client := New(listener.Addr().String())
			var ready bool
			if ordinaryExec {
				_, err = client.Exec(ctx, "p", "yard", ports.InstanceExecRequest{Command: []string{"true"}})
			} else {
				ready, err = client.ProbeOriginalInstanceAgentIf(ctx, "p", "yard", "original", kind)
			}
			elapsed := time.Since(start)
			if !ordinaryExec && (ready || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "unproved") || elapsed > 2*time.Second) {
				t.Fatal("withheld SDK boundary escaped probe deadline", ready, err, time.Since(start))
			}
			select {
			case <-blocked:
			default:
				t.Fatal("fixture did not reach the withheld SDK boundary")
			}
			if ordinaryExec {
				t.Logf("Exec reached withheld %s: deadline_error=%t elapsed=%s error_type=%T", boundary, errors.Is(err, context.DeadlineExceeded), elapsed, err)
				if !errors.Is(err, context.DeadlineExceeded) || elapsed > 2*time.Second {
					t.Fatal("ordinary Exec escaped its caller deadline", err, elapsed)
				}
			}
			if boundary == "streams" {
				select {
				case <-waited:
				default:
					t.Fatal("fixture did not reach SDK operation wait before stream drain")
				}
			}
			if boundary == "native-handshake" {
				if observer.before != 1 || !observer.uncertain || len(observer.accepted) != 0 || len(observer.terminal) != 0 {
					t.Fatal("withheld native handshake lost dispatch uncertainty", observer)
				}
			} else if !observer.empty() {
				t.Fatal("withheld read-only boundary fabricated mutation completion", observer)
			}
		})
	}
}

type stalledAgentProbeOperation struct {
	incus.Operation
	waitRelease, cancelRelease          <-chan struct{}
	cancelStarted, waitDone, cancelDone chan struct{}
}

func (o *stalledAgentProbeOperation) WaitContext(context.Context) error {
	defer close(o.waitDone)
	if o.waitRelease != nil {
		<-o.waitRelease
	}
	return nil
}
func (o *stalledAgentProbeOperation) Cancel() error {
	close(o.cancelStarted)
	defer close(o.cancelDone)
	<-o.cancelRelease
	return nil
}
func (*stalledAgentProbeOperation) Get() api.Operation {
	return api.Operation{StatusCode: api.Success, Metadata: map[string]any{"return": float64(0)}}
}
func TestInstanceAgentProbeDoesNotJoinStalledCancelOrWait(t *testing.T) {
	for _, boundary := range []string{"wait", "stream"} {
		t.Run(boundary, func(t *testing.T) {
			release := make(chan struct{})
			o := &stalledAgentProbeOperation{cancelRelease: release, cancelStarted: make(chan struct{}), waitDone: make(chan struct{}), cancelDone: make(chan struct{})}
			if boundary == "wait" {
				o.waitRelease = release
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := finishInstanceAgentProbe(ctx, o, make(chan bool))
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
				close(release)
				t.Fatal("probe joined stalled SDK cancellation/wait", err, time.Since(start))
			}
			select {
			case <-o.cancelStarted:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("probe did not request cancellation")
			}
			close(release)
			for _, done := range []<-chan struct{}{o.waitDone, o.cancelDone} {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("test-owned SDK worker did not stop")
				}
			}
		})
	}
}

func TestInstanceAgentProbeRejectsNonFinalNilSDKWait(t *testing.T) {
	operation := &custodyOperation{value: api.Operation{StatusCode: api.Running, Metadata: map[string]any{"return": float64(0)}}}
	done := make(chan bool)
	close(done)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := finishInstanceAgentProbe(ctx, operation, done); err == nil || !strings.Contains(err.Error(), "remote success unproved") {
		t.Fatal("nonfinal nil SDK wait became ready", err)
	}
}
