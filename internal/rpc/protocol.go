package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	MaxFrameSize = 1024 * 1024
)

type Request struct {
	Version     int             `json:"version"`
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	Method      string          `json:"method,omitempty"`
	OperationID string          `json:"operationId,omitempty"`
	Deadline    *time.Time      `json:"deadline,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	Version     int             `json:"version"`
	Type        string          `json:"type"`
	ID          string          `json:"id,omitempty"`
	OperationID string          `json:"operationId,omitempty"`
	Sequence    uint64          `json:"sequence,omitempty"`
	Revision    uint64          `json:"revision,omitempty"`
	Event       string          `json:"event,omitempty"`
	Result      any             `json:"result,omitempty"`
	Error       *Error          `json:"error,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	// Data is an encoded ErrorData; relays forward it verbatim.
	Data    json.RawMessage `json:"data,omitempty"`
	relayed bool            // Set only when decoding an owner response; never serialized.
}

func (fault *Error) Error() string { return string(fault.Code) + ": " + fault.Message }

type Codec struct {
	reader *bufio.Reader
	writer io.Writer
}

func NewCodec(reader io.Reader, writer io.Writer) *Codec {
	return &Codec{reader: bufio.NewReader(reader), writer: writer}
}

func (codec *Codec) ReadRequest() (Request, error) {
	payload, err := codec.readPayload()
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err := json.Unmarshal(payload, &request); err != nil {
		return Request{}, fmt.Errorf("decode RPC request: %w", err)
	}
	return request, nil
}

func (codec *Codec) ReadResponse() (Response, error) {
	payload, err := codec.readPayload()
	if err != nil {
		return Response{}, err
	}
	var response Response
	if err := json.Unmarshal(payload, &response); err != nil {
		return Response{}, fmt.Errorf("decode RPC response: %w", err)
	}
	if response.Error != nil {
		response.Error.relayed = true
	}
	return response, nil
}

func (codec *Codec) Write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode RPC frame: %w", err)
	}
	if len(payload) == 0 || len(payload) > MaxFrameSize {
		return fmt.Errorf("RPC frame size %d is outside the allowed range", len(payload))
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if err := writeAll(codec.writer, header); err != nil {
		return fmt.Errorf("write RPC frame header: %w", err)
	}
	if err := writeAll(codec.writer, payload); err != nil {
		return fmt.Errorf("write RPC frame body: %w", err)
	}
	return nil
}

func (codec *Codec) readPayload() ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(codec.reader, header); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header)
	if size == 0 || size > MaxFrameSize {
		return nil, fmt.Errorf("RPC frame size %d is outside the allowed range", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(codec.reader, payload); err != nil {
		return nil, fmt.Errorf("read RPC frame body: %w", err)
	}
	return payload, nil
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) != 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

func validateRequest(request Request) error {
	if request.Version != ProtocolVersion {
		return &Error{Code: CodeIncompatibleVersion, Message: fmt.Sprintf("protocol version %d is not supported", request.Version)}
	}
	if !validRPCIdentity(request.ID) {
		return &Error{Code: CodeInvalidRequest, Message: "request ID is invalid"}
	}
	if request.OperationID != "" && !validRPCIdentity(request.OperationID) {
		return &Error{Code: CodeInvalidRequest, Message: "operation ID is invalid"}
	}
	if len(request.Params) != 0 && !json.Valid(request.Params) {
		return &Error{Code: CodeInvalidRequest, Message: "params must be valid JSON"}
	}
	switch request.Type {
	case "request":
		if request.Method == "" {
			return &Error{Code: CodeInvalidRequest, Message: "method is required"}
		}
	case "cancel":
		if request.OperationID == "" {
			return &Error{Code: CodeInvalidRequest, Message: "operation ID is required"}
		}
		if request.Deadline != nil {
			return &Error{Code: CodeInvalidRequest, Message: "cancel frame must not carry a deadline"}
		}
	default:
		return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("unknown frame type %q", request.Type)}
	}
	if containsSensitiveJSON(request.Params) {
		return &Error{Code: CodeSecretForbidden, Message: "secret-bearing fields are not accepted by RPC"}
	}
	return nil
}

func validRPCIdentity(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func containsSensitiveJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	return containsSensitive(value)
}

func containsSensitive(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := normalizeKey(key)
			if normalized == "secret" || normalized == "password" || normalized == "token" ||
				normalized == "payload" || normalized == "privatekey" ||
				strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") ||
				strings.Contains(normalized, "token") || strings.Contains(normalized, "privatekey") {
				return true
			}
			if containsSensitive(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsSensitive(child) {
				return true
			}
		}
	}
	return false
}

func normalizeKey(value string) string {
	result := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char == '_' || char == '-' {
			continue
		}
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}
		result = append(result, char)
	}
	return string(result)
}

// FaultOf is the error a session sends for a handler error of a request
// whose context is ctx.
func FaultOf(ctx context.Context, err error) *Error {
	if errors.Is(err, context.Canceled) || errors.Is(context.Cause(ctx), contextCanceled) {
		err = contextCanceled
	}
	return asFault(err)
}

func asFault(err error) *Error {
	var fault *Error
	if errors.As(err, &fault) {
		return fault
	}
	if errors.Is(err, contextCanceled) || errors.Is(err, context.Canceled) {
		return &Error{Code: CodeCancelled, Message: "operation cancelled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeDeadlineExceeded, Message: "operation deadline exceeded"}
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

var contextCanceled = errors.New("operation cancelled")
