package ownerapi

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/credential"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type SyncGit struct {
	Branch    string `json:"branch"`
	Upstream  string `json:"upstream"`
	Head      string `json:"head"`
	Relation  string `json:"relation"`
	Worktree  string `json:"worktree"`
	Remote    string `json:"remote"`
	Staged    int    `json:"staged"`
	Unstaged  int    `json:"unstaged"`
	Untracked int    `json:"untracked"`
	Conflicts int    `json:"conflicts"`
	Ahead     int    `json:"ahead"`
	Behind    int    `json:"behind"`
	LastFetch string `json:"lastFetch"`
	Available bool   `json:"available"`
}

type SyncCredentials struct {
	State     string                        `json:"state"`
	Records   int                           `json:"records"`
	Conflicts int                           `json:"conflicts"`
	Peers     []domain.CredentialPeerStatus `json:"peers"`
}

type HostSyncStatus struct {
	SchemaVersion    int             `json:"schemaVersion"`
	HostID           string          `json:"hostId"`
	HostIDPending    bool            `json:"hostIdPending"`
	Automation       string          `json:"automation"`
	Offline          bool            `json:"offline"`
	Registration     string          `json:"registration"`
	RecoveryRequired bool            `json:"recoveryRequired"`
	Generation       uint64          `json:"generation"`
	AppliedCommit    string          `json:"appliedCommit"`
	Git              *SyncGit        `json:"git,omitempty"`
	Credentials      SyncCredentials `json:"credentials"`
}

func (service Service) HostSyncStatus(ctx context.Context) (HostSyncStatus, error) {
	result := HostSyncStatus{SchemaVersion: 1, Automation: "manual", Offline: true, Registration: "not-configured",
		Credentials: SyncCredentials{State: "unavailable", Peers: []domain.CredentialPeerStatus{}}}
	status, err := configsync.ReadStatus(service.Loaded.Context.Paths.ConfigHome, service.Environment)
	if err != nil {
		return result, &Error{Code: "host_sync_status_failed", Message: "configuration sync metadata is unavailable or invalid"}
	}
	result.HostID, result.HostIDPending = status.HostID, status.HostIDPending
	result.RecoveryRequired, result.Generation, result.AppliedCommit = status.RecoveryRequired, status.Generation, status.SourceCommit
	record, exists, err := configsync.ReadSourceRecord(service.Loaded.Context.Paths.ConfigHome)
	if err != nil {
		result.Registration = "broken"
	} else if exists {
		result.Registration = "configured"
		state := service.InspectGit(ctx, record)
		result.Git = &SyncGit{Branch: state.Branch, Upstream: state.Upstream, Head: state.Head,
			Relation: state.Relation, Worktree: state.Worktree, Remote: publicConfigRemote(state.Remote), Staged: state.Staged,
			Unstaged: state.Unstaged, Untracked: state.Untracked, Conflicts: state.Conflicts, Ahead: state.Ahead,
			Behind: state.Behind, LastFetch: state.LastFetch, Available: state.Available}
	}
	credentials, err := service.credentialStatus(ctx)
	if err == nil {
		result.Credentials.State, result.Credentials.Records = "available", len(credentials.Credentials)
		result.Credentials.Peers = append([]domain.CredentialPeerStatus{}, credentials.Peers...)
		for _, credential := range credentials.Credentials {
			if credential.Conflict || credential.NeedsMerge {
				result.Credentials.Conflicts++
			}
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func publicConfigRemote(value string) string {
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "file:") {
		return "<local-source>"
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" {
			return "<redacted-remote>"
		}
		switch parsed.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return "<redacted-remote>"
		}
		parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
		parsed.ForceQuery = false
		return parsed.String()
	}
	colon, slash := strings.IndexByte(value, ':'), strings.IndexByte(value, '/')
	if colon > 0 && (slash < 0 || slash > colon) && !strings.ContainsAny(value[:colon], `\ `) {
		host, path, _ := strings.Cut(value, ":")
		if strings.HasPrefix(host, "@") {
			return "<redacted-remote>"
		}
		if index := strings.LastIndexByte(host, '@'); index >= 0 {
			host = host[index+1:]
		}
		if host == "" || len(host) > 253 || strings.IndexFunc(host, func(char rune) bool {
			return !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '.' || char == '_')
		}) >= 0 || strings.ContainsAny(path, "@\x00\r\n") {
			return "<redacted-remote>"
		}
		path, _, _ = strings.Cut(path, "?")
		path, _, _ = strings.Cut(path, "#")
		return host + ":" + path
	}
	return "<redacted-remote>"
}

func (service Service) credentialStatus(ctx context.Context) (domain.CredentialStatus, error) {
	if reader, ok := service.Credentials.(ports.CredentialStatusReader); ok {
		return reader.ReadCredentialStatus(ctx)
	}
	metadata, err := service.Credentials.ListMetadata(ctx)
	if err != nil {
		return domain.CredentialStatus{}, err
	}
	summaries, err := credential.Summarize(metadata)
	if err != nil {
		return domain.CredentialStatus{}, err
	}
	return domain.CredentialStatus{Credentials: summaries, Peers: []domain.CredentialPeerStatus{}}, nil
}
