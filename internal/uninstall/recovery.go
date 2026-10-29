package uninstall

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type RecoveryStage string

const (
	RecoveryPrepared         RecoveryStage = "prepared"
	RecoveryProducersStopped RecoveryStage = "producers-stopped"
	RecoveryYardsRemoved     RecoveryStage = "yards-removed"
	RecoveryFilesRemoved     RecoveryStage = "files-removed"
)

// TeardownSnapshot is the approved physical plan used live and after recovery.
// Agent is a process-local handle; its original ownership binding is persisted.
type TeardownSnapshot struct {
	Resources    []ports.TeardownResource
	Artifacts    []ports.TeardownArtifact
	Registered   []domain.Context
	Services     []RecoveryService `json:",omitempty"`
	State        ports.ReconcileState
	Agent        *sshagentruntime.PreparedLock `json:"-"`
	AgentBinding string
	AgentState   string
}

type RecoveryYard struct {
	Context     domain.Context
	Environment map[string]string
	Snapshot    *TeardownSnapshot
	Network     *yardnetwork.RemovalPlan
}

type RecoveryService struct {
	Scope     string   `json:"scope"`
	Units     []string `json:"units"`
	Artifacts []ports.TeardownArtifact
}

// RecoveryProfileCheck pins a shipped owner's read-only conditional proof to
// the same public hook and nonsecret installation context approved by the plan.
type RecoveryProfileCheck struct {
	Profile     string
	Hook        string
	Environment map[string]string
}

type RecoveryPlan struct {
	Schema              int
	Installation        Installation
	Stage               RecoveryStage
	Yards               []RecoveryYard
	Trees               []Tree
	Launchers           []Tree
	Startup             []StartupEdit
	OwnedGitDirectories []string
	Services            []RecoveryService
	Preserved           []string
	SourceRoot          string
	SourceAssets        []Tree
	AssetsReady         bool
	Shared              []ports.TeardownResource
	OwnerLock           *Tree
	EpochBefore         uint64
	ProfileChecks       []RecoveryProfileCheck
}

type RecoveryStore struct {
	Directory string
	UID       uint32
}

