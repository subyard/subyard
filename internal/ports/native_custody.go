package ports

import "context"

// NativeResourceGeneration names a fixed owning component resource role. The
// owning component binds the actual resource path; callers cannot supply it.
type NativeResourceGeneration struct {
	Role  string `json:"role"`
	Value string `json:"value"`
}

// NativeOperation identifies an accepted official SDK operation. It contains no
// command or configuration payload. A final status is a remote fact, not merely
// a successful wait call or desired-state observation.
type NativeOperation struct {
	Kind     string `json:"kind"`
	Server   string `json:"server"`
	Project  string `json:"project"`
	Resource string `json:"resource"`
	ID       string `json:"id"`
	Status   int    `json:"status"`
}
type NativeOperationObserver interface {
	// Before durably records the exact resource before its native POST. Missing
	// acceptance after this point remains UNKNOWN, including a withheld UUID.
	Before(NativeOperation) error
	Accepted(NativeOperation) error
	Terminal(NativeOperation) error
	// Uncertain closes invocation admission when an accepted request cannot be
	// bound or drained. A later successful child operation cannot hide it.
	Uncertain()
	CommandStarted(kind, source string) (uint64, error)
	CommandTerminal(uint64) error
}

// NativeOperationFailure is returned only after the exact accepted operation has
// a durably recorded final failure/cancellation. It proves that operation closed;
// an arbitrary transport or component error provides no such proof.
type NativeOperationFailure struct {
	Operation NativeOperation
	Cause     error
}

func (e *NativeOperationFailure) Error() string { return "native operation failed: " + e.Cause.Error() }
func (e *NativeOperationFailure) Unwrap() error { return e.Cause }

type nativeOperationObserverKey struct{}
type nativeInvocationKey struct{}
type nativeOwnerHomeKey struct{}

func WithNativeOwnerHome(ctx context.Context, home string) context.Context {
	return context.WithValue(ctx, nativeOwnerHomeKey{}, home)
}
func NativeOwnerHome(ctx context.Context) string {
	value, _ := ctx.Value(nativeOwnerHomeKey{}).(string)
	return value
}

func WithNativeInvocationKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, nativeInvocationKey{}, key)
}
func NativeInvocationKey(ctx context.Context) string {
	value, _ := ctx.Value(nativeInvocationKey{}).(string)
	return value
}

func WithNativeOperationObserver(ctx context.Context, observer NativeOperationObserver) context.Context {
	return context.WithValue(ctx, nativeOperationObserverKey{}, observer)
}
func NativeOperationObserverFrom(ctx context.Context) NativeOperationObserver {
	value, _ := ctx.Value(nativeOperationObserverKey{}).(NativeOperationObserver)
	return value
}

// WithoutNativeOperationObserver scopes an owning component's declared
// read-only probe. Expected drift from that probe is not an unknown mutation.
func WithoutNativeOperationObserver(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeOperationObserverKey{}, false)
}
