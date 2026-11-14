package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultWriteStallTimeout bounds how long one outgoing frame may wait for the
// client to read it before the session disconnects that client.
const DefaultWriteStallTimeout = 30 * time.Second

// DefaultMaxOperations bounds the requests a session runs or holds an unsent
// response for at once. It exceeds a native client's own pending-request bound.
const DefaultMaxOperations = 128

// writeProgressChunk bounds each underlying write, so the stall watchdog
// observes progress within a large frame.
const writeProgressChunk = 4096

var errWriteStall = errors.New("RPC client made no read progress within the write stall timeout")

type Call struct {
	ID          string
	OperationID string
	Method      string
	Params      json.RawMessage
}

// Emit publishes one event on the session-wide ordered stream and returns its
// monotonic revision. Adapter-local revisions remain in typed event data.
type Emit func(event string, data any) (uint64, error)

type orderedEventSequencer struct {
	mu   sync.Mutex
	next uint64
}

func (sequencer *orderedEventSequencer) publish(
	response Response, enqueue func(Response) error,
) (uint64, error) {
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	sequencer.next++
	response.Sequence = sequencer.next
	response.Revision = sequencer.next
	return sequencer.next, enqueue(response)
}

type Handler interface {
	Handle(context.Context, Call, Emit) (any, error)
}

type HandlerFunc func(context.Context, Call, Emit) (any, error)

func (function HandlerFunc) Handle(ctx context.Context, call Call, emit Emit) (any, error) {
	return function(ctx, call, emit)
}

type Session struct {
	Handler       Handler
	Capabilities  []string
	EngineVersion string
	// Buffer bounds queued outgoing frames. Responses wait for room; events that
	// do not fit are coalesced so the client observes a sequence gap.
	Buffer int
	// WriteStallTimeout ends a session whose client reads no outgoing bytes for
	// this duration. Zero selects DefaultWriteStallTimeout.
	WriteStallTimeout time.Duration
	// MaxOperations bounds concurrently admitted requests, counted until their
	// response is queued. Further requests are refused with operation_capacity.
	// Zero selects DefaultMaxOperations.
	MaxOperations int
	// DrainOnEOF treats input EOF as a stdio half-close: already accepted calls
	// finish and their responses are flushed before output is closed. Socket-like
	// sessions keep the default false behavior, where EOF cancels active work.
	DrainOnEOF bool
}

