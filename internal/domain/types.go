package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type AccessKind string

const (
	AccessLocal  AccessKind = "local"
	AccessRemote AccessKind = "remote"
)

type YardKind string

type YardImageRef string

type ResolvedYardImage string

const (
	YardContainer YardKind = "container"
	YardVM        YardKind = "vm"
)

type RuntimePaths struct {
	RepositoryRoot string `json:"repositoryRoot"`
	ConfigDir      string `json:"configDir"`
	OperatorHome   string `json:"operatorHome"`
	ConfigHome     string `json:"configHome"`
	DataHome       string `json:"dataHome"`
	StoragePath    string `json:"storagePath"`
	HostBase       string `json:"hostBase"`
	StateDir       string `json:"stateDir"`
}

type Context struct {
	YardName         string       `json:"yardName"`
	AccessKind       AccessKind   `json:"accessKind"`
	YardKind         YardKind     `json:"yardKind"`
	YardInstanceName string       `json:"yardInstanceName"`
	IncusProject     string       `json:"incusProject"`
	IncusBridge      string       `json:"incusBridge"`
	SSHHost          string       `json:"sshHost"`
	DevUser          string       `json:"devUser"`
	SSHPort          int          `json:"sshPort"`
	OwnerEndpoint    string       `json:"ownerEndpoint,omitempty"`
	OwnerYardName    string       `json:"ownerYardName,omitempty"`
	YardImageRef     YardImageRef `json:"yardImageRef,omitempty"`
	ShiftMode        string       `json:"shiftMode"`
	ForwardSSHAgent  bool         `json:"forwardSshAgent"`
	DevSudo          bool         `json:"devSudo"`
	NestedE2EVMs     bool         `json:"nestedE2EVMs"`
	DevUID           int          `json:"devUid"`
	Paths            RuntimePaths `json:"paths"`
}

func (ctx *Context) UnmarshalJSON(payload []byte) error {
	type canonicalContext Context
	var canonical canonicalContext
	if err := json.Unmarshal(payload, &canonical); err != nil {
		return err
	}
	var legacy struct {
		AccessKind       *AccessKind `json:"yardType"`
		YardKind         *YardKind   `json:"instanceType"`
		YardInstanceName *string     `json:"instanceName"`
		OwnerEndpoint    *string     `json:"remoteDest"`
		OwnerYardName    *string     `json:"remoteYard"`
	}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return err
	}
	if legacy.AccessKind != nil {
		if _, exists := fields["accessKind"]; exists && canonical.AccessKind != *legacy.AccessKind {
			return errors.New("context contains conflicting accessKind and legacy yardType")
		}
		if _, exists := fields["accessKind"]; !exists {
			canonical.AccessKind = *legacy.AccessKind
		}
	}
	if legacy.YardKind != nil {
		if _, exists := fields["yardKind"]; exists && canonical.YardKind != *legacy.YardKind {
			return errors.New("context contains conflicting yardKind and legacy instanceType")
		}
		if _, exists := fields["yardKind"]; !exists {
			canonical.YardKind = *legacy.YardKind
		}
	}
	if legacy.YardInstanceName != nil {
		if _, exists := fields["yardInstanceName"]; exists &&
			canonical.YardInstanceName != *legacy.YardInstanceName {
			return errors.New("context contains conflicting yardInstanceName and legacy instanceName")
		}
		if _, exists := fields["yardInstanceName"]; !exists {
			canonical.YardInstanceName = *legacy.YardInstanceName
		}
	}
	if legacy.OwnerEndpoint != nil {
		if _, exists := fields["ownerEndpoint"]; exists && canonical.OwnerEndpoint != *legacy.OwnerEndpoint {
			return errors.New("context contains conflicting ownerEndpoint and legacy remoteDest")
		}
		if _, exists := fields["ownerEndpoint"]; !exists {
			canonical.OwnerEndpoint = *legacy.OwnerEndpoint
		}
	}
	if legacy.OwnerYardName != nil {
		if _, exists := fields["ownerYardName"]; exists && canonical.OwnerYardName != *legacy.OwnerYardName {
			return errors.New("context contains conflicting ownerYardName and legacy remoteYard")
		}
		if _, exists := fields["ownerYardName"]; !exists {
			canonical.OwnerYardName = *legacy.OwnerYardName
		}
	}
	*ctx = Context(canonical)
	return nil
}

