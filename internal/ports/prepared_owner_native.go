package ports

// PreparedImageProject retains one preconsented project namespace. Its only
// effect can create that exact absent project with the approved image policy.
type PreparedImageProject struct {
	Project      string
	Server       string `json:",omitempty"`
	Absent       bool
	SharedImages bool
	Config       map[string]string
	// InitialConfig is the original fixed 02 renderer's NUL-framed byte output.
	// Nil retains an image-only namespace; it never adopts an unrelated project.
	InitialConfig []byte
}

// PreparedTemporaryStop binds restoration of an originally stopped legacy
// instance to its original UUID and unchanged desired-running intent.
type PreparedTemporaryStop struct {
	Project, Instance, UUID, Desired string
}
