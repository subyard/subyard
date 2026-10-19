//go:build linux

package releaseruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

const (
	rustOwnerPrepareLifetime = 5 * time.Minute
	rustOwnerStartupLimit    = 5 * time.Second
	rustOwnerExchangeLimit   = 76 * time.Minute
	maximumRustOwnerReply    = 8 << 20
)

var errRustOwnerBarrierAbsent = errors.New("release execution requires its already-held owner barrier")

type rustCustodyContext struct {
	Session  string                `json:"session"`
	DataRoot releaseowner.Identity `json:"dataRoot"`
}

type rustOwnerReply struct {
	ActivationReconciliationOwned bool                                    `json:"activationReconciliationOwned"`
	Capabilities                  *releasetransition.RecoveryCapabilities `json:"capabilities,omitempty"`
	ActivationOwners              []string                                `json:"activationOwners,omitempty"`
	Custody                       *rustCustodyContext                     `json:"custody,omitempty"`
	Admission                     *rustResumeAdmission                    `json:"admission,omitempty"`
	RetainedReleases              []releasetransition.ReleaseID           `json:"retainedReleases"`
	Inspection                    *releasetransition.Inspection           `json:"inspection,omitempty"`
	Outcome                       *releasetransition.Outcome              `json:"outcome,omitempty"`
	Error                         string                                  `json:"error,omitempty"`
}

type rustResumeAdmission struct {
	Plan        releasetransition.PlanToken     `json:"plan"`
	Transaction releasetransition.TransactionID `json:"transaction"`
}

// One process owns one preparation. The request that created it may finish;
// explicit Close, expiry, or consumed execution owns its remaining lifetime.
type rustOwnerSession struct {
	revalidate       func(context.Context) error
	activationOwned  bool
	mu               sync.Mutex
	prepare          sync.Mutex
	wire             sync.Mutex
	command          *exec.Cmd
	connection       *net.UnixConn
	custody          *net.UnixConn
	reader           *bufio.Reader
	done             chan struct{}
	exitErr          error
	closed           bool
	used             bool
	armed            bool
	expires          time.Time
	timer            *time.Timer
	grant            releasetransition.Authorization
	scope            rustCustodyContext
	inspection       *releasetransition.Inspection
	admission        *rustResumeAdmission
	retainedReleases []releasetransition.ReleaseID
	request          releasetransition.ProcessRequest
	options          rustOwnerOptions
	root             *os.File
	source           *verifiedPublishedCandidate
	layout           config.PersistentFileSnapshot
}

func socketPair() (*net.UnixConn, *os.File, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(pair[0]), "release-owner-client")
	child := os.NewFile(uintptr(pair[1]), "release-owner-operation")
	connection, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		child.Close()
		return nil, nil, err
	}
	return connection.(*net.UnixConn), child, nil
}

func duplicateReleaseFile(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "retained-release-asset"), nil
}

