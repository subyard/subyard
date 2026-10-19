// Package releaseowner contains the closed component-native protocol. Release
// assessment, consent, history and reduction belong to the Rust owner.
package releaseowner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"io"
	"os"
	"time"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"golang.org/x/sys/unix"
)

const MaxRequest = 1 << 20
const MaxReply = 8 << 20

type Identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	Links  uint64 `json:"links"`
}

func IdentityOf(file *os.File) (Identity, error) {
	var s unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &s); err != nil {
		return Identity{}, err
	}
	return Identity{uint64(s.Dev), s.Ino, s.Mode, s.Uid, uint64(s.Nlink)}, nil
}

type Context struct {
	Session                string                                  `json:"session"`
	DataHome               string                                  `json:"dataHome"`
	Releases               releasetransition.ReleasePair           `json:"releases"`
	Direction              releasetransition.Direction             `json:"direction"`
	SourceIngress          *releasetransition.SourceIngressRequest `json:"sourceIngress"`
	RepositoryRoot         string                                  `json:"repositoryRoot"`
	ConfigHome             string                                  `json:"configHome"`
	RuntimeRoot            string                                  `json:"runtimeRoot"`
	Yard                   string                                  `json:"yard"`
	Target                 string                                  `json:"target"`
	Artifact               string                                  `json:"artifact"`
	Root                   Identity                                `json:"root"`
	Repository             Identity                                `json:"repository"`
	PhysicalRepositoryRoot string                                  `json:"-"`
}
type Binding struct {
	Session     string `json:"session"`
	Transaction string `json:"transaction"`
	Plan        string `json:"plan"`
	Journal     string `json:"journal"`
	Artifact    string `json:"artifact"`
	Helper      string `json:"helper"`
	Ordinal     uint64 `json:"ordinal"`
}
type Edit struct {
	Setting string  `json:"setting"`
	Value   *string `json:"value"`
}
type BeforeValue struct {
	Setting string `json:"setting"`
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}
type SettingsRecipe struct {
	Yard           string         `json:"yard"`
	Target         string         `json:"target"`
	Inheritance    string         `json:"inheritance"`
	Expected       string         `json:"expected"`
	Desired        string         `json:"desired"`
	ExpectedExists bool           `json:"expectedExists"`
	ExpectedFile   string         `json:"expectedFile"`
	DesiredFile    string         `json:"desiredFile"`
	Edits          []Edit         `json:"edits"`
	Before         []BeforeValue  `json:"before"`
	Source         string         `json:"source"`
	Inputs         []InputBinding `json:"inputs"`
}
type InputBinding struct {
	Relative string                        `json:"relative"`
	Exists   bool                          `json:"exists"`
	Digest   string                        `json:"digest"`
	Identity config.PersistentFileIdentity `json:"identity"`
}
type Assignment struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Line    int    `json:"line"`
	Direct  bool   `json:"direct"`
	Dynamic bool   `json:"dynamic"`
}
type YardSettings struct {
	Yard              string       `json:"yard"`
	CanonicalExists   bool         `json:"canonicalExists"`
	FlatExists        bool         `json:"flatExists"`
	Assignments       []Assignment `json:"assignments"`
	Inherited         []string     `json:"inherited"`
	TemplateSupported bool         `json:"templateSupported"`
	Access            string       `json:"access"`
	SSHPort           int          `json:"sshPort"`
	SSHGroup          string       `json:"sshGroup"`
	Sources           []Source     `json:"sources"`
}
type Source struct {
	Role        string       `json:"role"`
	Label       string       `json:"label"`
	Exists      bool         `json:"exists"`
	SHA256      string       `json:"sha256"`
	Assignments []Assignment `json:"assignments"`
}
type PreparedSettings struct {
	Yard           string         `json:"yard"`
	Target         string         `json:"target"`
	ExpectedExists bool           `json:"expectedExists"`
	ExpectedFile   string         `json:"expectedFile"`
	DesiredFile    string         `json:"desiredFile"`
	Source         string         `json:"source"`
	Inputs         []InputBinding `json:"inputs"`
}
type OwnerBefore struct {
	State          string `json:"state"`
	Registration   string `json:"registration"`
	Overrides      string `json:"overrides"`
	Controller     string `json:"controller"`
	SharedImages   bool   `json:"sharedImages"`
	PreparedRecipe string `json:"preparedRecipe,omitempty"`
}
type Target struct {
	PreparedRecipe  string `json:"preparedRecipe,omitempty"`
	AfterOwner      string `json:"afterOwner,omitempty"`
	Scope           string `json:"scope"`
	ReportedActual  string `json:"reportedActual"`
	ReportedDesired string `json:"reportedDesired"`
	Before          string `json:"before"`
	Class           string `json:"class"`
	ScopeState      string `json:"scopeState"`
	Stage           string `json:"stage"`
	Owner           string `json:"owner"`
	Yard            string `json:"yard"`
	Project         string `json:"project"`
	Instance        string `json:"instance"`
	InstanceUUID    string `json:"instanceUuid"`
	State           string `json:"state"`
	Actual          string `json:"actual"`
	Desired         string `json:"desired"`
	Source          string `json:"source"`
}
type Terminal struct {
	Version      int                     `json:"version"`
	Binding      Binding                 `json:"binding"`
	Owner        string                  `json:"owner"`
	Resource     string                  `json:"resource"`
	State        string                  `json:"state"`
	Server       string                  `json:"server"`
	Project      string                  `json:"project"`
	Instance     string                  `json:"instance"`
	InstanceUUID string                  `json:"instanceUuid"`
	Operation    string                  `json:"operation"`
	FinalStatus  int                     `json:"finalStatus"`
	Operations   []ports.NativeOperation `json:"operations"`
	Observed     string                  `json:"observed"`
}
type Request struct {
	Producer             *Binding              `json:"producer,omitempty"`
	OriginalV1           *OriginalV1Checkpoint `json:"originalV1,omitempty"`
	LegacyRust           bool                  `json:"legacyRust,omitempty"`
	RecipeHash           string                `json:"recipeHash,omitempty"`
	Offset               uint64                `json:"offset,omitempty"`
	Total                uint64                `json:"total,omitempty"`
	Content              string                `json:"content,omitempty"`
	Fresh                bool                  `json:"fresh,omitempty"`
	Resolved             bool                  `json:"resolved,omitempty"`
	IngressBinding       *IngressBinding       `json:"bindingIngress,omitempty"`
	Step                 *IngressStep          `json:"step,omitempty"`
	Op                   string                `json:"op"`
	Session              string                `json:"session,omitempty"`
	Yard                 string                `json:"yard,omitempty"`
	Context              *Context              `json:"context,omitempty"`
	Binding              *Binding              `json:"binding,omitempty"`
	Update               *Identity             `json:"update,omitempty"`
	Owner                json.RawMessage       `json:"owner,omitempty"`
	Edits                []Edit                `json:"edits,omitempty"`
	Recipe               *SettingsRecipe       `json:"recipe,omitempty"`
	Target               *Target               `json:"target,omitempty"`
	Before               *OwnerBefore          `json:"before,omitempty"`
	Token                string                `json:"token,omitempty"`
	Successor            string                `json:"successor,omitempty"`
	RollbackRepositories *RollbackRepositories `json:"rollbackRepositories,omitempty"`
	TargetOwners         []string              `json:"targetOwners,omitempty"`
}
type RollbackRepository struct {
	Release        string   `json:"release"`
	ArtifactDigest string   `json:"artifactDigest"`
	Root           Identity `json:"root"`
}
type RollbackRepositories struct {
	Source RollbackRepository `json:"source"`
	Target RollbackRepository `json:"target"`
}
type Reply struct {
	ResolvedRecipe  string                 `json:"resolvedRecipe,omitempty"`
	InstalledChunk  *InstalledRecipeChunk  `json:"installedChunk,omitempty"`
	RollbackSupport *ports.RollbackSupport `json:"rollbackSupport,omitempty"`
	Code            string                 `json:"code"`
	Diagnostic      *Diagnostic            `json:"diagnostic,omitempty"`
	Layout          *ConfigLayoutSnapshot  `json:"layout,omitempty"`
	Ingress         *IngressFacts          `json:"ingress,omitempty"`
	Yards           []string               `json:"yards,omitempty"`
	Owners          []string               `json:"owners,omitempty"`
	Settings        *YardSettings          `json:"settings,omitempty"`
	Recipe          *PreparedSettings      `json:"recipe,omitempty"`
	Owner           *OwnerBefore           `json:"owner,omitempty"`
	Targets         []Target               `json:"targets,omitempty"`
	Actual          string                 `json:"actual,omitempty"`
	Terminal        *Terminal              `json:"terminal,omitempty"`
}

