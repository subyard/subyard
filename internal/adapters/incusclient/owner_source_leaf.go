package incusclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/lxc/incus/v6/shared/api"
	"golang.org/x/sys/unix"
)

const ownerSourceLimit = 32 << 20
const ownerBrowserMarker = "SUBYARD_PRIVATE_OWNER_BROWSER"
const ownerSourceMarker = "SUBYARD_PRIVATE_OWNER_SOURCE"

type ownerSourceRequest struct {
	Expected                ports.NativeOperation
	Selected                string
	Policy                  ports.PreparedOwnerImagePolicy
	Instance                ports.PreparedOwnerInstance
	Primary, Fallback       string
	Kind                    domain.YardKind
	Environment             []string
	Home, Directory, Socket string
	Extensions              []string
	Invocation, OwnerHome   string
	Deadline                int64
}
type ownerSourceMessage struct {
	Kind         string
	Operation    ports.NativeOperation
	Intent       ports.NativeOwnerCreateIntent
	Ref          ports.NativeOwnerCreateRef
	Created      *ports.NativeOwnerCreated
	Resolved     ports.PreparedOwnerInstance
	Policy       ports.PreparedOwnerImagePolicy
	Cache        ports.OwnerImageCacheSuccessor
	Caches       []ports.OwnerImageCacheSuccessor
	Key          string
	Data         []byte
	Ordinal      uint64
	URI          string
	Error, Stale bool
	Failure      *ports.NativeOperation
}
type ownerSourceWire struct {
	conn     net.Conn
	sequence uint64
	mu       sync.Mutex
}

func (w *ownerSourceWire) send(v any) error {
	raw, err := nativejson.EncodeLosslessNativeJSON(v, ownerSourceLimit)
	if err != nil {
		return err
	}
	w.sequence++
	return nativejson.WritePrivateFrame(w.conn, w.sequence, raw, ownerSourceLimit)
}
func (w *ownerSourceWire) read(v any) error {
	w.sequence++
	raw, err := nativejson.ReadPrivateFrame(w.conn, w.sequence, ownerSourceLimit)
	if err != nil {
		return err
	}
	return nativejson.DecodeLosslessNativeJSON(raw, v, ownerSourceLimit)
}
func (w *ownerSourceWire) call(m ownerSourceMessage) (ownerSourceMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.send(m); err != nil {
		return ownerSourceMessage{}, err
	}
	var reply ownerSourceMessage
	if err := w.read(&reply); err != nil {
		return reply, err
	}
	if reply.Kind != "ack" || reply.Error {
		return reply, errors.New("native source acknowledgement unavailable")
	}
	return reply, nil
}
func ownerSourceEnvironment(env []string) []string {
	result := slices.Clone(env)
	return slices.DeleteFunc(result, func(s string) bool {
		return len(s) > len(ownerBrowserMarker) && s[:len(ownerBrowserMarker)+1] == ownerBrowserMarker+"=" || len(s) > len(ownerSourceMarker) && s[:len(ownerSourceMarker)+1] == ownerSourceMarker+"="
	})
}

