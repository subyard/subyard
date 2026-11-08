package profile

// UnmanagedIntegration is a profile-owned, read-only guest observation. It never
// selects an integration or participates in activation readiness.
type UnmanagedIntegration struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
	Detail  string   `json:"detail"`
}
