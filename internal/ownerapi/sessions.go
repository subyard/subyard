package ownerapi

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/state"
	"golang.org/x/crypto/ssh"
)

// ValidateSessionParams preserves parameter validation before owner routing checks.
func ValidateSessionParams(params SessionParams) error {
	if params.Kind != "shell" && params.Kind != "resources" && params.Kind != "vscode" {
		return &Error{Code: "invalid_params", Message: "unsupported session kind, scope or project selection"}
	}
	if params.Scope == "" {
		params.Scope = "yard"
	}
	if params.Scope != "yard" && params.Scope != "host" || params.ProjectID != "" && !domain.SafeID(params.ProjectID) ||
		params.Scope == "host" && (params.ProjectID != "" || params.Kind == "vscode") ||
		params.Kind == "resources" && params.ProjectID != "" || params.Kind == "vscode" && params.ProjectID == "" {
		return &Error{Code: "invalid_params", Message: "unsupported session kind, scope or project selection"}
	}
	return nil
}

type SessionParams struct {
	Kind      string `json:"kind"`
	Scope     string `json:"scope,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
}

type SessionVSCode struct {
	SSHAlias             string `json:"sshAlias"`
	DevUser              string `json:"devUser"`
	Address              string `json:"address"`
	Port                 int    `json:"port"`
	FolderPath           string `json:"folderPath"`
	HostKey              string `json:"hostKey"`
	HostKeyFingerprint   string `json:"hostKeyFingerprint"`
	RemoteAuthentication string `json:"remoteAuthentication"`
}

type SessionPreparation struct {
	SchemaVersion  int            `json:"schemaVersion"`
	Kind           string         `json:"kind"`
	Scope          string         `json:"scope"`
	YardName       string         `json:"yardName"`
	ProjectID      string         `json:"projectId,omitempty"`
	OwnerArguments []string       `json:"ownerArguments,omitempty"`
	LocalArguments []string       `json:"localArguments,omitempty"`
	VSCode         *SessionVSCode `json:"vscode,omitempty"`
}

func (service Service) PrepareSession(ctx context.Context, params SessionParams) (SessionPreparation, error) {
	result := SessionPreparation{SchemaVersion: 1, Kind: params.Kind, Scope: params.Scope, YardName: service.Loaded.Context.YardName}
	if err := ValidateSessionParams(params); err != nil {
		return result, err
	}
	if params.Scope == "" {
		params.Scope, result.Scope = "yard", "yard"
	}
	if params.Scope == "host" {
		if params.Kind == "resources" {
			if _, err := exec.LookPath("htop"); err != nil {
				return result, &Error{Code: "session_tool_unavailable", Message: "host resources require htop; install htop on the owner host and retry"}
			}
			result.OwnerArguments = []string{"htop"}
		} else {
			result.OwnerArguments = []string{"bash", "-l"}
		}
		return result, nil
	}
	yard := service.Loaded.Context
	incus := service.Instances
	instance, err := incus.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil || !strings.EqualFold(instance.Status, "running") {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, &Error{Code: "session_yard_unavailable", Message: "yard session requires an already running yard; start it explicitly"}
	}
	var record domain.ProjectRecord
	if params.ProjectID != "" {
		if !service.ProjectsAllowed {
			return result, &Error{Code: "session_project_forbidden", Message: "this yard role does not permit project sessions"}
		}
		store, err := state.NewFileStore(yard.Paths.StateDir)
		if err == nil {
			record, err = store.GetReadOnly(ctx, params.ProjectID)
		}
		if err != nil {
			return result, &Error{Code: "session_project_unavailable", Message: "project is not registered in the selected owner yard"}
		}
		result.ProjectID = record.ProjectID
	}
	if params.Kind != "vscode" {
		result.OwnerArguments = []string{"yard", "-Y", yard.YardName, "shell"}
		if record.ProjectID != "" {
			result.OwnerArguments = append(result.OwnerArguments, record.ProjectID)
		}
		if params.Kind == "resources" {
			result.OwnerArguments = append(result.OwnerArguments, "--", "htop")
		}
		return result, nil
	}
	if !domain.SafeSSHTarget(yard.SSHHost) {
		return result, &Error{Code: "session_ssh_unavailable", Message: "yard SSH transport is unavailable; reconcile the yard explicitly"}
	}
	if err := (application.ProjectActionRunner{Yard: yard, Instances: incus}).CheckCodeTargetReady(ctx); err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, &Error{Code: "session_ssh_unavailable", Message: "yard SSH transport is unavailable; reconcile the yard explicitly"}
	}
	key, err := sessionGuestHostKey(yard)
	if err != nil {
		return result, &Error{Code: "session_host_key_unavailable", Message: "protected owner guest SSH pin is unavailable or ambiguous; reconcile the yard explicitly"}
	}
	result.LocalArguments = []string{"yard", "-Y", yard.YardName, "code", record.ProjectID}
	result.VSCode = &SessionVSCode{SSHAlias: yard.SSHHost, DevUser: yard.DevUser, Address: "127.0.0.1", Port: yard.SSHPort,
		FolderPath: record.YardPath, HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
		HostKeyFingerprint: ssh.FingerprintSHA256(key), RemoteAuthentication: "already-authorized-desktop-agent-key"}
	return result, nil
}

func sessionGuestHostKey(yard domain.Context) (ssh.PublicKey, error) {
	root := filepath.Join(yard.Paths.DataHome, "ssh")
	snapshot, err := config.ReadPersistentFileSnapshot(root, filepath.Join(root, "known_hosts"))
	if err != nil || !snapshot.Exists || len(snapshot.Content) > 64<<10 {
		return nil, errors.New("guest SSH pin unavailable")
	}
	namespace := "[127.0.0.1]:" + strconv.Itoa(yard.SSHPort)
	var selected ssh.PublicKey
	for _, line := range bytes.Split(snapshot.Content, []byte("\n")) {
		marker, hosts, key, _, _, err := ssh.ParseKnownHosts(line)
		if err != nil || marker != "" || !slices.Contains(hosts, namespace) || key.Type() != ssh.KeyAlgoED25519 {
			continue
		}
		if selected != nil && !bytes.Equal(selected.Marshal(), key.Marshal()) {
			return nil, errors.New("guest SSH pin ambiguous")
		}
		selected = key
	}
	if selected == nil {
		return nil, errors.New("guest SSH pin unavailable")
	}
	return selected, nil
}