func NormalizeContext(ctx Context) (Context, error) {
	if ctx.DevUser == "" {
		ctx.DevUser = "dev"
	}
	clean := func(name, value string) (string, error) {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("%s must be an absolute path", name)
		}
		return filepath.Clean(value), nil
	}

	fields := []struct {
		name  string
		value *string
	}{
		{"repository root", &ctx.Paths.RepositoryRoot},
		{"config dir", &ctx.Paths.ConfigDir},
		{"operator home", &ctx.Paths.OperatorHome},
		{"config home", &ctx.Paths.ConfigHome},
		{"data home", &ctx.Paths.DataHome},
		{"storage path", &ctx.Paths.StoragePath},
		{"host base", &ctx.Paths.HostBase},
		{"state dir", &ctx.Paths.StateDir},
	}
	for _, field := range fields {
		value, err := clean(field.name, *field.value)
		if err != nil {
			return Context{}, err
		}
		*field.value = value
	}
	if err := ctx.Validate(); err != nil {
		return Context{}, err
	}
	return ctx, nil
}

func (ctx Context) Validate() error {
	if ctx.YardName == "" {
		return errors.New("yard name is required")
	}
	if ctx.AccessKind != AccessLocal && ctx.AccessKind != AccessRemote {
		return fmt.Errorf("access kind must be %q or %q", AccessLocal, AccessRemote)
	}
	if ctx.YardKind != YardContainer && ctx.YardKind != YardVM {
		return fmt.Errorf("yard kind must be %q or %q", YardContainer, YardVM)
	}
	if ctx.NestedE2EVMs && ctx.YardKind != YardContainer {
		return errors.New("nested E2E VMs currently require a container yard")
	}
	if ctx.YardInstanceName == "" || ctx.IncusProject == "" || ctx.SSHHost == "" || ctx.DevUser == "" {
		return errors.New("instance name, Incus project, SSH host and dev user are required")
	}
	if ctx.ShiftMode != "shift" && ctx.ShiftMode != "acl" {
		return errors.New("shift mode must be shift or acl")
	}
	if ctx.DevUID < 0 {
		return errors.New("dev UID must be non-negative")
	}
	if ctx.Paths.HostBase != filepath.Clean(ctx.Paths.HostBase) {
		return errors.New("host base must be normalized")
	}
	if ctx.Paths.DataHome != filepath.Clean(ctx.Paths.DataHome) {
		return errors.New("data home must be normalized")
	}
	if broadHostPath(ctx.Paths.HostBase, ctx.Paths.OperatorHome) {
		return fmt.Errorf("host base is too broad: %s", ctx.Paths.HostBase)
	}
	if broadHostPath(ctx.Paths.DataHome, ctx.Paths.OperatorHome) {
		return fmt.Errorf("data home is too broad: %s", ctx.Paths.DataHome)
	}
	if ctx.AccessKind == AccessLocal && (ctx.SSHPort < 1 || ctx.SSHPort > 65535) {
		return errors.New("SSH port must be an integer from 1 to 65535")
	}
	if ctx.AccessKind == AccessRemote && ctx.OwnerEndpoint == "" {
		return errors.New("remote yard context requires an owner endpoint")
	}
	if ctx.AccessKind == AccessRemote && !SafeName(ctx.OwnerYardName) {
		return errors.New("remote yard context requires a canonical owner yard name")
	}
	return nil
}

func broadHostPath(path, operatorHome string) bool {
	broad := []string{"/", "/boot", "/dev", "/etc", "/home", "/opt", "/proc", "/root", "/run", "/srv", "/sys", "/usr", "/var"}
	return slices.Contains(broad, path) || path == operatorHome
}

type ProjectMode string

const (
	ProjectSync ProjectMode = "sync"
	ProjectGit  ProjectMode = "git"
	ProjectBind ProjectMode = "bind"
)

type ProjectRecord struct {
	Schema          int         `json:"schema"`
	IdentityVersion int         `json:"identityVersion,omitempty"`
	ProjectID       string      `json:"projectId"`
	Name            string      `json:"name"`
	HostPath        string      `json:"hostPath"`
	SourceKey       string      `json:"sourceKey,omitempty"`
	YardPath        string      `json:"yardPath"`
	Mode            ProjectMode `json:"mode"`
	SSHHost         string      `json:"sshHost"`
	ImportedAt      string      `json:"importedAt,omitempty"`
	Target          string      `json:"target,omitempty"`
	Profile         string      `json:"profile,omitempty"`
	RegistrySource  string      `json:"registrySource,omitempty"`
}