func (runtime *Runtime) startRustOwner(ctx context.Context, assets, engine, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, grant releasetransition.Authorization) (result *rustOwnerSession, resultErr error) {
	defer func() {
		if engine != nil && engine.releaseOwner != nil {
			resultErr = markSelectedRustOwnerError(resultErr)
		}
	}()
	if assets == nil || assets.root == nil || engine == nil || engine.releaseOwner == nil || engine.engine == nil ||
		target == nil || target.manifestDigest != request.ArtifactDigest || target.candidate.release != request.Target ||
		assets.registryDigest == "" || assets.registryDigest != engine.registryDigest ||
		request.RegistryDigest != "" && request.RegistryDigest != assets.registryDigest {
		return nil, errors.New("Rust owner and exact helper/assets do not match the selected transition")
	}
	if assets.manifestDigest != engine.manifestDigest {
		return nil, errors.New("Rust release owner cannot adopt another runtime's transition assets; use its exact historical Go owner")
	}
	options, source, layout, err := runtime.rustOwnerFacts(ctx, assets, target, request)
	if err != nil {
		return nil, err
	}
	session := &rustOwnerSession{source: source, layout: layout, options: options, request: request,
		done: make(chan struct{}), grant: grant, expires: time.Now().Add(rustOwnerPrepareLifetime)}
	session.revalidate = func(ctx context.Context) error {
		// Rust verifies every captured artifact inventory and byte under the
		// update lease immediately before effects; the parent retains admission facts.
		if options.ConfigLayout != nil {
			fresh, err := config.ReadPersistentFileSnapshot(request.ConfigHome, filepath.Join(request.ConfigHome, ".sync/manifest.json"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if config.BindPersistentSnapshot(fresh) != config.BindPersistentSnapshot(layout) {
				return fmt.Errorf("%w: configuration layout changed before execution", domain.ErrPlanStale)
			}
		}
		return nil
	}
	fail := func(err error) (*rustOwnerSession, error) {
		session.Close()
		return nil, err
	}
	session.root, err = duplicateReleaseFile(assets.root)
	if err != nil {
		return fail(err)
	}
	connection, child, err := socketPair()
	if err != nil {
		return fail(err)
	}
	session.connection = connection
	defer child.Close()
	custody, custodyChild, err := socketPair()
	if err != nil {
		return fail(err)
	}
	session.custody = custody
	defer custodyChild.Close()
	grantRead, grantWrite, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	defer grantRead.Close()
	if grant == "" {
		grant, err = newReleaseTransitionGrant()
		if err != nil {
			grantWrite.Close()
			return fail(err)
		}
		session.grant = grant
	}
	_, writeErr := io.WriteString(grantWrite, string(grant))
	closeErr := grantWrite.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fail(err)
	}
	// The repository descriptor is a separate typed startup role. Logical paths
	// remain stable across processes, including before a draft is published.
	command := exec.Command("/proc/self/fd/3", "_release-owner-operation-v1")
	command.ExtraFiles = []*os.File{engine.releaseOwner}
	command.Stdin, command.Stdout = child, child
	command.Stderr = runtime.config.Stderr
	command.Env = releaseMigrationEnvironment(runtime.config.Inputs, runtime.config.Context, "", request.RuntimeRoot)
	if err := command.Start(); err != nil {
		return fail(err)
	}
	session.command = command
	go func() {
		err := command.Wait()
		session.mu.Lock()
		session.exitErr = err
		session.mu.Unlock()
		close(session.done)
	}()
	options.RepositoryRoot = filepath.Join(request.RuntimeRoot, "releases", string(assets.candidate.release))
	session.options = options
	deadline := time.Now().Add(rustOwnerStartupLimit)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := session.connection.SetDeadline(deadline); err != nil {
		return fail(err)
	}
	control, err := session.connection.SyscallConn()
	if err != nil {
		return fail(err)
	}
	var sendErr error
	if err := control.Control(func(fd uintptr) {
		sendErr = unix.Sendmsg(int(fd), []byte{'C'}, unix.UnixRights(int(engine.engine.Fd()), int(grantRead.Fd()), int(custodyChild.Fd()), int(session.root.Fd())), nil, unix.MSG_NOSIGNAL)
	}); err != nil || sendErr != nil {
		return fail(errors.Join(err, sendErr))
	}
	session.reader = bufio.NewReader(session.connection)
	reply, err := session.exchange(ctx, map[string]any{"op": "open", "options": options}, rustOwnerStartupLimit)
	if err != nil {
		return fail(err)
	}
	if reply.Custody == nil || !lowerSHA256(reply.Custody.Session) || reply.Inspection != nil || reply.Outcome != nil || reply.Admission != nil || reply.RetainedReleases != nil {
		return fail(errors.New("Rust owner returned an invalid opened scope"))
	}
	session.scope = *reply.Custody
	session.timer = time.AfterFunc(time.Until(session.expires), func() {
		session.mu.Lock()
		unused := !session.used
		session.mu.Unlock()
		if unused {
			session.Close()
		}
	})
	return session, nil
}

func (session *rustOwnerSession) exchange(ctx context.Context, request any, limit time.Duration) (rustOwnerReply, error) {
	session.wire.Lock()
	defer session.wire.Unlock()
	deadline := time.Now().Add(limit)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := session.connection.SetDeadline(deadline); err != nil {
		return rustOwnerReply{}, err
	}
	stop := context.AfterFunc(ctx, func() { session.connection.SetDeadline(time.Now()) })
	defer stop()
	if err := json.NewEncoder(session.connection).Encode(request); err != nil {
		return rustOwnerReply{}, err
	}
	return session.readReply()
}

func (session *rustOwnerSession) readReply() (rustOwnerReply, error) {
	var reply rustOwnerReply
	var raw []byte
	for {
		part, err := session.reader.ReadSlice('\n')
		if len(raw)+len(part) > maximumRustOwnerReply {
			return reply, errors.New("Rust owner response exceeds its bound")
		}
		raw = append(raw, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return reply, errors.New("Rust owner response is unavailable")
		}
		break
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return reply, errors.New("Rust owner returned an invalid response")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return reply, errors.New("Rust owner returned trailing response data")
	}
	if reply.Error != "" {
		return reply, fmt.Errorf("Rust release owner refused: %s", reply.Error)
	}
	return reply, nil
}

func (session *rustOwnerSession) Inspect(ctx context.Context, recovery *releasetransition.RecoveryProcessRequest) (releasetransition.Inspection, error) {
	session.prepare.Lock()
	defer session.prepare.Unlock()
	session.mu.Lock()
	if session.closed || session.used || !time.Now().Before(session.expires) {
		session.mu.Unlock()
		return releasetransition.Inspection{}, domain.ErrPlanStale
	}
	if session.inspection != nil {
		inspection := cloneRustInspection(*session.inspection)
		session.mu.Unlock()
		return inspection, nil
	}
	session.mu.Unlock()
	request := map[string]any{"op": "inspect", "goal": releasetransition.Goal{Target: session.request.Target, Direction: session.request.Direction}}
	if recovery != nil {
		if recovery.MigrationOwner != nil {
			request = map[string]any{"op": "inspect-migration-owner", "replacement": recovery.MigrationOwner}
		} else {
			request = map[string]any{"op": "inspect-recovery", "replacement": recovery.Recovery, "contract": recovery.Contract}
		}
	}
	reply, err := session.exchange(ctx, request, rustOwnerExchangeLimit)
	if err != nil {
		return releasetransition.Inspection{}, err
	}
	if reply.Inspection == nil || reply.Outcome != nil || reply.Custody != nil {
		return releasetransition.Inspection{}, errors.New("Rust owner returned an invalid preparation")
	}
	goal := releasetransition.Goal{Target: session.request.Target, Direction: session.request.Direction}
	if err := releasetransition.ValidateProcessInspection(goal, *reply.Inspection); err != nil {
		return releasetransition.Inspection{}, err
	}
	if err := validateRustRetainedReleases(reply.RetainedReleases); err != nil {
		return releasetransition.Inspection{}, err
	}
	if reply.Admission != nil {
		digest, valid := strings.CutPrefix(string(reply.Admission.Plan), "resume-v1-")
		if len(reply.RetainedReleases) == 0 || !valid || !lowerSHA256(digest) ||
			len(reply.Admission.Transaction) > 128 || !domain.SafeID(string(reply.Admission.Transaction)) ||
			reply.Inspection.Plan != reply.Admission.Plan || reply.Inspection.Resume == nil ||
			*reply.Inspection.Resume != reply.Admission.Transaction || reply.Inspection.Outcome == nil ||
			reply.Inspection.Outcome.Transaction == nil || *reply.Inspection.Outcome.Transaction != reply.Admission.Transaction {
			return releasetransition.Inspection{}, errors.New("Rust owner returned an invalid durable admission ticket")
		}
	}
	session.mu.Lock()
	session.inspection = reply.Inspection
	session.admission = reply.Admission
	session.retainedReleases = slices.Clone(reply.RetainedReleases)
	session.activationOwned = reply.ActivationReconciliationOwned
	session.mu.Unlock()
	return cloneRustInspection(*reply.Inspection), nil
}

// Rust supplies checked retention references; Go neither derives eligibility
// from journal IDs nor treats a missing protocol field as an empty GC root set.
func validateRustRetainedReleases(releases []releasetransition.ReleaseID) error {
	if releases == nil || len(releases) > 3 || !slices.IsSorted(releases) {
		return errors.New("Rust owner returned invalid retained release references")
	}
	for index, release := range releases {
		if len(release) > 128 || !domain.SafeID(string(release)) || index > 0 && releases[index-1] == release {
			return errors.New("Rust owner returned invalid retained release references")
		}
	}
	return nil
}

func (session *rustOwnerSession) matchesRustAdmission(inspection releasetransition.Inspection) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return !session.closed && !session.used && session.admission != nil && len(session.retainedReleases) > 0 &&
		inspection.Resume != nil && *inspection.Resume == session.admission.Transaction && inspection.Plan == session.admission.Plan
}