func ownerSourceCapability(ctx context.Context) bool {
	return ports.NativeSelfExecutableFrom(ctx) != nil || ports.NativePreparationExecutableFrom(ctx) != nil
}
func (client *Client) runOwnerSource(ctx context.Context, mode string, request ownerSourceRequest) (result ownerSourceMessage, resultErr error) {
	if !ownerSourceCapability(ctx) {
		return result, domain.ErrPlanStale
	}
	if mode != "apply" && mode != "drain" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 95*time.Second)
		defer cancel()
	} else if _, ok := ctx.Deadline(); !ok {
		return result, domain.ErrPlanStale
	}
	request.Socket = client.socket
	request.Extensions = slices.Clone(client.requiredExtensions)
	request.Invocation = ports.NativeInvocationKey(ctx)
	request.OwnerHome = ports.NativeOwnerHome(ctx)
	if request.Directory == "" {
		request.Directory = string(request.Policy.Directory)
	}
	if request.Directory == "" {
		request.Directory = ports.NativeSourceDirectory(ctx)
	}
	if request.Directory == "" {
		var err error
		request.Directory, err = os.Getwd()
		if err != nil {
			return result, err
		}
	}
	if !filepath.IsAbs(request.Directory) {
		return result, domain.ErrPlanStale
	}
	request.Environment = ownerSourceEnvironment(request.Environment)
	deadline, _ := ctx.Deadline()
	request.Deadline = deadline.UnixNano()
	raw, err := nativejson.EncodeLosslessNativeJSON(request, ownerSourceLimit)
	if err != nil {
		return result, err
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return result, err
	}
	parent := os.NewFile(uintptr(pair[0]), "owner-source-parent")
	child := os.NewFile(uintptr(pair[1]), "owner-source-child")
	defer parent.Close()
	defer child.Close()
	conn, err := net.FileConn(parent)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	wire := &ownerSourceWire{conn: conn}
	err = shelladapter.RunNativeOwnerSource(ctx, mode, request.Environment, request.Directory, child, raw, func() error {
		if err := wire.send(request); err != nil {
			return err
		}
		for events := 0; events < 2048; events++ {
			var event ownerSourceMessage
			if err := wire.read(&event); err != nil {
				return err
			}
			if event.Kind == "result" {
				result = event
				return nil
			}
			ack := client.ownerSourceEvent(ctx, request, event)
			if err := wire.send(ack); err != nil {
				return err
			}
		}
		return errors.New("native source event bound")
	})
	if err != nil {
		if o := ports.NativeOperationObserverFrom(ctx); o != nil {
			o.Uncertain()
		}
		return result, fmt.Errorf("native source foreground custody unavailable: %w", err)
	}
	if result.Kind != "result" {
		return result, errors.New("native source result unavailable")
	}
	if result.Stale {
		return result, domain.ErrPlanStale
	}
	if result.Failure != nil {
		return result, &ports.NativeOperationFailure{Operation: *result.Failure, Cause: errors.New("native source SDK operation failed")}
	}
	if result.Error {
		return result, errors.New("native source operation failed")
	}
	return result, nil
}
func (client *Client) ownerSourceEvent(ctx context.Context, request ownerSourceRequest, m ownerSourceMessage) ownerSourceMessage {
	ack := ownerSourceMessage{Kind: "ack"}
	o := ports.NativeOperationObserverFrom(ctx)
	var err error
	switch m.Kind {
	case "before":
		if o == nil {
			err = domain.ErrPlanStale
		} else {
			err = o.Before(m.Operation)
		}
	case "accepted":
		if o == nil {
			err = domain.ErrPlanStale
		} else {
			err = o.Accepted(m.Operation)
		}
	case "terminal":
		if o == nil {
			err = domain.ErrPlanStale
		} else {
			err = o.Terminal(m.Operation)
		}
	case "uncertain":
		if o == nil {
			err = domain.ErrPlanStale
		} else {
			o.Uncertain()
		}
	case "create-before":
		v, ok := o.(ports.NativeOwnerCreateObserver)
		if !ok {
			err = domain.ErrPlanStale
		} else {
			ack.Ref, err = v.BeforeOwnerCreate(m.Intent)
		}
	case "create-result":
		v, ok := o.(ports.NativeOwnerCreateObserver)
		if !ok || m.Created == nil {
			err = domain.ErrPlanStale
		} else {
			err = v.RecordOwnerCreated(*m.Created)
		}
	case "create-read":
		v, ok := o.(ports.NativeOwnerCreateObserver)
		if !ok {
			err = domain.ErrPlanStale
		} else {
			var value ports.NativeOwnerCreated
			value, err = v.ReadOwnerCreated(m.Ref)
			ack.Created = &value
		}
	case "cache-save":
		err = client.acceptOwnerImageCache(ctx, request.Policy, m.Cache)
		if err != nil && o != nil {
			o.Uncertain()
		}
	case "cache-read":
		if !ownerImageCacheAllowedKey(request.Policy, m.Key) {
			ack.Error = true
			return ack
		}
		v := ownerImageCacheObserver(ctx)
		if v != nil {
			ack.Caches, err = v.ReadOwnerImageCache(m.Key)
		}
	case "browser":
		err = launchOwnerSourceBrowser(request.Environment, request.Directory, m.URI)
	default:
		err = domain.ErrPlanStale
	}
	ack.Error = err != nil
	return ack
}

type ownerSourceObserver struct{ wire *ownerSourceWire }

