package incusclient

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

var errOwnerImageSecret = errors.New("original image token custody unproved")
var errOwnerImageSecretAbsent = errors.New("original image token lookup absent")

// ownerImageSecret owns one token and one wait request. Source is transient;
// only the public NativeOperation projection reaches the existing observer.
type ownerImageSecret struct {
	work       context.Context
	cancelWork context.CancelCauseFunc
	stopWork   context.CancelFunc
	stop       context.CancelFunc
	custody    context.Context
	transport  *ownerImageSecretTransport
	control    incus.InstanceServer
	watch      incus.InstanceServer
	operation  ports.NativeOperation
	source     api.InstanceSource
	done       chan struct{}
	err        error // written only by watcher, read after done
	terminal   ports.NativeOperation
	once       sync.Once
	closeErr   error
}

// beginOwnerImageSecret accepts already loaded static TLS arguments only. Its
// caller must have checked the original policy guards, defer Close, and use
// WorkContext for the single target create.
// Returning proves the exact wait request is armed before that create starts.
func beginOwnerImageSecret(ctx context.Context, address string, args incus.ConnectionArgs, expected ports.NativeOperation) (*ownerImageSecret, error) {
	observer := ports.NativeOperationObserverFrom(ctx)
	deadline, bounded := ctx.Deadline()
	u, err := url.Parse(address)
	validScope := observer != nil && ports.NativeInvocationKey(ctx) != "" && bounded && time.Until(deadline) > 0 && ownerImageSecretKind(expected.Kind) && domain.SafeName(expected.Project) && ownerImageHex(expected.Server, 64) && ownerImageHex(expected.Resource, 64) && expected.ID == "" && expected.Status == 0
	validURL := err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawFragment == ""
	staticTLS := (args.AuthType == "" || args.AuthType == api.AuthenticationMethodTLS) && args.TLSClientCert != "" && args.TLSClientKey != "" && args.OIDCTokens == nil && args.CookieJar == nil && (args.HTTPClient == nil || args.HTTPClient.Jar == nil) && args.TransportWrapper == nil && !args.InsecureSkipVerify && !args.IdenticalCertificate
	if !validScope || !validURL || !staticTLS {
		return nil, errOwnerImageSecret
	}
	// Survive ordinary cancellation only inside the original custody deadline.
	custody, stop := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	work, cancelWork := context.WithCancelCause(ctx)
	work, stopWork := context.WithDeadline(work, deadline.Add(-min(30*time.Second, time.Until(deadline)/6)))
	h := &ownerImageSecret{work: work, cancelWork: cancelWork, stopWork: stopWork, stop: stop, custody: custody, operation: expected, done: make(chan struct{})}
	controlTransport := &ownerImageSecretTransport{ctx: custody, work: work}
	h.transport = controlTransport
	h.control, err = ownerImageSecretConnect(custody, address, args, expected.Project, controlTransport, 30*time.Second)
	if err != nil {
		h.release()
		return nil, errOwnerImageSecret
	}
	identity, err := nativeOperation(h.control, expected.Kind, expected.Project, expected.Resource)
	if err != nil || identity != expected {
		h.release()
		return nil, errOwnerImageSecret
	}
	if err := observer.Before(expected); err != nil {
		h.release()
		return nil, errOwnerImageSecret
	}
	op, err := h.control.CreateImageSecret(expected.Resource)
	if err != nil || op == nil {
		observer.Uncertain()
		h.release()
		return nil, errOwnerImageSecret
	}
	remote := op.Get()
	h.operation.ID = remote.ID
	if !ownerImageSecretIdentity(remote, h.operation) || remote.StatusCode != api.Running {
		observer.Uncertain()
		h.release()
		return nil, errOwnerImageSecret
	}
	accepted := observer.Accepted(h.operation) == nil
	secret, validSecret := remote.Metadata["secret"].(string)
	h.source = api.InstanceSource{Type: "image", Mode: "pull", Protocol: "incus", Server: address, Certificate: args.TLSServerCert, Project: expected.Project, Fingerprint: expected.Resource, Secret: secret}
	ready, err := h.armWatch(custody, address, args, observer, accepted)
	if err != nil {
		observer.Uncertain()
		cleanup, stopCleanup := context.WithTimeout(custody, 30*time.Second)
		h.transport.ctx = cleanup
		_ = h.cleanup()
		stopCleanup()
		h.release()
		return nil, errOwnerImageSecret
	}
	arm := time.NewTimer(30 * time.Second)
	defer arm.Stop()
	select {
	case <-ready:
		if accepted && validSecret && ownerImageHex(secret, 64) && work.Err() == nil {
			return h, nil
		}
	case <-h.done:
	case <-work.Done():
	case <-arm.C:
	}
	observer.Uncertain()
	return nil, errors.Join(errOwnerImageSecret, h.Close())
}