func (store RecoveryStore) validateDirectory() error {
	if err := safePath(filepath.Join(store.Directory, "plan.json")); err != nil {
		return err
	}
	info, err := os.Lstat(store.Directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != store.UID {
		return config.OwnerMetadataError(store.Directory, "uninstall recovery root", fmt.Sprintf("must be an operator-owned real mode 0700 directory; %s; required type=directory, mode=0700, uid=%d", config.FileMetadata(info), store.UID), "", nil)
	}
	return nil
}

func (plan RecoveryPlan) Validate(uid uint32) error {
	if plan.Schema != 1 {
		return errors.New("unsupported full uninstall recovery schema")
	}
	if err := plan.Installation.Validate(plan.Installation.DataHome, uid); err != nil {
		return err
	}
	switch plan.Stage {
	case RecoveryPrepared, RecoveryProducersStopped, RecoveryYardsRemoved, RecoveryFilesRemoved:
	default:
		return errors.New("invalid full uninstall recovery stage")
	}
	if len(plan.Yards) > 128 || len(plan.Trees) > 256 || len(plan.Startup) > 16 || len(plan.Launchers) > 32 {
		return errors.New("uninstall recovery plan exceeds its bounded inventory")
	}
	if len(plan.ProfileChecks) > 128 {
		return errors.New("uninstall conditional owner inventory is oversized")
	}
	if plan.EpochBefore == ^uint64(0) {
		return errors.New("full uninstall owner epoch cannot advance")
	}
	for _, check := range plan.ProfileChecks {
		if check.Profile == "" || filepath.Base(check.Profile) != check.Profile || check.Hook == "" || filepath.IsAbs(check.Hook) || filepath.Clean(check.Hook) != check.Hook || check.Hook == ".." || strings.HasPrefix(check.Hook, "../") || len(check.Environment) > 2048 {
			return errors.New("invalid conditional owner check")
		}
		for name, value := range check.Environment {
			if err := config.ValidateNonSecretContent(name, value); err != nil {
				return errors.New("conditional owner context contains protected content")
			}
		}
	}
	if len(plan.OwnedGitDirectories) > 256 {
		return errors.New("uninstall owned Git inventory is oversized")
	}
	for _, path := range plan.OwnedGitDirectories {
		if err := safePath(path); err != nil {
			return err
		}
		contained := false
		for _, tree := range plan.Trees {
			if strings.HasPrefix(path, tree.Path+"/") {
				contained = true
				break
			}
		}
		if !contained {
			return errors.New("owned Git directory is outside the approved uninstall roots")
		}
	}
	for _, yard := range plan.Yards {
		if yard.Context.AccessKind != domain.AccessLocal || yard.Context.Paths.DataHome != plan.Installation.DataHome || yard.Context.Paths.ConfigHome != plan.Installation.ConfigHome {
			return errors.New("uninstall recovery yard belongs to another owner or installation")
		}
		if yard.Snapshot == nil || len(yard.Snapshot.Resources) > 256 || len(yard.Snapshot.Artifacts) > 256 {
			return errors.New("uninstall recovery yard inventory is oversized")
		}
		if len(yard.Environment) > 2048 {
			return errors.New("uninstall recovery environment is oversized")
		}
		for name, value := range yard.Environment {
			if err := config.ValidateNonSecretContent(name, value); err != nil {
				return errors.New("uninstall recovery context contains protected content")
			}
		}
	}
	for _, tree := range append(append([]Tree{}, plan.Trees...), plan.Launchers...) {
		if err := safePath(tree.Path); err != nil {
			return err
		}
		if len(tree.Entries) > maxEntries {
			return errors.New("uninstall recovery artifact is oversized")
		}
		if tree.Opaque && len(tree.Entries) != 1 {
			return errors.New("invalid conditional private root")
		}
		paths := map[string]bool{}
		for _, entry := range tree.Entries {
			if filepath.Clean(entry.Path) != entry.Path || filepath.IsAbs(entry.Path) || entry.Path == ".." || len(entry.Path) > 4096 || paths[entry.Path] {
				return errors.New("invalid uninstall recovery descendant path")
			}
			relative, err := filepath.Rel(tree.Path, filepath.Join(tree.Path, entry.Path))
			if err != nil || relative == ".." || len(relative) >= 3 && relative[:3] == "../" {
				return errors.New("uninstall recovery descendant escapes its root")
			}
			paths[entry.Path] = true
		}
		if len(tree.Entries) > 0 && !paths["."] {
			return errors.New("uninstall recovery root identity is missing")
		}
	}
	return nil
}

// Save publishes a durable plan only after the command has obtained consent.
// expected pins the old plan, including an interrupted operation's identity;
// a caller cannot overwrite a foreign or concurrently replaced recovery plan.
func (store RecoveryStore) Save(plan RecoveryPlan, expected *Tree) (Tree, error) {
	if err := store.validateDirectory(); err != nil {
		return Tree{}, err
	}
	if err := plan.Validate(store.UID); err != nil {
		return Tree{}, err
	}
	path := filepath.Join(store.Directory, "plan.json")
	if expected != nil && expected.Path != path {
		return Tree{}, errors.New("uninstall recovery binding names a different store")
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return Tree{}, err
	}
	if len(body) > 64<<20 {
		return Tree{}, errors.New("uninstall recovery plan exceeds the storage limit")
	}
	// The existing owner barrier also covers this native atomic checkpoint;
	// its inherited descriptor survives caller interruption.
	request := struct {
		Path     string
		UID      uint32
		Body     string
		Expected *Tree
	}{path, store.UID, base64.StdEncoding.EncodeToString(body), expected}
	if err := nativeArtifact("publish-plan", request, nil); err != nil {
		return Tree{}, err
	}
	return Capture(path)
}

func (store RecoveryStore) Read() (RecoveryPlan, Tree, error) {
	var plan RecoveryPlan
	if err := store.validateDirectory(); err != nil {
		return plan, Tree{}, err
	}
	path := filepath.Join(store.Directory, "plan.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		path = filepath.Join(store.Directory, ".plan-next")
	}
	binding, err := Capture(path)
	if err != nil {
		return plan, binding, err
	}
	if len(binding.Entries) != 1 || binding.Entries[0].Mode&syscall.S_IFMT != syscall.S_IFREG || binding.Entries[0].Mode&0777 != 0600 || binding.Entries[0].UID != store.UID {
		return plan, binding, protectedEntryError(path, "uninstall recovery plan", binding.Entries, store.UID)
	}
	body, err := store.ReadBody(binding)
	if err != nil {
		return plan, binding, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, binding, config.OwnerMetadataError(path, "uninstall recovery plan", "incomplete original full uninstall plan; retain this protected store and restore its original approved publication before continuing", "", err)
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return plan, binding, errors.New("uninstall recovery plan has trailing data")
	}
	if err := plan.Validate(store.UID); err != nil {
		return plan, binding, err
	}
	return plan, binding, nil
}

// ReadBody binds transport to the original publication, rather than hashing a
// replacement pathname and thereby authorizing different services or hooks.
func (store RecoveryStore) ReadBody(binding Tree) ([]byte, error) {
	if err := store.validateDirectory(); err != nil {
		return nil, err
	}
	if filepath.Dir(binding.Path) != store.Directory || len(binding.Entries) != 1 {
		return nil, errors.New("invalid original recovery binding")
	}
	if err := binding.Check(); err != nil {
		return nil, err
	}
	parent, err := openParent(store.Directory)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	file, err := parent.OpenFile(filepath.Base(binding.Path), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	original := binding.Entries[0]
	if !ok || uint64(stat.Dev) != original.Device || stat.Ino != original.Inode {
		return nil, errors.New("original recovery publication changed")
	}
	body, err := io.ReadAll(io.LimitReader(file, 64<<20+1))
	if err != nil || len(body) > 64<<20 {
		return nil, errors.New("uninstall recovery plan is unavailable or oversized")
	}
	if err := binding.Check(); err != nil {
		return nil, err
	}
	return body, nil
}

// CaptureProducers shares the native owner proof with ordinary lifecycle removal.
type ProducerEvidence struct {
	EngineSHA256          string
	ExecutingEngineSHA256 string
	Templates             map[string]string
}

func CaptureProducers(root string, plan RecoveryPlan, environment map[string]string, owners []string, port int, original ProducerEvidence) ([]RecoveryService, error) {
	var services []RecoveryService
	request := struct {
		Root         string
		Installation Installation
		Environment  map[string]string
		Owners       []string
		Yards        []RecoveryYard
		Port         int
		Original     ProducerEvidence
	}{root, plan.Installation, environment, owners, plan.Yards, port, original}
	err := nativeArtifact("capture-producers", request, &services)
	return services, err
}