func (o *ownerSourceObserver) Before(v ports.NativeOperation) error {
	_, err := o.wire.call(ownerSourceMessage{Kind: "before", Operation: v})
	return err
}
func (o *ownerSourceObserver) Accepted(v ports.NativeOperation) error {
	_, err := o.wire.call(ownerSourceMessage{Kind: "accepted", Operation: v})
	return err
}
func (o *ownerSourceObserver) Terminal(v ports.NativeOperation) error {
	_, err := o.wire.call(ownerSourceMessage{Kind: "terminal", Operation: v})
	return err
}
func (o *ownerSourceObserver) Uncertain() { _, _ = o.wire.call(ownerSourceMessage{Kind: "uncertain"}) }
func (o *ownerSourceObserver) CommandStarted(string, string) (uint64, error) {
	return 0, errors.New("source leaf has no nested command admission")
}
func (o *ownerSourceObserver) CommandTerminal(uint64) error {
	return errors.New("source leaf has no nested command admission")
}
func (o *ownerSourceObserver) BeforeOwnerCreate(v ports.NativeOwnerCreateIntent) (ports.NativeOwnerCreateRef, error) {
	r, err := o.wire.call(ownerSourceMessage{Kind: "create-before", Intent: v})
	return r.Ref, err
}
func (o *ownerSourceObserver) RecordOwnerCreated(v ports.NativeOwnerCreated) error {
	_, err := o.wire.call(ownerSourceMessage{Kind: "create-result", Created: &v})
	return err
}
func (o *ownerSourceObserver) ReadOwnerCreated(v ports.NativeOwnerCreateRef) (ports.NativeOwnerCreated, error) {
	r, err := o.wire.call(ownerSourceMessage{Kind: "create-read", Ref: v})
	if err != nil || r.Created == nil {
		return ports.NativeOwnerCreated{}, errors.New("source created fact unavailable")
	}
	return *r.Created, nil
}
func (o *ownerSourceObserver) RecordOwnerImageCache(v ports.OwnerImageCacheSuccessor) error {
	_, err := o.wire.call(ownerSourceMessage{Kind: "cache-save", Cache: v})
	return err
}
func (o *ownerSourceObserver) ReadOwnerImageCache(key string) ([]ports.OwnerImageCacheSuccessor, error) {
	r, err := o.wire.call(ownerSourceMessage{Kind: "cache-read", Key: key})
	return r.Caches, err
}

type ownerSourceLeafKey struct{}
type ownerSourceLeaf struct {
	wire         *ownerSourceWire
	preparation  bool
	caches       []ports.OwnerImageCacheSuccessor
	browser      string
	browserStops []func() error
	request      ownerSourceRequest
	oci          *api.InstanceSource
	helperFiles  []*os.File
}

func sourceLeaf(ctx context.Context) *ownerSourceLeaf {
	v, _ := ctx.Value(ownerSourceLeafKey{}).(*ownerSourceLeaf)
	return v
}