func (h *ownerImageSecret) armWatch(ctx context.Context, address string, args incus.ConnectionArgs, observer ports.NativeOperationObserver, accepted bool) (<-chan struct{}, error) {
	ready := make(chan struct{})
	base, err := url.Parse(address)
	if err != nil {
		return nil, errOwnerImageSecret
	}
	watchTransport := &ownerImageSecretTransport{ctx: ctx, path: strings.TrimSuffix(base.Path, "/") + "/1.0/operations/" + h.operation.ID + "/wait", project: h.operation.Project, ready: ready}
	h.watch, err = ownerImageSecretConnect(ctx, address, args, h.operation.Project, watchTransport, 0)
	if err != nil {
		return nil, errOwnerImageSecret
	}
	go func() {
		defer close(h.done)
		terminal, _, waitErr := h.watch.GetOperationWait(h.operation.ID, -1)
		if waitErr != nil || ctx.Err() != nil || terminal == nil || !ownerImageSecretIdentity(*terminal, h.operation) || terminal.StatusCode != api.Cancelled || !accepted {
			h.err = errOwnerImageSecret
		} else {
			value := h.operation
			value.Status = int(terminal.StatusCode)
			h.terminal = value
			if observer != nil && observer.Terminal(value) != nil {
				h.err = errOwnerImageSecret
			}
		}
		if h.err != nil {
			if observer != nil {
				observer.Uncertain()
			}
			h.cancelWork(h.err)
		}
	}()
	return ready, nil
}

func (h *ownerImageSecret) WorkContext() context.Context { return h.work }
func (h *ownerImageSecret) Source() api.InstanceSource   { return h.source }

// Close never infers closure from absence. The watch response remains the sole
// terminal publisher, including when cancellation races normal consumption.
func (h *ownerImageSecret) Close() error {
	h.once.Do(func() {
		defer h.release()
		cleanup, stopCleanup := context.WithTimeout(h.custody, 30*time.Second)
		defer stopCleanup()
		h.transport.ctx = cleanup
		select {
		case <-h.done:
			if h.err != nil {
				h.closeErr = h.cleanup()
			}
		default:
			h.closeErr = h.cleanup()
		}
		select {
		case <-h.done:
		case <-cleanup.Done():
			h.closeErr = errors.Join(h.closeErr, errOwnerImageSecret)
			h.stop()
			<-h.done
		}
		// An already armed exact wait retains its operation after daemon-map
		// eviction. Only its authenticated, published terminal reconciles 404.
		terminal := h.operation
		terminal.Status = int(api.Cancelled)
		if errors.Is(h.closeErr, errOwnerImageSecretAbsent) && cleanup.Err() == nil && h.err == nil && h.terminal == terminal {
			h.closeErr = nil
		}
		h.closeErr = errors.Join(h.closeErr, h.err)
		if h.closeErr != nil {
			if observer := ports.NativeOperationObserverFrom(h.work); observer != nil {
				observer.Uncertain()
			}
		}
	})
	return h.closeErr
}

func (h *ownerImageSecret) cleanup() error {
	remote, _, err := h.control.GetOperation(h.operation.ID)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return errors.Join(errOwnerImageSecret, errOwnerImageSecretAbsent)
	}
	if err != nil || remote == nil || !ownerImageSecretIdentity(*remote, h.operation) {
		return errOwnerImageSecret
	}
	if !remote.StatusCode.IsFinal() {
		if err := h.control.DeleteOperation(h.operation.ID); err != nil {
			if api.StatusErrorCheck(err, http.StatusNotFound) {
				return errors.Join(errOwnerImageSecret, errOwnerImageSecretAbsent)
			}
			return errOwnerImageSecret
		}
		remote, _, err = h.control.GetOperation(h.operation.ID)
	}
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return errors.Join(errOwnerImageSecret, errOwnerImageSecretAbsent)
	}
	if err != nil || remote == nil || !ownerImageSecretIdentity(*remote, h.operation) || remote.StatusCode != api.Cancelled {
		return errOwnerImageSecret
	}
	return nil
}

func (h *ownerImageSecret) release() {
	h.cancelWork(errOwnerImageSecret)
	h.stopWork()
	h.stop()
	for _, server := range []incus.InstanceServer{h.control, h.watch} {
		if server != nil {
			client, err := server.GetHTTPClient()
			if err == nil {
				client.CloseIdleConnections()
			}
		}
	}
	h.source.Secret = ""
}

func ownerImageSecretConnect(ctx context.Context, address string, args incus.ConnectionArgs, project string, transport *ownerImageSecretTransport, timeout time.Duration) (incus.InstanceServer, error) {
	client := http.Client{}
	if args.HTTPClient != nil {
		client = *args.HTTPClient
	}
	client.Timeout = timeout
	args.HTTPClient = &client
	args.SkipGetServer, args.SkipGetEvents = true, true
	args.TransportWrapper = func(base *http.Transport) incus.HTTPTransporter {
		return transport.wrap(ctx, base)
	}
	server, err := incus.ConnectIncusWithContext(ctx, address, &args)
	if err != nil {
		return nil, err
	}
	return server.UseProject(project), nil
}

