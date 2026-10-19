package ports

// PreparedImageProject retains one preconsented project namespace. Its only
// effect can create that exact absent project with the approved image policy.
type PreparedImageProject struct {
	Project      string
	Absent       bool
	SharedImages bool
	Config       map[string]string
}

// PreparedTemporaryStop binds restoration of an originally stopped legacy
// instance to its original UUID and unchanged desired-running intent.
type PreparedTemporaryStop struct {
	Project, Instance, UUID, Desired string
}
