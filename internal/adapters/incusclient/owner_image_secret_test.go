package incusclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

type ownerSecretObserver struct {
	mu                         sync.Mutex
	before, accepted, terminal []ports.NativeOperation
	uncertain                  bool
	fail                       string
	published                  chan struct{}
}

func (o *ownerSecretObserver) Before(v ports.NativeOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.before = append(o.before, v)
	if o.fail == "before" {
		return errors.New("synthetic observer failure")
	}
	return nil
}
func (o *ownerSecretObserver) Accepted(v ports.NativeOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.accepted = append(o.accepted, v)
	if o.fail == "accepted" {
		return errors.New("synthetic observer failure")
	}
	return nil
}
func (o *ownerSecretObserver) Terminal(v ports.NativeOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail == "terminal" {
		return errors.New("synthetic observer failure")
	}
	o.terminal = append(o.terminal, v)
	close(o.published)
	return nil
}
func (o *ownerSecretObserver) Uncertain() { o.mu.Lock(); defer o.mu.Unlock(); o.uncertain = true }
func (*ownerSecretObserver) CommandStarted(string, string) (uint64, error) {
	return 0, errors.New("unexpected command")
}
func (*ownerSecretObserver) CommandTerminal(uint64) error { return errors.New("unexpected command") }

// The wait handler holds the original operation after its lookup disappears,
// matching Incus 6.0.6 and 6.23.0's header-flush / Wait / Render ordering.
type ownerSecretFixture struct {
	mu                     sync.Mutex
	server                 *httptest.Server
	op                     api.Operation
	final                  chan struct{}
	armed                  chan struct{}
	lookupGone             bool
	waitFail, waitDrop     bool
	corrupt                string
	deletes, posts, events int
	project                string
	metadata               int
	waitBody               <-chan struct{}
	lookupObserved         chan struct{}
	lookupOnce             sync.Once
}

func newOwnerSecretFixture(t *testing.T) (*ownerSecretFixture, incus.ConnectionArgs, ports.NativeOperation, *ownerSecretObserver, context.Context) {
	t.Helper()
	f := &ownerSecretFixture{final: make(chan struct{}), armed: make(chan struct{}), op: api.Operation{ID: "11111111-2222-4333-8444-555555555555", Class: api.OperationClassToken, StatusCode: api.Running, Resources: map[string][]string{"images": {"/1.0/images/" + strings.Repeat("a", 64)}}, Metadata: map[string]any{"secret": strings.Repeat("b", 64)}}}
	f.project = "source-project"
	f.server = httptest.NewUnstartedServer(f)
	f.server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}))
	key, err := x509.MarshalPKCS8PrivateKey(f.server.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	args := incus.ConnectionArgs{TLSServerCert: cert, TLSClientCert: cert, TLSClientKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})), SkipGetServer: true, SkipGetEvents: true}
	args.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	server, err := incus.ConnectIncusWithContext(context.Background(), f.server.URL, &args)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := nativeOperation(server.UseProject("source-project"), "owner-image-secret-current-primary", "source-project", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	o := &ownerSecretObserver{published: make(chan struct{})}
	ctx, stop := context.WithTimeout(context.Background(), 4*time.Second)
	t.Cleanup(stop)
	ctx = ports.WithNativeOperationObserver(ctx, o)
	ctx = ports.WithNativeInvocationKey(ctx, "synthetic-original-invocation")
	return f, args, expected, o, ctx
}