type InstalledRecipeChunk struct {
	RecipeHash string `json:"recipeHash"`
	Offset     uint64 `json:"offset"`
	Total      uint64 `json:"total"`
	Content    string `json:"content"`
	Complete   bool   `json:"complete"`
}

type ConfigLayoutSnapshot struct {
	Exists bool   `json:"exists"`
	Raw    string `json:"raw"` // Standard padded base64 of exact original bytes.
	Digest string `json:"digest"`
}

// Components implements only physical/configuration contracts. It receives no
// release inspection or authorization grant, and returns no release outcome.
type Components interface {
	CaptureIngress(context.Context, *IngressBinding) (IngressFacts, error)
	ObserveIngress(context.Context, IngressStep) (string, error)
	ApplyIngress(context.Context, IngressStep) error
	VerifyIngress(context.Context, IngressStep) error
	CaptureInventory(context.Context) ([]string, []string, error)
	CaptureYard(context.Context, string, bool) (YardSettings, error)
	PrepareSettings(context.Context, string, []Edit) (PreparedSettings, error)
	CaptureOwner(context.Context) (OwnerBefore, error)
	CaptureActivation(context.Context, string, bool) ([]Target, error)
	ObserveSettings(context.Context, SettingsRecipe) (string, error)
	CaptureSettings(context.Context, SettingsRecipe) (string, error)
	ProjectSettings(context.Context, SettingsRecipe, []Edit) (string, error)
	ApplySettings(context.Context, SettingsRecipe) error
	ApplyOwner(context.Context, OwnerBefore, string) error
	ApplyActivation(context.Context, Target) error
	Close() error
}

