package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

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
	Buffer        int
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
	codec := NewCodec(input, output)
	buffer := session.Buffer
	if buffer <= 0 {
		buffer = 64
	}
	outgoing := make(chan Response, buffer)
	writerDone := make(chan error, 1)
	go func() {
		for response := range outgoing {
			if err := codec.Write(response); err != nil {
				writerDone <- err
				stop()
				return
			}
		}
		writerDone <- nil
	}()

	var negotiated atomic.Bool
	var eventSequence orderedEventSequencer
	var workers sync.WaitGroup
	operations := make(map[string]context.CancelCauseFunc)
	var operationsMu sync.Mutex
	enqueue := func(response Response) error {
		select {
		case outgoing <- response:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
			stop()
			return errors.New("RPC client exceeded bounded backpressure buffer")
		}
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
		operationsMu.Unlock()
		workers.Add(1)
		go func(request Request, operationID string) {
			defer workers.Done()
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
					Event:       event, Data: encoded}, enqueue)
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
	close(outgoing)
	writerErr := <-writerDone
	if readErr != nil {
		return readErr
	}
	return writerErr
}
