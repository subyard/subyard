// Package clientprojects defines the controller-side input to desktop integrations.
package clientprojects

import "context"

// Export contains authoritative project paths for exactly one resolved yard.
type Export struct {
	HostID   string
	Yard     string
	SSHHost  string
	Projects []Project
}

type Project struct {
	Name string
	Path string
}

// Plan captures a read-only client assessment. Apply must reject baseline drift.
type Plan struct {
	Target  string
	Changed bool
	Added   int
	Apply   func(context.Context) error
	Open    func(context.Context) error
}