func (f *ownerSecretFixture) reply(w http.ResponseWriter, op api.Operation, async bool) {
	metadata, _ := json.Marshal(op)
	response := api.Response{Type: api.SyncResponse, StatusCode: 200, Metadata: metadata}
	if async {
		response.Type = api.AsyncResponse
		response.StatusCode = 100
		response.Operation = "/1.0/operations/" + op.ID
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (f *ownerSecretFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("project") != f.project || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "wrong original namespace or TLS identity", 403)
		return
	}
	f.mu.Lock()
	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/images/aliases/"):
		f.metadata++
		f.mu.Unlock()
		metadata, _ := json.Marshal(api.ImageAliasesEntry{Name: "debian/13", Type: "container", ImageAliasesEntryPut: api.ImageAliasesEntryPut{Target: strings.Repeat("a", 64)}})
		_ = json.NewEncoder(w).Encode(api.Response{Type: api.SyncResponse, StatusCode: 200, Metadata: metadata})
		return
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/images"):
		f.metadata++
		fingerprint := strings.Repeat("a", 64)
		if f.corrupt == "image-fingerprint" {
			fingerprint = strings.Repeat("f", 64)
		}
		f.mu.Unlock()
		var image any = api.Image{Fingerprint: fingerprint, Type: "container"}
		if strings.HasSuffix(r.URL.Path, "/images") {
			image = []api.Image{image.(api.Image)}
		}
		metadata, _ := json.Marshal(image)
		_ = json.NewEncoder(w).Encode(api.Response{Type: api.SyncResponse, StatusCode: 200, Metadata: metadata})
		return
	case strings.HasSuffix(r.URL.Path, "/secret") && r.Method == http.MethodPost:
		f.posts++
		op := f.op
		switch f.corrupt {
		case "class":
			op.Class = api.OperationClassTask
		case "uuid":
			op.ID = ""
		case "resource":
			op.Resources = map[string][]string{"images": {"/1.0/images/" + strings.Repeat("f", 64)}}
		case "secret":
			op.Metadata = nil
		}
		f.mu.Unlock()
		f.reply(w, op, true)
		return
	case strings.HasSuffix(r.URL.Path, "/wait"):
		if f.waitFail {
			f.mu.Unlock()
			http.Error(w, "synthetic wait failure", 403)
			return
		}
		if r.URL.Query().Get("timeout") != "-1" {
			f.mu.Unlock()
			http.Error(w, "wrong wait bound", 400)
			return
		}
		// Capture the operation before eviction; keep this request outstanding.
		close(f.armed)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-f.final:
		case <-r.Context().Done():
			return
		}
		if f.waitBody != nil {
			select {
			case <-f.waitBody:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		op, drop := f.op, f.waitDrop
		f.mu.Unlock()
		if drop {
			return
		} // headers alone never constitute terminal evidence
		f.reply(w, op, false)
		return
	case r.Method == http.MethodDelete:
		f.deletes++
		if f.op.StatusCode == api.Running {
			f.op.StatusCode = api.Cancelled
			close(f.final)
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.Response{Type: api.SyncResponse, StatusCode: 200})
		return
	case strings.Contains(r.URL.Path, "/events"):
		f.events++
		f.mu.Unlock()
		http.Error(w, "unexpected event connection", 500)
		return
	default:
		op, gone := f.op, f.lookupGone
		f.mu.Unlock()
		if gone {
			if f.lookupObserved != nil {
				f.lookupOnce.Do(func() { close(f.lookupObserved) })
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 404, "error": "original operation gone"})
			return
		}
		f.reply(w, op, false)
	}
}

func (f *ownerSecretFixture) consume(evict bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op.StatusCode = api.Cancelled
	f.lookupGone = evict
	close(f.final)
}

