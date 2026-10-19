package ports

// PreparedOwnerImagePolicy retains the original component's two references and
// source inputs without resolving either image. Runtime and renderer authority
// remain in the owning instance capsule. This is distinct from a concrete
// PreparedOwnerImageSource, whose version-1 meaning is unchanged.
type PreparedOwnerImagePolicy struct {
	Version           int
	Primary, Fallback []byte
	Kind              string
	Environment       string
	Project           []byte
	Guards            []OwnerImageSourceGuard
}