// OwnerArchiveCleaner is only the terminal owning registration archive cleanup.
// It may make fixed read-only inventory queries, but must submit no mutations,
// launch no mutating child and finish every local archive write before return.
// Original preparation and recovery token remain mandatory component guards.
type OwnerArchiveCleaner interface {
	CleanupOwner(context.Context, OwnerBefore, string) error
}

// OwnerProgressReader observes only the original component preparation and
// recovery token. It never prepares a replacement owner plan or submits effects.
type OwnerProgressReader interface {
	ObserveOwner(context.Context, OwnerBefore, string) (string, error)
}

// ActivationProjector retains the owning component's original prospective
// activation scope separately from source/settings inventory.
type ActivationProjector interface {
	ProjectActivation(context.Context, OwnerBefore) ([]string, error)
}

type Factory func(context.Context, Context, *os.File) (Components, error)

func strict(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return errors.New("trailing native input")
	}
	return nil
}
func closeFiles(files []*os.File) {
	for _, file := range files {
		file.Close()
	}
}

// receiveHeader reads the first frame byte with recvmsg exclusively, then its
// remainder normally. No concurrent reader can consume ancillary descriptors.
func receiveHeader(input *os.File) ([]byte, []*os.File, error) {
	header := make([]byte, 52)
	control := make([]byte, unix.CmsgSpace(3*4))
	deadline := time.Now().Add(5 * time.Minute)
	var n, oob, flags int
	var err error
	for {
		if err = waitSocket(input, unix.POLLIN, deadline); err != nil {
			return nil, nil, err
		}
		n, oob, flags, _, err = unix.Recvmsg(int(input.Fd()), header[:1], control, unix.MSG_CMSG_CLOEXEC|unix.MSG_DONTWAIT)
		if err != unix.EINTR && err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			break
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if n == 0 {
		return nil, nil, io.EOF
	}
	files := []*os.File{}
	bad := flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0
	messages, parseErr := unix.ParseSocketControlMessage(control[:oob])
	if parseErr != nil {
		bad = true
	}
	for _, message := range messages {
		if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SCM_RIGHTS {
			bad = true
			continue
		}
		fds, e := unix.ParseUnixRights(&message)
		if e != nil {
			bad = true
			continue
		}
		for _, fd := range fds {
			files = append(files, os.NewFile(uintptr(fd), "release-native-capability"))
		}
	}
	if len(files) > 3 {
		bad = true
	}
	if bad {
		closeFiles(files)
		return nil, nil, errors.New("native ancillary refusal")
	}
	if err = readSocket(input, header[1:], time.Now().Add(5*time.Second)); err != nil {
		closeFiles(files)
		return nil, nil, err
	}
	return header, files, nil
}

// watchNativeOwner observes only socket lifetime. A signal interrupt is not
// evidence of owner loss. The poll argument keeps that distinction testable.
func watchNativeOwner(input *os.File, done <-chan struct{}, cancel context.CancelFunc, poll func([]unix.PollFd, int) (int, error)) {
	for {
		select {
		case <-done:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLRDHUP | unix.POLLHUP | unix.POLLERR}}
		_, err := poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil || fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR) != 0 {
			cancel()
			return
		}
	}
}