func (session Session) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if session.Handler == nil {
		return errors.New("RPC handler is required")
	}
	ctx, stop := context.WithCancel(ctx)
	var shutdownOnce sync.Once
	closeStreams := func() {
		shutdownOnce.Do(func() {
			if closer, ok := input.(io.Closer); ok {
				_ = closer.Close()
			}
			if closer, ok := output.(io.Closer); ok {
				_ = closer.Close()
			}
		})
	}
	stopClosing := context.AfterFunc(ctx, closeStreams)
	defer func() {
		stop()
		_ = stopClosing()
		closeStreams()
	}()
	stallTimeout := session.WriteStallTimeout
	if stallTimeout <= 0 {
		stallTimeout = DefaultWriteStallTimeout
	}
	// The watchdog runs only while the writer has a frame in flight; every chunk
	// the client accepts restarts it.
	var stalled atomic.Bool
	watchdog := time.AfterFunc(stallTimeout, func() {
		stalled.Store(true)
		stop()
	})
	watchdog.Stop()
	defer watchdog.Stop()
	codec := NewCodec(input, progressWriter{writer: output, progress: func() {
		watchdog.Reset(stallTimeout)
	}})
	maxOperations := session.MaxOperations
	if maxOperations <= 0 {
		maxOperations = DefaultMaxOperations
	}
	buffer := session.Buffer
	if buffer <= 0 {
		buffer = 64
	}
	outgoing := newOutbox(buffer)
	stopShutdown := context.AfterFunc(ctx, outgoing.shutdown)
	defer stopShutdown()
	writerDone := make(chan error, 1)
	go func() {
		for {
			response, ok := outgoing.pop()
			if !ok {
				writerDone <- nil
				return
			}
			watchdog.Reset(stallTimeout)
			err := codec.Write(response)
			watchdog.Stop()
			if stalled.Load() {
				err = errWriteStall
			}
			if err != nil {
				writerDone <- err
				stop()
				return
			}
		}
	}()

	var negotiated atomic.Bool
	var eventSequence orderedEventSequencer
	var workers sync.WaitGroup
	operations := make(map[string]context.CancelCauseFunc)
	admitted := 0
	var operationsMu sync.Mutex
	enqueue := func(response Response) error {
		if !outgoing.pushResponse(response) {
			return context.Cause(ctx)
		}
		return nil
	}
	enqueueEvent := func(response Response) error {
		if !outgoing.pushEvent(response) {
			return context.Cause(ctx)
		}
		return nil
	}

	readErr := error(nil)
	for ctx.Err() == nil {
		request, err := codec.ReadRequest()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		if err := validateRequest(request); err != nil {
			_ = enqueue(Response{Version: ProtocolVersion, Type: "response", ID: request.ID,
				OperationID: request.OperationID, Error: asFault(err)})
			continue
		}
		if request.Type == "cancel" {
			operationsMu.Lock()
			cancel := operations[request.OperationID]
			operationsMu.Unlock()
			if cancel == nil {
				_ = enqueue(Response{Version: ProtocolVersion, Type: "response", ID: request.ID,
					OperationID: request.OperationID,
					Error:       &Error{Code: "operation_not_found", Message: "operation is not active"}})
			} else {
				cancel(contextCanceled)
				_ = enqueue(Response{Version: ProtocolVersion, Type: "response", ID: request.ID,
					OperationID: request.OperationID,
					Result:      map[string]any{"cancelled": request.OperationID}})
			}
			continue
		}
		if request.Method == "rpc.negotiate" {
			negotiated.Store(true)
			_ = enqueue(Response{Version: ProtocolVersion, Type: "response", ID: request.ID, Result: map[string]any{
				"version": ProtocolVersion, "protocolMin": ProtocolVersion, "protocolMax": ProtocolVersion,
				"engineVersion": session.EngineVersion, "capabilities": session.Capabilities,
			}})
			continue
		}
		if !negotiated.Load() {
			_ = enqueue(Response{Version: ProtocolVersion, Type: "response", ID: request.ID,
				OperationID: request.OperationID,
				Error:       &Error{Code: "negotiation_required", Message: "rpc.negotiate must be called first"}})
			continue
		}
		operationID := request.OperationID
		if operationID == "" {
			operationID = request.ID
		}
		operationParent := ctx
		deadlineCancel := func() {}
		if request.Deadline != nil {
			operationParent, deadlineCancel = context.WithDeadline(ctx, *request.Deadline)
		}
		operationContext, cancel := context.WithCancelCause(operationParent)
		operationsMu.Lock()
		if admitted >= maxOperations {
			operationsMu.Unlock()
			cancel(nil)
			deadlineCancel()
			_ = enqueue(Response{Version: ProtocolVersion, Type: "response", ID: request.ID,
				OperationID: operationID,
				Error:       &Error{Code: "operation_capacity", Message: "too many active operations; retry after one finishes"}})
			continue
		}
		if _, duplicate := operations[operationID]; duplicate {
			operationsMu.Unlock()
			cancel(nil)
			deadlineCancel()
			_ = enqueue(Response{Version: ProtocolVersion, Type: "response", ID: request.ID,
				OperationID: operationID,
				Error:       &Error{Code: "duplicate_operation", Message: "operation ID is already active"}})
			continue
		}
		operations[operationID] = cancel
		admitted++
		operationsMu.Unlock()
		workers.Add(1)
		go func(request Request, operationID string) {
			defer workers.Done()
			// The slot covers the unsent response too, bounding held results.
			defer func() {
				operationsMu.Lock()
				admitted--
				operationsMu.Unlock()
			}()
			var finishOnce sync.Once
			finish := func() {
				finishOnce.Do(func() {
					operationsMu.Lock()
					delete(operations, operationID)
					operationsMu.Unlock()
					cancel(nil)
					deadlineCancel()
				})
			}
			defer finish()
			emit := func(event string, data any) (uint64, error) {
				encoded, err := json.Marshal(data)
				if err != nil {
					return 0, fmt.Errorf("encode RPC event: %w", err)
				}
				return eventSequence.publish(Response{Version: ProtocolVersion, Type: "event", ID: request.ID,
					OperationID: operationID,
					Event:       event, Data: encoded}, enqueueEvent)
			}
			result, err := session.Handler.Handle(operationContext, Call{
				ID: request.ID, OperationID: operationID, Method: request.Method, Params: request.Params,
			}, emit)
			response := Response{Version: ProtocolVersion, Type: "response", ID: request.ID,
				OperationID: operationID, Result: result}
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(context.Cause(operationContext), contextCanceled) {
					err = contextCanceled
				}
				response.Result = nil
				response.Error = asFault(err)
			}
			// A response makes the operation observably complete. Release its ID before enqueueing
			// that response so a client can immediately use the same ID for the next protocol phase.
			finish()
			_ = enqueue(response)
		}(request, operationID)
	}
	if readErr != nil || !session.DrainOnEOF {
		stop()
	}
	if ctx.Err() != nil {
		operationsMu.Lock()
		for _, cancel := range operations {
			cancel(context.Canceled)
		}
		operationsMu.Unlock()
	}
	workers.Wait()
	outgoing.close()
	writerErr := <-writerDone
	if readErr != nil && !errors.Is(writerErr, errWriteStall) {
		return readErr
	}
	return writerErr
}