func (record ProjectRecord) Validate(expectedID string) error {
	if record.Schema != 1 {
		return fmt.Errorf("unsupported project state schema %d; expected schema 1", record.Schema)
	}
	if !SafeID(record.ProjectID) {
		return fmt.Errorf("invalid project ID %q", record.ProjectID)
	}
	if record.IdentityVersion != 0 && record.IdentityVersion != 2 {
		return fmt.Errorf("unsupported project identity version %d", record.IdentityVersion)
	}
	if record.IdentityVersion == 2 &&
		(record.ProjectID != record.Name || !SafeProjectName(record.Name)) {
		return errors.New("canonical project identity requires ProjectID == Name and a safe name")
	}
	if expectedID != "" && record.ProjectID != expectedID {
		return fmt.Errorf("project ID %q does not match filename %q", record.ProjectID, expectedID)
	}
	if record.Name == "" || record.YardPath == "" || record.SSHHost == "" {
		return errors.New("project name, yard path and SSH host are required")
	}
	if record.SourceKey != "" {
		if len(record.SourceKey) != 64 {
			return errors.New("project source key must be a SHA-256 hex digest")
		}
		for _, character := range record.SourceKey {
			if !strings.ContainsRune("0123456789abcdef", character) {
				return errors.New("project source key must be a SHA-256 hex digest")
			}
		}
	}
	expectedPath := filepath.Join("/srv/workspaces", record.ProjectID, "src")
	if record.YardPath != expectedPath {
		return fmt.Errorf("project yard path must be %q", expectedPath)
	}
	if record.Mode != ProjectSync && record.Mode != ProjectGit && record.Mode != ProjectBind {
		return fmt.Errorf("invalid project mode %q", record.Mode)
	}
	if record.Target != "" && record.Target != "yard" && !SafeName(record.Target) {
		return fmt.Errorf("invalid project target %q", record.Target)
	}
	if record.Profile != "" && !SafeName(record.Profile) {
		return fmt.Errorf("invalid project profile %q", record.Profile)
	}
	if record.RegistrySource != "" && record.RegistrySource != "yard" {
		return fmt.Errorf("invalid registry source %q", record.RegistrySource)
	}
	return nil
}

