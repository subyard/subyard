package domain

// AdoptedInstallation is deliberately unreadable as creation ownership by older releases.
const AdoptedInstallation = "adopted-v1"

// LegacyContinuation grants only normal reconciliation of an existing native
// identity. It is never installation creation or destructive ownership evidence.
type LegacyContinuation struct {
	Schema                     int      `json:"schema"`
	Scope                      string   `json:"scope"`
	OperatorUID                int      `json:"operatorUid"`
	HostID                     string   `json:"hostId"`
	OwnerEpoch                 uint64   `json:"ownerEpoch"`
	Context                    Context  `json:"context"`
	RuntimeRoot                string   `json:"runtimeRoot"`
	InstanceUUID               string   `json:"instanceUuid"`
	Resources                  []string `json:"resources"`
	Provenance                 string   `json:"establishmentProvenance"`
	SecurityPolicyBinding      string   `json:"securityPolicyBinding"`
	EstablishmentNativeBinding string   `json:"establishmentNativeBinding"`
}

// ContinuationContext excludes released asset paths, which legitimately change
// during update and rollback; persisted owner and native identity remain bound.
func ContinuationContext(yard Context) Context {
	yard.Paths.RepositoryRoot = ""
	yard.Paths.ConfigDir = ""
	return yard
}
