// Package ownerapi projects read-only owner facts for typed callers. The caller
// supplies a validated local owner context and retains routing/context binding.
package ownerapi

import (
	"context"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

// Error is a safe query failure; transport boundaries choose its wire representation.
type Error struct{ Code, Message string }

func (err *Error) Error() string { return err.Message }

// ProfileCheck retains the existing adapter's command context and read-only runner.
type ProfileCheck struct {
	Runner  ports.AdapterRunner
	Context map[string]string
}

// Service uses existing owner readers. Lazy bridges preserve the CLI's shared
// catalog/adapter and registered Git authority without exposing transport state.
type Service struct {
	Loaded            config.Loaded
	RepositoryRoot    string
	Environment       map[string]string
	Instances         ports.Incus
	Resources         resource.Registry
	ProjectsAllowed   bool
	OperationID       string
	ProvisionProfiles func() ([]string, error)
	ProfileCheck      func() ProfileCheck
	InspectGit        func(context.Context, configsync.SourceRecord) GitState
	Credentials       ports.CredentialMetadataReader
}

// GitState contains inspected registration facts before the public remote redaction.
type GitState struct {
	Branch, Upstream, Head, Relation, Worktree, Remote    string
	Staged, Unstaged, Untracked, Conflicts, Ahead, Behind int
	LastFetch                                             string
	Available                                             bool
}
