package configsync

import (
	"errors"

	"github.com/Subyard/Subyard/internal/config"
)

const (
	sourceSchema   = 1
	manifestSchema = 2
)

var (
	ErrPlanStale       = errors.New("config sync plan is stale")
	ErrRecoveryPending = errors.New("an interrupted config sync transaction requires recovery")
)

type SourceManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	Policy        map[string]string `json:"policy,omitempty"`
}

type ManagedFile struct {
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Mode       uint32 `json:"mode"`
	Generation uint64 `json:"generation"`
}

type Manifest struct {
	SchemaVersion int           `json:"schemaVersion"`
	Generation    uint64        `json:"generation"`
	SourceID      string        `json:"sourceId"`
	SourceCommit  string        `json:"sourceCommit"`
	HostID        string        `json:"hostId"`
	SourceSchema  int           `json:"sourceSchema"`
	SourceDigest  string        `json:"sourceDigest"`
	Files         []ManagedFile `json:"files"`
}

type Change struct {
	Path         string
	Action       string
	BeforeDigest string
	AfterDigest  string
	Mode         uint32
	Applications []config.SettingApplication
	Detail       string
}

type Plan struct {
	SourceRoot         string
	SourceID           string
	SourceCommit       string
	SourceDigest       string
	HostID             string
	InitializeHostID   bool
	PreviousGeneration uint64
	Generation         uint64
	Changes            []Change
	ManifestChanged    bool
	Digest             string
	Adopt              bool

	options     Options
	desired     map[string]candidateFile
	previous    Manifest
	localInputs []localInputObservation
}

func (plan Plan) NeedsApply() bool {
	return plan.InitializeHostID || len(plan.Changes) != 0 || plan.ManifestChanged
}

func (plan Plan) NeedsConfirmation() bool {
	return plan.InitializeHostID || len(plan.Changes) != 0
}

type YardUseProbe func(name string) (reason string, inUse bool, err error)

type Options struct {
	SourceRoot         string
	SourceIdentityRoot string
	ConfigHome         string
	RepositoryRoot     string
	OperatorHome       string
	Environment        map[string]string
	FileSettings       []config.FileSettingMapping
	Adopt              bool
	ConfigLocked       bool
	YardInUse          YardUseProbe
}

type Status struct {
	HostID           string
	HostIDPending    bool
	ManifestPath     string
	ManifestPresent  bool
	SourceCommit     string
	Generation       uint64
	RecoveryRequired bool
}

type candidateFile struct {
	SourcePath   string
	Content      []byte
	Digest       string
	Mode         uint32
	Applications []config.SettingApplication
}
