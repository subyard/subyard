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

// NativeOwnerMember is the one registration member retained across the owning
// compensation rename. Its path is fixed by the original owner recipe.
type NativeOwnerMember struct {
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	UID             uint32 `json:"uid"`
	GID             uint32 `json:"gid"`
	Mode            uint32 `json:"mode"`
	Links           uint64 `json:"links"`
	Size            int64  `json:"size"`
	ModifiedSeconds int64  `json:"modifiedSeconds"`
	ModifiedNanos   int64  `json:"modifiedNanos"`
	ChangedSeconds  int64  `json:"changedSeconds"`
	ChangedNanos    int64  `json:"changedNanos"`
	Digest          string `json:"digest"`
}

// NativeOwnerRestorationObserver records an owning rename result, not native
// quiescence. Missing or conflicting results never confer retry authority.
type NativeOwnerRestorationObserver interface {
	RecordOwnerRestoration(token, source string, member NativeOwnerMember) error
	ReadOwnerRestoration(token, source string) (NativeOwnerMember, error)
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