// outbox is the bounded, ordered queue between session producers and the single
// writer. Responses are never dropped: they wait for room, and the writer's stall
// watchdog bounds that wait for a client that stops reading. Events never block
// and never fail the session: when the queue is full, the newest event replaces an
// earlier unsent overflow event. Sequence numbers are assigned before enqueueing,
// so a coalesced event leaves an observable gap, and the retained newest event is
// delivered before any later frame so the gap is visible even at the tail.
type outbox struct {
	mu       sync.Mutex
	ready    *sync.Cond
	frames   []Response
	limit    int
	overflow *Response
	waiting  int
	closed   bool
	done     bool
}

func newOutbox(limit int) *outbox {
	queue := &outbox{limit: limit, frames: make([]Response, 0, limit+1)}
	queue.ready = sync.NewCond(&queue.mu)
	return queue
}

func (queue *outbox) pushEvent(response Response) bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.done || queue.closed {
		return false
	}
	if queue.overflow != nil && queue.hasRoomForEventLocked() {
		queue.frames = append(queue.frames, *queue.overflow)
		queue.overflow = nil
	}
	if queue.overflow == nil && queue.hasRoomForEventLocked() {
		queue.frames = append(queue.frames, response)
	} else {
		queue.overflow = &response
	}
	queue.ready.Broadcast()
	return true
}

// Waiting responses take precedence over new events, so an event burst cannot
// starve a response of queue room.
func (queue *outbox) hasRoomForEventLocked() bool {
	return queue.waiting == 0 && len(queue.frames) < queue.limit
}

func (queue *outbox) pushResponse(response Response) bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.waiting++
	for !queue.done && !queue.closed && len(queue.frames) >= queue.limit {
		queue.ready.Wait()
	}
	queue.waiting--
	if queue.done || queue.closed {
		return false
	}
	if queue.overflow != nil {
		queue.frames = append(queue.frames, *queue.overflow)
		queue.overflow = nil
	}
	queue.frames = append(queue.frames, response)
	queue.ready.Broadcast()
	return true
}

func (queue *outbox) pop() (Response, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for !queue.done && len(queue.frames) == 0 && queue.overflow == nil && !queue.closed {
		queue.ready.Wait()
	}
	if queue.done {
		return Response{}, false
	}
	var response Response
	switch {
	case len(queue.frames) > 0:
		response = queue.frames[0]
		queue.frames[0] = Response{}
		queue.frames = queue.frames[1:]
	case queue.overflow != nil:
		response = *queue.overflow
		queue.overflow = nil
	default:
		return Response{}, false
	}
	queue.ready.Broadcast()
	return response, true
}

func (queue *outbox) shutdown() {
	queue.mu.Lock()
	queue.done = true
	queue.ready.Broadcast()
	queue.mu.Unlock()
}

func (queue *outbox) close() {
	queue.mu.Lock()
	queue.closed = true
	queue.ready.Broadcast()
	queue.mu.Unlock()
}

// progressWriter splits writes into bounded chunks and reports each chunk that
// made progress, so a slow but steadily reading client is not treated as stalled.
type progressWriter struct {
	writer   io.Writer
	progress func()
}

func (writer progressWriter) Write(value []byte) (int, error) {
	written := 0
	for written < len(value) {
		end := min(len(value), written+writeProgressChunk)
		count, err := writer.writer.Write(value[written:end])
		written += count
		if count > 0 {
			writer.progress()
		}
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
