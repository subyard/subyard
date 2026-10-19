package ports

// NativeOwnerCreateRef addresses one existing operation or command reservation
// in the original owner invocation. It never identifies a resource by name.
type NativeOwnerCreateRef struct {
	Producer string
	Domain   string
	Index    uint64
}

// NativeOwnerCreateIntent binds a fixed original owner create request. The
// invocation supplies its own binding, recipe and source; callers cannot set them.
type NativeOwnerCreateIntent struct {
	Scope         string `json:"scope"`
	Kind          string `json:"kind"`
	Domain        string `json:"domain"`
	Token         string `json:"token"`
	RequestDigest string `json:"requestDigest"`
	Server        string `json:"server"`
	Project       string `json:"project"`
	Resource      string `json:"resource"`
	Pool          string `json:"pool"`
	Fingerprint   string `json:"fingerprint,omitempty"`
}

// NativeOwnerCreated is causal resource identity, not invocation quiescence.
type NativeOwnerCreated struct {
	Ref            NativeOwnerCreateRef
	Intent         NativeOwnerCreateIntent
	OperationID    string
	Status         int
	InstanceUUID   string
	IdentityDigest string
	ETagDigest     string
}

type NativeOwnerCreateObserver interface {
	BeforeOwnerCreate(NativeOwnerCreateIntent) (NativeOwnerCreateRef, error)
	RecordOwnerCreated(NativeOwnerCreated) error
	ReadOwnerCreated(NativeOwnerCreateRef) (NativeOwnerCreated, error)
}