func SafeID(value string) bool {
	if value == "" || value == "." || value == ".." || strings.HasPrefix(value, "-") {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '.' && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func SafeProjectName(value string) bool {
	return len(value) <= 50 && SafeID(value)
}

func ProjectNameKey(value string) string {
	return strings.ToLower(value)
}

func ProjectNamesEqual(left, right string) bool {
	return ProjectNameKey(left) == ProjectNameKey(right)
}

func SafeName(value string) bool {
	if value == "" || !((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= '0' && value[0] <= '9')) {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func SafeSSHTarget(value string) bool {
	return value != "" && value[0] != '-' && strings.IndexFunc(value, func(char rune) bool {
		return !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && !strings.ContainsRune("_.@:\u005b\u005d+-", char)
	}) < 0
}

type CredentialMetadata struct {
	SchemaVersion   int       `json:"schemaVersion"`
	CredentialID    string    `json:"credentialId"`
	RevisionID      string    `json:"revisionId"`
	Parents         []string  `json:"parents"`
	Label           string    `json:"label"`
	Kind            string    `json:"kind"`
	Zone            string    `json:"zone"`
	Scope           string    `json:"scope"`
	Consumer        string    `json:"consumer"`
	State           string    `json:"state"`
	RecipientActors []string  `json:"recipientActors"`
	Exclusive       bool      `json:"exclusive"`
	Syncable        bool      `json:"syncable"`
	AuthorityHost   string    `json:"authorityHost"`
	AssignedYard    string    `json:"assignedYard"`
	AssignmentEpoch int64     `json:"assignmentEpoch"`
	ActorID         string    `json:"actorId"`
	ActorCounter    int64     `json:"actorCounter"`
	Timestamp       time.Time `json:"timestamp"`
}

type CredentialSyncState struct {
	Peer                string `json:"peer"`
	LastAttempt         int64  `json:"lastAttempt"`
	LastSuccess         int64  `json:"lastSuccess"`
	Error               string `json:"error"`
	LastHead            string `json:"lastHead"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	NextRetry           int64  `json:"nextRetry"`
}

type CredentialPeer struct {
	SchemaVersion int    `json:"schemaVersion"`
	Name          string `json:"name"`
	ActorID       string `json:"actorId"`
	AgeRecipient  string `json:"ageRecipient"`
	SigningPublic string `json:"signingPublic"`
	Transport     string `json:"transport"`
	Dest          string `json:"dest"`
	OwnerYardName string `json:"ownerYardName"`
	ManualOnly    bool   `json:"manualOnly"`
	Trusted       bool   `json:"trusted"`
}

func (peer *CredentialPeer) UnmarshalJSON(payload []byte) error {
	type canonical CredentialPeer
	var value canonical
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	var legacy struct {
		OwnerYardName *string `json:"remoteYard"`
	}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return err
	}
	if legacy.OwnerYardName != nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			return err
		}
		if _, exists := fields["ownerYardName"]; exists && value.OwnerYardName != *legacy.OwnerYardName {
			return errors.New("credential peer contains conflicting ownerYardName and legacy remoteYard")
		}
		if _, exists := fields["ownerYardName"]; !exists {
			value.OwnerYardName = *legacy.OwnerYardName
		}
	}
	*peer = CredentialPeer(value)
	return nil
}

type CredentialSummary struct {
	CredentialID    string   `json:"credentialId"`
	Label           string   `json:"label"`
	Kind            string   `json:"kind"`
	Zone            string   `json:"zone"`
	Consumer        string   `json:"consumer"`
	State           string   `json:"state"`
	Heads           []string `json:"heads"`
	NeedsMerge      bool     `json:"needsMerge"`
	Conflict        bool     `json:"conflict"`
	ConflictReason  string   `json:"conflictReason,omitempty"`
	Exclusive       bool     `json:"exclusive"`
	AuthorityHost   string   `json:"authorityHost,omitempty"`
	AssignedYard    string   `json:"assignedYard,omitempty"`
	AssignmentEpoch int64    `json:"assignmentEpoch,omitempty"`
	Syncable        bool     `json:"syncable"`
}

type CredentialPeerStatus struct {
	Name                string `json:"name"`
	Role                string `json:"role"`
	ManualOnly          bool   `json:"manualOnly"`
	Trusted             bool   `json:"trusted"`
	LastAttempt         int64  `json:"lastAttempt"`
	LastSuccess         int64  `json:"lastSuccess"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	NextRetry           int64  `json:"nextRetry"`
	Failed              bool   `json:"failed"`
}

type CredentialStatus struct {
	Credentials []CredentialSummary    `json:"credentials"`
	Peers       []CredentialPeerStatus `json:"peers"`
}

type OperationEvent struct {
	OperationID string         `json:"operationId"`
	Sequence    uint64         `json:"sequence"`
	Revision    uint64         `json:"revision"`
	Kind        string         `json:"kind"`
	At          time.Time      `json:"at"`
	Data        map[string]any `json:"data,omitempty"`
}

type AdapterRequest struct {
	Schema      int               `json:"schema"`
	OperationID string            `json:"operationId"`
	Adapter     string            `json:"adapter"`
	Action      string            `json:"action"`
	Arguments   []string          `json:"arguments,omitempty"`
	Context     map[string]string `json:"context"`
	Input       map[string]any    `json:"input,omitempty"`
}

type AdapterResult struct {
	Schema      int            `json:"schema"`
	OperationID string         `json:"operationId"`
	Status      string         `json:"status"`
	Output      map[string]any `json:"output,omitempty"`
	ErrorCode   string         `json:"errorCode,omitempty"`
}

type CommandEffect string

const (
	CommandRead   CommandEffect = "read"
	CommandMutate CommandEffect = "mutate"
)

type ConfirmationPolicy string

const (
	ConfirmationNever            ConfirmationPolicy = "never"
	ConfirmationPromptDefaultYes ConfirmationPolicy = "prompt-default-yes"
	ConfirmationPromptDefaultNo  ConfirmationPolicy = "prompt-default-no"
)

type RemotePolicy string

const (
	RemoteOnController RemotePolicy = "local"
	RemoteOnOwner      RemotePolicy = "forward"
	RemoteDenied       RemotePolicy = "deny"
)

type ExecutionTarget string

const (
	TargetLocalController ExecutionTarget = "local-controller"
	TargetLocalOwner      ExecutionTarget = "local-owner"
	TargetRemoteOwner     ExecutionTarget = "remote-owner"
)

type CommandPolicy struct {
	Name         string
	Effect       CommandEffect
	Confirmation ConfirmationPolicy
	RemotePolicy RemotePolicy
	Consequences []string
}

type OperationPlan struct {
	OperationID         string               `json:"operationId"`
	Command             string               `json:"command"`
	Effect              CommandEffect        `json:"effect"`
	Confirmation        ConfirmationPolicy   `json:"confirmation"`
	Target              ExecutionTarget      `json:"target"`
	Consequences        []string             `json:"consequences,omitempty"`
	Assessment          *ActionAssessment    `json:"assessment,omitempty"`
	ConfirmationRequest *ConfirmationRequest `json:"confirmationRequest,omitempty"`
	Confirmed           bool                 `json:"confirmed"`
	CreatedAt           time.Time            `json:"createdAt"`
}