func cloneRustInspection(inspection releasetransition.Inspection) releasetransition.Inspection {
	// The frozen DTO contains only JSON values; keep callers from mutating the
	// original consent scope retained by the owner session.
	raw, _ := json.Marshal(inspection)
	var copy releasetransition.Inspection
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func borrowRustOwnerBarrier(dataHome, inherited string) (*os.File, error) {
	var file *os.File
	if inherited != "" {
		fd, err := strconv.Atoi(inherited)
		if err != nil || fd < 3 {
			return nil, errors.New("invalid inherited release owner barrier")
		}
		copy, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		file = os.NewFile(uintptr(copy), "inherited-release-owner-barrier")
	} else {
		var err error
		file, err = uninstall.BorrowOwnerLock(dataHome)
		if err != nil {
			return nil, err
		}
	}
	if file == nil {
		return nil, errRustOwnerBarrierAbsent
	}
	current, statErr := os.Lstat(uninstall.OwnerLockPath(dataHome))
	info, fileErr := file.Stat()
	identity, identityErr := releaseowner.IdentityOf(file)
	if statErr != nil || fileErr != nil || identityErr != nil || !os.SameFile(current, info) ||
		identity.UID != uint32(os.Getuid()) || identity.Mode&unix.S_IFMT != unix.S_IFREG ||
		identity.Mode&0o777 != 0o600 || identity.Links != 1 {
		file.Close()
		return nil, errors.New("release owner barrier no longer matches its protected installation")
	}
	return file, nil
}

func (session *rustOwnerSession) Execute(ctx context.Context, plan releasetransition.PlanToken, inheritedBarrier string) (releasetransition.Outcome, error) {
	session.mu.Lock()
	if session.closed || session.used || session.inspection == nil || session.inspection.Plan != plan || !time.Now().Before(session.expires) {
		session.mu.Unlock()
		return releasetransition.Outcome{}, domain.ErrPlanStale
	}
	session.used = true
	session.timer.Stop()
	inspection := *session.inspection
	admission := session.admission
	session.mu.Unlock()
	if session.revalidate != nil {
		if err := session.revalidate(ctx); err != nil {
			return releasetransition.Outcome{}, err
		}
	}
	needsCustody := admission != nil || inspection.Assessment.Changed || inspection.Resume != nil
	var barrier *os.File
	if needsCustody {
		var err error
		barrier, err = borrowRustOwnerBarrier(session.options.DataHome, inheritedBarrier)
		if err != nil {
			return releasetransition.Outcome{}, err
		}
		defer barrier.Close()
	}
	session.wire.Lock()
	defer session.wire.Unlock()
	deadline := time.Now().Add(rustOwnerExchangeLimit)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := session.connection.SetDeadline(deadline); err != nil {
		return releasetransition.Outcome{}, err
	}
	if err := session.custody.SetDeadline(time.Now().Add(rustOwnerStartupLimit)); err != nil {
		return releasetransition.Outcome{}, err
	}
	stop := context.AfterFunc(ctx, func() { session.Close() })
	defer stop()
	session.mu.Lock()
	session.armed = needsCustody
	session.mu.Unlock()
	transaction := ""
	request := map[string]any{"op": "execute", "plan": plan, "authorization": session.grant}
	admissionPlan := plan
	if admission != nil {
		request = map[string]any{"op": "resume"}
		admissionPlan, transaction = admission.Plan, string(admission.Transaction)
	}
	if admission == nil && inspection.Resume != nil {
		transaction = string(*inspection.Resume)
	}
	if err := json.NewEncoder(session.connection).Encode(request); err != nil {
		return releasetransition.Outcome{}, err
	}
	if needsCustody {
		if err := session.admit(barrier, admissionPlan, transaction); err != nil {
			session.Close()
			return releasetransition.Outcome{}, err
		}
	}
	reply, err := session.readReply()
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return releasetransition.Outcome{}, err
	}
	if reply.Outcome == nil || reply.Inspection != nil || reply.Custody != nil || reply.Admission != nil {
		return releasetransition.Outcome{}, errors.New("Rust owner returned an invalid execution outcome")
	}
	goal := releasetransition.Goal{Target: session.request.Target, Direction: session.request.Direction}
	if err := releasetransition.ValidateProcessConvergence(goal, inspection, *reply.Outcome); err != nil {
		return releasetransition.Outcome{}, err
	}
	if err := validateRustRetainedReleases(reply.RetainedReleases); err != nil {
		return releasetransition.Outcome{}, err
	}
	session.mu.Lock()
	session.retainedReleases = slices.Clone(reply.RetainedReleases)
	session.mu.Unlock()
	return *reply.Outcome, nil
}

