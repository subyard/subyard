package ports

import (
	"context"
	"errors"
	"os"
)

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
// ErrNativeOwnerRestorationAbsent identifies only an absent restoration leaf
// after the original intent, acceptance and facts directory were validated.
var ErrNativeOwnerRestorationAbsent = errors.New("original owner restoration leaf absent")

type NativeOwnerRestorationObserver interface {
	RecordOwnerRestoration(token, source string, member NativeOwnerMember) error
	ReadOwnerRestoration(token, source string) (NativeOwnerMember, error)
}

// NativeOperationFailure is returned only after the exact accepted operation has
// a durably recorded final failure/cancellation, or the owning synchronous
// config CAS has an exact success-status operation plus a nonzero command exit
// after stream drain.
// It proves that operation closed;
// an arbitrary transport or component error provides no such proof.
type NativeOperationFailure struct {
	Operation NativeOperation
	// ExecExitCode is an error-only witness from the owning config CAS SDK leaf after
	// exact remote status200 and stream drain. It is never request or wire data.
	ExecExitCode *int
	Cause        error
}

func (e *NativeOperationFailure) Error() string { return "native operation failed: " + e.Cause.Error() }
func (e *NativeOperationFailure) Unwrap() error { return e.Cause }

// WithNativeSynchronousConfigCAS scopes only the compiled synchronous Python
// configs-write call under checked original-current materialized-files custody.
// It is not request data and grants no authority to shell commands or hooks.
func WithNativeSynchronousConfigCAS(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeSynchronousConfigCASKey{}, true)
}

func NativeSynchronousConfigCASFrom(ctx context.Context) bool {
	value, _ := ctx.Value(nativeSynchronousConfigCASKey{}).(bool)
	return value
}

type nativeSynchronousConfigCASKey struct{}
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

// NativeSelfExecutable is the admitted helper's retained executable image.
// Implementations compute the digest from their own image, never request data.
type NativeSelfExecutable interface {
	Borrow() (*os.File, string, error)
}
type nativeSelfExecutableKey struct{}

func WithNativeSelfExecutable(ctx context.Context, executable NativeSelfExecutable) context.Context {
	return context.WithValue(ctx, nativeSelfExecutableKey{}, executable)
}
func NativeSelfExecutableFrom(ctx context.Context) NativeSelfExecutable {
	value, _ := ctx.Value(nativeSelfExecutableKey{}).(NativeSelfExecutable)
	return value
}

// Preparation carries an executable proof only, never mutation admission.
type nativePreparationExecutableKey struct{}

func WithNativePreparationExecutable(ctx context.Context, image NativeSelfExecutable) context.Context {
	if image == nil {
		return context.WithValue(ctx, nativePreparationExecutableKey{}, false)
	}
	return context.WithValue(ctx, nativePreparationExecutableKey{}, image)
}
func NativePreparationExecutableFrom(ctx context.Context) NativeSelfExecutable {
	v, _ := ctx.Value(nativePreparationExecutableKey{}).(NativeSelfExecutable)
	return v
}

type nativeInteractionKey struct{}

func WithNativeInteraction(ctx context.Context, file *os.File) context.Context {
	return context.WithValue(ctx, nativeInteractionKey{}, file)
}
func NativeInteractionFrom(ctx context.Context) *os.File {
	v, _ := ctx.Value(nativeInteractionKey{}).(*os.File)
	return v
}

type nativeSourceDirectoryKey struct{}

func WithNativeSourceDirectory(ctx context.Context, directory string) context.Context {
	return context.WithValue(ctx, nativeSourceDirectoryKey{}, directory)
}
func NativeSourceDirectory(ctx context.Context) string {
	v, _ := ctx.Value(nativeSourceDirectoryKey{}).(string)
	return v
}

// NativeCheckedInvocation exposes only the canonical intent already admitted by
// the original observer. It adds no transport field or ambient mutation grant.
type NativeCheckedInvocation interface {
	CheckedInvocation() (intentDigest, sourceRecipeDigest string, err error)
}

// NativeHostPowerInstallation is original producer provenance, never invocation
// closure or permission to select a later matching installation.
type NativeHostPowerConsumer struct {
	EngineDigest    string                     `json:"engineDigest"`
	UnitDigest      string                     `json:"unitDigest"`
	ContractsDigest string                     `json:"contractsDigest"`
	FilesDigest     string                     `json:"filesDigest"`
	Generations     []NativeResourceGeneration `json:"generations"`
}
type NativeHostPowerIdentity struct {
	Role   string `json:"role"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}
type NativeHostPowerInstallation struct {
	Producer   string                    `json:"producer"`
	Intent     string                    `json:"intent"`
	Recipe     string                    `json:"recipe"`
	Source     string                    `json:"source"`
	HostSource string                    `json:"hostSource"`
	Output     string                    `json:"output"`
	Command    uint64                    `json:"command"`
	Consumer   NativeHostPowerConsumer   `json:"consumer"`
	Identities []NativeHostPowerIdentity `json:"identities"`
}
type NativeHostPowerInstallationObserver interface {
	RecordHostPowerInstallation(uint64, NativeHostPowerInstallation) (NativeHostPowerInstallation, error)
	ReadHostPowerInstallation(string) (*NativeHostPowerInstallation, error)
}
