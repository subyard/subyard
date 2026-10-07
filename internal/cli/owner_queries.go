package cli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerapi"
	"github.com/Subyard/Subyard/internal/rpc"
)

const (
	profileListCapability    = "profile-list-v1"
	settingsListCapability   = "settings-list-v1"
	hostSyncStatusCapability = "host-sync-status-v1"
	sessionPrepareCapability = "session-prepare-v1"
	yardBootstrapCapability  = "yard-bootstrap-v1"
)

func (handler *rpcHandler) requireQueryOwner() error {
	if handler.loaded.Context.AccessKind != domain.AccessLocal {
		return &rpc.Error{Code: "remote_owner_required", Message: "request this query on the owner host"}
	}
	return nil
}

func (handler *rpcHandler) currentOwnerQuery() (*rpcHandler, error) {
	if err := handler.requireQueryOwner(); err != nil {
		return nil, err
	}
	bound := handler.loaded.Context
	loaded, err := config.Load(config.LoadOptions{Catalog: &handler.cli.catalog,
		RepositoryRoot: handler.cli.options.RepositoryRoot, OperatorHome: bound.Paths.OperatorHome,
		YardName: bound.YardName, Environment: handler.cli.baseEnv})
	if err != nil {
		return nil, &rpc.Error{Code: "owner_configuration_invalid", Message: "current owner configuration is unavailable or invalid"}
	}
	if loaded.Context.AccessKind != bound.AccessKind || loaded.Context.Paths.ConfigHome != bound.Paths.ConfigHome ||
		loaded.Context.IncusProject != bound.IncusProject || loaded.Context.YardInstanceName != bound.YardInstanceName {
		return nil, &rpc.Error{Code: "owner_context_changed", Message: "owner context changed; reconnect before querying this yard"}
	}
	return &rpcHandler{cli: handler.cli, loaded: loaded}, nil
}

func (handler *rpcHandler) ownerQueries() ownerapi.Service {
	incus, _ := handler.cli.statusPorts()
	return ownerapi.Service{
		Loaded: handler.loaded, RepositoryRoot: handler.cli.options.RepositoryRoot,
		Environment: handler.cli.baseEnv, Instances: incus, Resources: handler.cli.resources,
		ProjectsAllowed: requireProjectRole(handler.loaded) == nil,
		OperationID:     handler.cli.env["SUBYARD_OPERATION_ID"], Credentials: handler.credentials(),
		ProvisionProfiles: func() ([]string, error) { return provisionableProfiles(handler.cli.options.RepositoryRoot) },
		ProfileCheck: func() ownerapi.ProfileCheck {
			definition, ok := handler.cli.manifest.Lookup("provision")
			if !ok {
				return ownerapi.ProfileCheck{}
			}
			return ownerapi.ProfileCheck{
				Runner:  handler.cli.operationOrchestrator(handler.cli.env["SUBYARD_OPERATION_ID"], handler.loaded, nil, &definition).Runner,
				Context: structuredCommandContext(handler.loaded),
			}
		},
		InspectGit: func(ctx context.Context, record configsync.SourceRecord) ownerapi.GitState {
			state := handler.cli.inspectConfigGit(ctx, record.Checkout)
			verifyConfigGitRegistration(record, &state)
			return ownerapi.GitState{Branch: state.Branch, Upstream: state.Upstream, Head: state.Head,
				Relation: state.Relation, Worktree: state.Worktree, Remote: state.RemoteRaw,
				Staged: state.Staged, Unstaged: state.Unstaged, Untracked: state.Untracked,
				Conflicts: state.Conflicts, Ahead: state.Ahead, Behind: state.Behind,
				LastFetch: state.LastFetch, Available: state.Problem == nil}
		},
	}
}

func ownerQueryError(err error) error {
	var query *ownerapi.Error
	if errors.As(err, &query) {
		return &rpc.Error{Code: query.Code, Message: query.Message}
	}
	return err
}

func (handler *rpcHandler) profileList(ctx context.Context) (ownerapi.ProfileList, error) {
	if err := handler.requireQueryOwner(); err != nil {
		return ownerapi.ProfileList{}, err
	}
	result, err := handler.ownerQueries().ProfileList(ctx)
	return result, ownerQueryError(err)
}
func (handler *rpcHandler) settingsList() (ownerapi.SettingsList, error) {
	if err := handler.requireQueryOwner(); err != nil {
		return ownerapi.SettingsList{}, err
	}
	result, err := handler.ownerQueries().SettingsList()
	return result, ownerQueryError(err)
}
func (handler *rpcHandler) hostSyncStatus(ctx context.Context) (ownerapi.HostSyncStatus, error) {
	if err := handler.requireQueryOwner(); err != nil {
		return ownerapi.HostSyncStatus{}, err
	}
	result, err := handler.ownerQueries().HostSyncStatus(ctx)
	return result, ownerQueryError(err)
}
func (handler *rpcHandler) prepareSession(ctx context.Context, params ownerapi.SessionParams) (ownerapi.SessionPreparation, error) {
	if err := ownerapi.ValidateSessionParams(params); err != nil {
		return ownerapi.SessionPreparation{}, ownerQueryError(err)
	}
	if err := handler.requireQueryOwner(); err != nil {
		return ownerapi.SessionPreparation{}, err
	}
	result, err := handler.ownerQueries().PrepareSession(ctx, params)
	return result, ownerQueryError(err)
}

// Reserve room for the response envelope within the protocol's 1 MiB frame.
func boundedOwnerQuery(value any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	var decoded any
	if err != nil || len(encoded) > rpc.MaxFrameSize-4096 || json.Unmarshal(encoded, &decoded) != nil || !boundedQueryValue(decoded) {
		return nil, &rpc.Error{Code: "query_too_large", Message: "owner query exceeds the protocol response bound"}
	}
	return value, nil
}

func boundedQueryValue(value any) bool {
	switch value := value.(type) {
	case string:
		return len(value) <= 8192
	case []any:
		if len(value) > 1024 {
			return false
		}
		for _, child := range value {
			if !boundedQueryValue(child) {
				return false
			}
		}
	case map[string]any:
		for _, child := range value {
			if !boundedQueryValue(child) {
				return false
			}
		}
	}
	return true
}
