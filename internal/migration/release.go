package migration

import "io"

// ReleaseOptions carries the release context shared by CLI admission and migrations.
type ReleaseOptions struct {
	RegistryPath   string
	RepositoryRoot string
	RuntimeRoot    string
	ConfigHome     string
	DataHome       string
	Version        string
	Executable     string
	Incus          string
	Environment    []string
	Diagnostics    io.Writer
	Stderr         io.Writer
}
