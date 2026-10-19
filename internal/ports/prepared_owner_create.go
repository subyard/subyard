package ports

// OwnerImageSourceGuard is an original client configuration or public trust
// source. Bytes retain admitted host path labels without UTF-8 conversion.
type OwnerImageSourceGuard struct {
	Path  []byte
	State string
}

// PreparedOwnerImageSource contains only the selected image's public transport
// description. Client keys, credentials and remote secret tokens are excluded.
type PreparedOwnerImageSource struct {
	Version             int
	Reference           []byte
	Kind                string
	Local               bool
	Protocol            string
	Server, Certificate []byte
	Fingerprint         string
	Environment         string
	Alias, Project      []byte
	Guards              []OwnerImageSourceGuard
}

// PreparedOwnerInstance retains the fixed original instance create request.
// Rendered is the existing 03 renderer's NUL-framed type/config output.
type PreparedOwnerInstance struct {
	Project, Instance string
	Runtime, Source   string
	Server            string
	Scope             string
	Rendered          []byte
	Image             PreparedOwnerImageSource
	Policy            *PreparedOwnerImagePolicy `json:",omitempty"`
}

// PreparedOwnerVolume contains only the original fixed SRV volume role.
type PreparedOwnerVolume struct {
	Project, Instance, Pool, Volume, Scope, Runtime, Source string
	InstanceUUID                                            string
	Server                                                  string
	Absent                                                  bool
	Identity, ETag                                          string
	Rendered                                                []byte
}