// Serve retains one helper for syntax normalization and all selected native
// operations. A malformed frame terminates the helper; every received FD is
// closed on refusal. Session.Close drains component work before lease closure.
func Serve(ctx context.Context, input *os.File, output io.Writer, factory Factory) error {
	image, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, io.LimitReader(image, (128<<20)+1))
	image.Close()
	if err != nil || size > 128<<20 {
		return errors.New("native image bound")
	}
	imageDigest := fmt.Sprintf("%x", hasher.Sum(nil))
	session := &session{factory: factory, image: imageDigest}
	nativeContext, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	watchDone := make(chan struct{})
	defer func() { close(done); <-watchDone }()
	// Poll never consumes stream bytes or ancillary descriptors. Only the framed
	// reader calls recvmsg. Owner EOF closes admission and cancels native work.
	go func() { defer close(watchDone); watchNativeOwner(input, done, cancel, unix.Poll) }()
	defer session.close()
	for sequence := uint64(1); sequence != 0; sequence++ {
		header, files, err := receiveHeader(input)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		err = func() error {
			defer closeFiles(files)
			size := binary.BigEndian.Uint32(header[48:])
			kind := header[5]
			if !bytes.Equal(header[:4], []byte("SYC1")) || header[4] != 1 || header[6] != 0 || header[7] != 0 || binary.BigEndian.Uint64(header[8:16]) != sequence || size == 0 || size > MaxRequest || !nativeFrameKind(kind) || kind != 15 && len(files) != 0 {
				return errors.New("native frame refusal")
			}
			raw := make([]byte, size)
			if err := readSocket(input, raw, time.Now().Add(5*time.Second)); err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			if !bytes.Equal(sum[:], header[16:48]) {
				return errors.New("native input binding")
			}
			var projection []byte
			var presence byte
			if kind == 15 {
				reply, err := session.handle(nativeContext, raw, files)
				if err != nil {
					return err
				}
				projection, err = json.Marshal(reply)
				if err != nil {
					return err
				}
			}
			if kind != 15 {
				projection, presence, err = releasetransition.NormalizeFrozenRecord(kind, raw)
				if err != nil {
					header[6] = 1
					projection = nil
					presence = 0
				}
			}
			if len(projection) > MaxReply {
				return errors.New("native reply bound")
			}
			header[7] = presence
			binary.BigEndian.PutUint32(header[48:], uint32(len(projection)))
			if _, err = writeOutput(input, output, header); err != nil {
				return err
			}
			_, err = writeOutput(input, output, projection)
			return err
		}()
		if err != nil {
			return err
		}
	}
	return errors.New("native sequence exhausted")
}

func nativeFrameKind(kind byte) bool {
	switch kind {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21:
		return true
	}
	return false
}

type IngressBinding struct {
	Transaction string                        `json:"transaction"`
	Plan        string                        `json:"plan"`
	Releases    releasetransition.ReleasePair `json:"releases"`
	Steps       []IngressBoundStep            `json:"steps"`
}
type IngressBoundStep struct {
	Kind       string `json:"kind"`
	Checkpoint string `json:"checkpoint"`
	Expected   string `json:"expected"`
	Desired    string `json:"desired"`
	Static     string `json:"staticPin"`
}
type IngressStep struct {
	Binding  IngressBinding `json:"binding"`
	Kind     string         `json:"kind"`
	Expected string         `json:"expected"`
	Desired  string         `json:"desired"`
	Static   string         `json:"staticPin"`
}
type IngressOperation struct {
	Kind     string `json:"kind"`
	Decision string `json:"decision"`
	Expected string `json:"expected"`
	Desired  string `json:"desired"`
	Static   string `json:"staticPin"`
}
type IngressFacts struct {
	Yards      []string                             `json:"yards"`
	Operations []IngressOperation                   `json:"operations"`
	Decisions  []releasetransition.RedactedDecision `json:"decisions"`
	Blockers   []releasetransition.Blocker          `json:"blockers"`
}
