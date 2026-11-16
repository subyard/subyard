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
type Error struct {
	Code    string
	Message string
}

func (err *Error) Error() string { return err.Message }

// ProfileCheck retains the existing adapter's command context and read-only runner.
type ProfileCheck struct {
	Runner  ports.AdapterRunner
	Context map[string]string
}

// Service uses existing owner readers. Lazy bridges preserve the CLI's shared
// catalog/adapter and registered Git authority without exposing transport state.
type Service struct {
	Loaded          config.Loaded
	RepositoryRoot  string
	Environment     map[string]string
	Instances       ports.Incus
	Resources       resource.Registry
	ProjectsAllowed bool
	OperationID     string
	ProfileCheck    func() ProfileCheck
	InspectGit      func(context.Context, configsync.SourceRecord) GitState
	Credentials     ports.CredentialMetadataReader
}

// GitState contains inspected registration facts before the public remote redaction.
type GitState struct {
	Branch, Upstream, Head, Relation, Worktree, Remote    string
	Staged, Unstaged, Untracked, Conflicts, Ahead, Behind int
	LastFetch                                             string
	Available                                             bool
}

// Native query failures stay independent of the wire transport. The RPC
// contract test compares this registry with the manifest's owner-query group.
const (
	codeHostSyncStatusFailed      = "host_sync_status_failed"
	codeInvalidParams             = "invalid_params"
	codeProfileCatalogInvalid     = "profile_catalog_invalid"
	codeSessionHostKeyUnavailable = "session_host_key_unavailable"
	codeSessionProjectForbidden   = "session_project_forbidden"
	codeSessionProjectUnavailable = "session_project_unavailable"
	codeSessionSshUnavailable     = "session_ssh_unavailable"
	codeSessionToolUnavailable    = "session_tool_unavailable"
	codeSessionYardUnavailable    = "session_yard_unavailable"
)

// ErrorCodes lists the safe failures produced by the owner query service.
func ErrorCodes() []string {
	return []string{codeHostSyncStatusFailed, codeInvalidParams, codeProfileCatalogInvalid, codeSessionHostKeyUnavailable, codeSessionProjectForbidden, codeSessionProjectUnavailable, codeSessionSshUnavailable, codeSessionToolUnavailable, codeSessionYardUnavailable}
}