func (session *rustOwnerSession) admit(barrier *os.File, plan releasetransition.PlanToken, transaction string) error {
	identity, err := releaseowner.IdentityOf(barrier)
	if err != nil {
		return err
	}
	message := struct {
		Version     int                         `json:"version"`
		Session     string                      `json:"session"`
		Plan        releasetransition.PlanToken `json:"plan"`
		Transaction string                      `json:"transaction"`
		DataRoot    releaseowner.Identity       `json:"dataRoot"`
		Lock        releaseowner.Identity       `json:"lock"`
	}{1, session.scope.Session, plan, transaction, session.scope.DataRoot, identity}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	oob := unix.UnixRights(int(barrier.Fd()))
	n, control, err := session.custody.WriteMsgUnix([]byte{'A'}, oob, nil)
	if err != nil || n != 1 || control != len(oob) {
		return errors.New("release owner custody transfer failed")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err := io.Copy(session.custody, bytes.NewReader(append(header[:], raw...))); err != nil {
		return err
	}
	var ack struct {
		Version     int                         `json:"version"`
		Session     string                      `json:"session"`
		Plan        releasetransition.PlanToken `json:"plan"`
		Transaction string                      `json:"transaction"`
		Accepted    bool                        `json:"accepted"`
	}
	if err := json.NewDecoder(io.LimitReader(session.custody, 4096)).Decode(&ack); err != nil {
		return err
	}
	if ack.Version != 1 || !ack.Accepted || ack.Session != message.Session || ack.Plan != plan || ack.Transaction != transaction {
		return errors.New("release owner custody acknowledgement does not match its prepared scope")
	}
	return nil
}

func (session *rustOwnerSession) Close() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	if session.timer != nil {
		session.timer.Stop()
	}
	command, armed := session.command, session.armed
	session.mu.Unlock()
	if session.connection != nil {
		session.connection.Close()
	}
	if session.custody != nil {
		session.custody.Close()
	}
	if command != nil {
		select {
		case <-session.done:
		case <-time.After(time.Second):
			// Kill only the Rust coordinator. An armed native helper owns its own
			// process group/OFDs and must finish or persist UNKNOWN independently.
			_ = command.Process.Kill()
			<-session.done
		}
	}
	var err error
	if session.root != nil {
		err = session.root.Close()
	}
	if session.source != nil {
		err = errors.Join(err, session.source.Close())
	}
	for _, root := range session.options.originalRoots {
		err = errors.Join(err, root.Close())
	}
	if armed {
		// Close is resource cleanup, never proof that accepted native work drained.
		return err
	}
	return err
}