type ownerImageSecretTransport struct {
	base          *http.Transport
	ctx, work     context.Context
	path, project string
	ready         chan struct{}
	once          sync.Once
	dialMu        sync.Mutex
	dials         sync.WaitGroup
	closed        bool
	stopDial      context.CancelFunc
}

func (t *ownerImageSecretTransport) wrap(ctx context.Context, base *http.Transport) incus.HTTPTransporter {
	t.base = base
	// Transport detaches request cancellation while dialing. Keep the
	// original custody lifetime, including for a stalled TLS handshake.
	dialContext, stopDial := context.WithCancel(ctx)
	t.stopDial = stopDial
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 3 * time.Second}
	dialRaw := func(ctx context.Context, network, address string) (net.Conn, error) {
		t.dialMu.Lock()
		if t.closed {
			t.dialMu.Unlock()
			return nil, context.Canceled
		}
		t.dials.Add(1)
		t.dialMu.Unlock()
		defer t.dials.Done()
		connection, err := dialer.DialContext(ctx, network, address)
		if err == nil {
			t.dials.Add(1)
			context.AfterFunc(dialContext, func() { defer t.dials.Done(); _ = connection.Close() })
		}
		return connection, err
	}
	base.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		return dialRaw(dialContext, network, address)
	}
	base.DialTLSContext = func(_ context.Context, network, address string) (net.Conn, error) {
		t.dialMu.Lock()
		if t.closed {
			t.dialMu.Unlock()
			return nil, context.Canceled
		}
		t.dials.Add(1)
		t.dialMu.Unlock()
		defer t.dials.Done()
		budget, stop := context.WithTimeout(dialContext, base.TLSHandshakeTimeout)
		defer stop()
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		// Preserve the SDK's verified redirect retry: first the configured
		// certificate name, then the actual endpoint host, with the same roots.
		for attempt := 0; attempt < 2; attempt++ {
			config := base.TLSClientConfig.Clone()
			if config.ServerName == "" || attempt == 1 {
				config.ServerName = host
			}
			var connection net.Conn
			connection, err = dialRaw(budget, network, address)
			if err != nil {
				return nil, err
			}
			secure := tls.Client(connection, config)
			err = secure.HandshakeContext(budget)
			if err == nil {
				return secure, nil
			}
			_ = connection.Close()
			if budget.Err() != nil {
				return nil, err
			}
		}
		return nil, err
	}
	return t
}

func (t *ownerImageSecretTransport) Transport() *http.Transport { return t.base }
func (t *ownerImageSecretTransport) CloseIdleConnections() {
	t.dialMu.Lock()
	t.closed = true
	t.dialMu.Unlock()
	t.stopDial()
	t.base.CloseIdleConnections()
	t.dials.Wait()
}
func (t *ownerImageSecretTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := t.ctx
	if request.Method == http.MethodPost {
		ctx = t.work
	}
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err == nil && t.ready != nil && request.Method == http.MethodGet && request.URL.Path == t.path && request.URL.Query().Get("timeout") == "-1" && request.URL.Query().Get("project") == t.project && response.StatusCode == http.StatusOK {
		t.once.Do(func() { close(t.ready) })
	}
	if err == nil && response.Body != nil {
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(response.Body, ownerImageLimit+1), response.Body}
	}
	return response, err
}

func ownerImageSecretKind(kind string) bool {
	for _, role := range []string{"current", "legacy", "recreate"} {
		if kind == "owner-image-secret-"+role+"-primary" || kind == "owner-image-secret-"+role+"-fallback" {
			return true
		}
	}
	return false
}

func ownerImageHex(value string, size int) bool {
	return len(value) == size && strings.Trim(value, "0123456789abcdef") == ""
}

func ownerImageSecretIdentity(remote api.Operation, expected ports.NativeOperation) bool {
	id := remote.ID
	if !ownerImageSecretUUID(id) || id != expected.ID || remote.Class != api.OperationClassToken || len(remote.Resources) != 1 || len(remote.Resources["images"]) != 1 {
		return false
	}
	resource, err := url.Parse(remote.Resources["images"][0])
	return err == nil && !resource.IsAbs() && resource.Host == "" && resource.User == nil && resource.Path == "/1.0/images/"+expected.Resource && resource.RawPath == "" && resource.RawQuery == "" && !resource.ForceQuery && resource.Fragment == "" && resource.RawFragment == ""
}

func ownerImageSecretUUID(id string) bool {
	return len(id) == 36 && id[8] == '-' && id[13] == '-' && id[18] == '-' && id[23] == '-' && ownerImageHex(strings.ReplaceAll(id, "-", ""), 32)
}