// RunOwnerSourceLeaf is a fixed private entry; its request arrives only on the
// inherited peer, never argv/env. SDK auth/helper limitations stay inside this
// bounded foreground process and are not described as SDK cancellation fixes.
func RunOwnerSourceLeaf(ctx context.Context, mode string) error {
	if syscall.Getpgrp() != os.Getpid() {
		return domain.ErrPlanStale
	}
	if mode != "capture" && mode != "check" && mode != "catalog" && mode != "apply" && mode != "drain" {
		return domain.ErrPlanStale
	}
	file := os.NewFile(3, "owner-source-peer")
	defer file.Close()
	unix.CloseOnExec(3)
	cred, err := unix.GetsockoptUcred(3, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || int(cred.Pid) != os.Getppid() || cred.Uid != uint32(os.Getuid()) {
		return domain.ErrPlanStale
	}
	conn, err := net.FileConn(file)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	wire := &ownerSourceWire{conn: conn}
	var request ownerSourceRequest
	if err := wire.read(&request); err != nil {
		return err
	}
	deadline := time.Unix(0, request.Deadline)
	if time.Until(deadline) <= 0 || time.Until(deadline) > 80*time.Minute {
		return domain.ErrPlanStale
	}
	_ = conn.SetDeadline(deadline)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	watchdog := time.AfterFunc(time.Until(deadline), func() { _ = syscall.Kill(-os.Getpid(), syscall.SIGKILL) })
	defer watchdog.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		if ownerSourceParentLost(done, unix.Poll) {
			_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
		}
	}()

	directory, err := os.Getwd()
	if err != nil || directory != request.Directory || !slices.Equal(ownerSourceEnvironment(os.Environ()), request.Environment) {
		return domain.ErrPlanStale
	}
	leaf := &ownerSourceLeaf{wire: wire, preparation: request.Invocation == "" && mode != "drain", request: request}
	ctx = context.WithValue(ctx, ownerSourceLeafKey{}, leaf)
	if request.Invocation != "" || mode == "drain" {
		if request.OwnerHome == "" {
			return domain.ErrPlanStale
		}
		owner := os.NewFile(4, "owner-source-original-OFD")
		defer owner.Close()
		if err := uninstall.ValidateOwnerLockDescriptor(request.OwnerHome, uint32(os.Getuid()), owner); err != nil {
			return err
		}
		// The SDK's OCI helper overrides stdin and provides no ExtraFiles hook.
		// This sole intentional inheritable OFD fences that exact descendant too.
		if _, err := unix.FcntlInt(owner.Fd(), unix.F_SETFD, 0); err != nil {
			return err
		}
		observer := &ownerSourceObserver{wire}
		if mode != "drain" {
			ctx = ports.WithNativeOperationObserver(ctx, observer)
		}
		ctx = ports.WithNativeInvocationKey(ctx, request.Invocation)
		ctx = ports.WithNativeOwnerHome(ctx, request.OwnerHome)
	} else if mode == "apply" || request.OwnerHome != "" {
		return domain.ErrPlanStale
	}
	if mode != "capture" {
		ctx = ports.WithNativeOwnerImageCache(ctx, &ownerSourceObserver{wire})
	}
	client := New(request.Socket, request.Extensions...)
	result := ownerSourceMessage{Kind: "result"}
	switch mode {
	case "capture":
		result.Policy, err = client.CaptureOwnerImagePolicy(ctx, request.Primary, request.Fallback, request.Kind, request.Environment, request.Home)
		if err == nil && leaf.preparation && client.ownerImagePreparationAuth(result.Policy, request.Environment, request.Home) {
			_, err = client.ReadOwnerPolicyImageCatalog(ctx, result.Policy, request.Environment, request.Home)
		}
	case "check":
		err = client.CheckOwnerImagePolicy(ctx, request.Policy, request.Environment, request.Home)
	case "catalog":
		result.Data, err = client.ReadOwnerPolicyImageCatalog(ctx, request.Policy, request.Environment, request.Home)
	case "apply":
		result.Created, result.Resolved, err = client.ApplyPreparedOwnerPolicyInstance(ctx, request.Instance, request.Environment, request.Home)
	case "drain":
		result.Operation, err = client.DrainOwnerImageSecretOperation(ctx, request.Policy, request.Selected, request.Environment, request.Home, request.Expected)
	}
	for _, file := range leaf.helperFiles {
		_ = file.Close()
	}
	for i := len(leaf.browserStops) - 1; i >= 0; i-- {
		err = errors.Join(err, leaf.browserStops[i]())
	}
	result.Caches = leaf.caches
	if mode == "capture" {
		result.Policy.Cache = leaf.caches
	}
	if err != nil {
		result.Error = true
		result.Stale = errors.Is(err, domain.ErrPlanStale)
		var known *ports.NativeOperationFailure
		if errors.As(err, &known) {
			result.Failure = &known.Operation
		}
	}
	if err := wire.send(result); err != nil {
		return err
	}
	return nil
}

// Interrupted polling says nothing about the parent. Only a definitive socket
// loss/error fences this foreground group; session completion stops the poll.
func ownerSourceParentLost(done <-chan struct{}, poll func([]unix.PollFd, int) (int, error)) bool {
	for {
		select {
		case <-done:
			return false
		default:
		}
		fds := []unix.PollFd{{Fd: 3, Events: unix.POLLRDHUP | unix.POLLHUP | unix.POLLERR | unix.POLLNVAL}}
		_, err := poll(fds, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return true
		}
	}
}