func TestOwnerImageSecretRetainsTerminalBeforeTargetCompletes(t *testing.T) {
	f, args, expected, o, ctx := newOwnerSecretFixture(t)
	h, err := beginOwnerImageSecret(ctx, f.server.URL, args, expected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	select {
	case <-f.armed:
	default:
		t.Fatal("target could start before wait was armed")
	}
	source := h.Source()
	if source.Secret != strings.Repeat("b", 64) || source.Fingerprint != expected.Resource || source.Project != expected.Project {
		t.Fatal("transient source differs")
	}
	f.consume(true)
	select {
	case <-o.published:
	case <-ctx.Done():
		t.Fatal("terminal not published while target remains blocked")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal("Close not idempotent")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.before) != 1 || len(o.accepted) != 1 || len(o.terminal) != 1 || o.terminal[0].Status != 401 || o.terminal[0].ID != f.op.ID || o.uncertain {
		t.Fatal("exact token custody differs")
	}
	raw, _ := json.Marshal(o.terminal)
	if strings.Contains(string(raw), source.Secret) {
		t.Fatal("secret persisted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletes != 0 || f.posts != 1 || f.events != 0 {
		t.Fatal("unexpected effect or early cancellation")
	}
}

func TestOwnerImageSecretSetupFailurePreventsTarget(t *testing.T) {
	f, args, expected, o, ctx := newOwnerSecretFixture(t)
	f.waitFail = true
	h, err := beginOwnerImageSecret(ctx, f.server.URL, args, expected)
	if h != nil || !errors.Is(err, errOwnerImageSecret) {
		t.Fatal("wait failure admitted target")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletes != 1 || f.op.StatusCode != api.Cancelled {
		t.Fatal("setup failure did not drain original token")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.uncertain || len(o.terminal) != 0 {
		t.Fatal("failed wait manufactured a terminal")
	}
}

func TestOwnerImageSecretDroppedWaitAnd404RemainUnknown(t *testing.T) {
	f, args, expected, o, ctx := newOwnerSecretFixture(t)
	f.waitDrop = true
	h, err := beginOwnerImageSecret(ctx, f.server.URL, args, expected)
	if err != nil {
		t.Fatal(err)
	}
	f.consume(true)
	select {
	case <-h.WorkContext().Done():
	case <-ctx.Done():
		t.Fatal("watch failure did not cancel target")
	}
	if err := h.Close(); !errors.Is(err, errOwnerImageSecret) {
		t.Fatal("missing terminal became success")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.uncertain || len(o.terminal) != 0 {
		t.Fatal("absence manufactured terminal evidence")
	}
}

func TestOwnerImageSecretDelayedTerminalReconcilesCleanupAbsence(t *testing.T) {
	for _, mode := range []string{"terminal", "wrong-UUID", "publication-failure", "late-terminal"} {
		t.Run(mode, func(t *testing.T) {
			f, args, expected, observer, ctx := newOwnerSecretFixture(t)
			if mode == "late-terminal" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 250*time.Millisecond)
				defer stop()
			}
			body := make(chan struct{})
			f.waitBody, f.lookupObserved = body, make(chan struct{})
			if mode == "publication-failure" {
				observer.fail = "terminal"
			}
			handle, err := beginOwnerImageSecret(ctx, f.server.URL, args, expected)
			if err != nil {
				t.Fatal(err)
			}
			f.consume(true)
			if mode == "wrong-UUID" {
				f.mu.Lock()
				f.op.ID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
				f.mu.Unlock()
			}
			result := make(chan error, 1)
			go func() { result <- handle.Close() }()
			select {
			case <-f.lookupObserved:
			case <-ctx.Done():
				t.Fatal("cleanup did not observe original map eviction")
			}
			if mode == "late-terminal" {
				<-ctx.Done()
			}
			close(body)
			select {
			case err := <-result:
				if (err == nil) != (mode == "terminal") {
					t.Fatal("cleanup absence overrode exact evidence or manufactured closure", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cleanup did not join original watch")
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if mode == "terminal" && (observer.uncertain || len(observer.terminal) != 1 || observer.terminal[0].ID != handle.operation.ID || observer.terminal[0].Status != 401) {
				t.Fatal("exact delayed token terminal lost")
			}
		})
	}
}

func TestOwnerImageSecretCancellationDrainsAndJoins(t *testing.T) {
	f, args, expected, o, ctx := newOwnerSecretFixture(t)
	ctx, cancel := context.WithCancelCause(ctx)
	h, err := beginOwnerImageSecret(ctx, f.server.URL, args, expected)
	if err != nil {
		t.Fatal(err)
	}
	original := errors.New("synthetic target failure")
	cancel(original)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(h.WorkContext()), original) {
		t.Fatal("original target failure lost")
	}
	select {
	case <-h.done:
	default:
		t.Fatal("watcher survived Close")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.terminal) != 1 {
		t.Fatal("cancelled token not published")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletes != 1 {
		t.Fatal("original token not cancelled exactly once")
	}
}

func TestOwnerImageSecretPublicationFailureCancelsTarget(t *testing.T) {
	f, args, expected, o, ctx := newOwnerSecretFixture(t)
	o.fail = "terminal"
	h, err := beginOwnerImageSecret(ctx, f.server.URL, args, expected)
	if err != nil {
		t.Fatal(err)
	}
	f.consume(true)
	select {
	case <-h.WorkContext().Done():
	case <-ctx.Done():
		t.Fatal("publication failure did not cancel target")
	}
	if err := h.Close(); !errors.Is(err, errOwnerImageSecret) || strings.Contains(err.Error(), strings.Repeat("b", 64)) {
		t.Fatal("publication failure hidden or disclosed secret")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.uncertain || len(o.terminal) != 0 {
		t.Fatal("publication failure gained closure")
	}
}

func TestOwnerImageSecretMissingLookupCannotOutliveCustodyDeadline(t *testing.T) {
	f, args, expected, _, ctx := newOwnerSecretFixture(t)
	ctx, stop := context.WithTimeout(ctx, 600*time.Millisecond)
	defer stop()
	h, err := beginOwnerImageSecret(ctx, f.server.URL, args, expected)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lookupGone = true
	f.mu.Unlock()
	start := time.Now()
	if err := h.Close(); !errors.Is(err, errOwnerImageSecret) {
		t.Fatal("missing lookup gained closure")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close escaped original custody deadline")
	}
	select {
	case <-h.done:
	default:
		t.Fatal("watcher escaped Close")
	}
}

func TestOwnerImageSecretStalledTLSClosesOwnedConnection(t *testing.T) {
	for _, path := range []string{"begin-failure", "close-wrapper"} {
		t.Run(path, func(t *testing.T) {
			_, args, expected, observer, ctx := newOwnerSecretFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted, hello, closed := make(chan net.Conn, 1), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(closed)
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				accepted <- connection
				buffer := make([]byte, 4096)
				if n, _ := connection.Read(buffer); n > 0 {
					close(hello)
				}
				_, _ = io.Copy(io.Discard, connection) // deliberately never answer TLS
			}()
			address := "https://" + listener.Addr().String()
			work, cancelWork := context.WithCancel(ctx)
			defer cancelWork()
			result := make(chan error, 1)
			var transport *ownerImageSecretTransport
			var server incus.InstanceServer
			if path == "begin-failure" {
				original, err := incus.ConnectIncusWithContext(ctx, address, &args)
				if err != nil {
					t.Fatal(err)
				}
				expected, err = nativeOperation(original.UseProject(expected.Project), expected.Kind, expected.Project, expected.Resource)
				if err != nil {
					t.Fatal(err)
				}
				go func() { _, err := beginOwnerImageSecret(work, address, args, expected); result <- err }()
			} else {
				transport = &ownerImageSecretTransport{ctx: ctx, work: work}
				server, err = ownerImageSecretConnect(ctx, address, args, expected.Project, transport, 0)
				if err != nil {
					t.Fatal(err)
				}
				go func() { _, err := server.CreateImageSecret(expected.Resource); result <- err }()
			}
			var connection net.Conn
			select {
			case connection = <-accepted:
				defer connection.Close()
			case <-ctx.Done():
				t.Fatal("source TCP connection missing")
			}
			select {
			case <-hello:
			case <-ctx.Done():
				t.Fatal("source did not enter TLS handshake")
			}
			cancelWork()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("stalled TLS admitted an operation")
				}
			case <-ctx.Done():
				t.Fatal("request survived cancellation")
			}
			if transport != nil {
				client, _ := server.GetHTTPClient()
				client.CloseIdleConnections() // must forward, cancel and join owned callbacks
				transport.dials.Wait()
				transport.dialMu.Lock()
				stopped := transport.closed
				transport.dialMu.Unlock()
				if !stopped {
					t.Fatal("wrapper cleanup was not forwarded")
				}
				late, err := transport.base.DialContext(context.Background(), "tcp", listener.Addr().String())
				if late != nil {
					_ = late.Close()
				}
				if late != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("closed transport admitted a late dial")
				}
				late, err = transport.base.DialTLSContext(context.Background(), "tcp", listener.Addr().String())
				if late != nil {
					_ = late.Close()
				}
				if late != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("closed transport admitted a late TLS callback")
				}
			}
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("owned TCP socket survived cleanup")
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if len(observer.accepted) != 0 || len(observer.terminal) != 0 || path == "begin-failure" && !observer.uncertain {
				t.Fatal("failed TLS gained token acceptance or closure")
			}
		})
	}
}

func TestOwnerImageSecretRejectsForeignIntentAndOperation(t *testing.T) {
	for _, changed := range []string{"kind", "fingerprint", "server", "url", "fragment", "auth", "certificate", "class", "uuid", "resource", "secret", "before", "accepted"} {
		t.Run(changed, func(t *testing.T) {
			f, args, expected, o, ctx := newOwnerSecretFixture(t)
			address := f.server.URL
			switch changed {
			case "kind":
				expected.Kind = "unbound-token"
			case "fingerprint":
				expected.Resource = "foreign"
			case "server":
				expected.Server = strings.Repeat("f", 64)
			case "url":
				address += "?credential=synthetic-marker"
			case "fragment":
				address += "#synthetic-marker"
			case "auth":
				args.AuthType = api.AuthenticationMethodOIDC
			case "certificate":
				args.TLSClientKey = ""
			case "class", "uuid", "resource", "secret":
				f.corrupt = changed
			case "before", "accepted":
				o.fail = changed
			}
			h, err := beginOwnerImageSecret(ctx, address, args, expected)
			if h != nil || err == nil || strings.Contains(err.Error(), "synthetic-marker") {
				t.Fatal("foreign intent/operation admitted or leaked")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if changed != "class" && changed != "uuid" && changed != "resource" && changed != "secret" && changed != "accepted" && f.posts != 0 {
				t.Fatal("foreign intent reached source")
			}
			if (changed == "class" || changed == "uuid" || changed == "resource") && f.deletes != 0 {
				t.Fatal("foreign operation cancelled")
			}
			if (changed == "accepted" || changed == "secret") && f.deletes != 1 {
				t.Fatal("unpublished original acceptance not cleaned")
			}
		})
	}
}
