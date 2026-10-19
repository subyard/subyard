//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"golang.org/x/sys/unix"
)

const sourceQueryRequestLimit = 64 << 10

// A frozen 1MiB original journal expands to about 1.4MiB as base64.
const sourceQueryReplyLimit = 2 << 20

func openSourceQueryRoot(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	fd, err := openApprovedDirectory(path, info)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "source-query-root"), nil
}

// A checkout has one fixed build output, made with the normal package stage.
// The query parent has no native callback authority. Its own test executable
// may differ; both query PROGRAM and CODEC images come from verified tooling.
func (runtime *Runtime) sourceQueryTooling(ctx context.Context) (*verifiedPublishedCandidate, func() error, error) {
	repository, err := filepath.EvalSymlinks(runtime.config.RepositoryRoot)
	if err != nil || !filepath.IsAbs(repository) {
		return nil, nil, errors.New("source query requires verified current tooling")
	}
	checkout := ""
	if filepath.Base(filepath.Dir(repository)) != "releases" {
		checkout = repository
		container := filepath.Join(repository, ".build", "runtime")
		stage, err := filepath.EvalSymlinks(filepath.Join(container, "current"))
		if err != nil || filepath.Dir(stage) != filepath.Join(container, "releases") {
			return nil, nil, errors.New("source query tooling is unavailable; run make build")
		}
		repository = stage
	}
	container := filepath.Dir(filepath.Dir(repository))
	verified, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{root: repository, release: releasetransition.ReleaseID(filepath.Base(repository))}, container, nil)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*verifiedPublishedCandidate, func() error, error) {
		verified.Close()
		return nil, nil, err
	}
	if verified.releaseOwner == nil || !strings.HasPrefix(string(verified.candidate.release), verified.version+"-") {
		return fail(errors.New("source query requires normal declared Rust tooling"))
	}
	rootInfo, err := verified.root.Stat()
	if err != nil {
		return fail(err)
	}
	rootIdentity, err := sourceInputInfo(rootInfo)
	if err != nil {
		return fail(err)
	}
	var engineInfo os.FileInfo
	check := func() error {
		named, err := os.Lstat(repository)
		if err != nil {
			return errors.New("source query tooling root changed")
		}
		namedIdentity, identityErr := sourceInputInfo(named)
		if identityErr != nil || rootIdentity != namedIdentity {
			return errors.New("source query tooling root changed")
		}
		if checkout == "" {
			return nil
		}
		current, err := filepath.EvalSymlinks(filepath.Join(checkout, ".build/runtime/current"))
		if err != nil || current != repository {
			return errors.New("source query tooling selection changed")
		}
		path := filepath.Join(checkout, ".build/yard")
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		image := os.NewFile(uintptr(fd), "source-build-helper")
		defer image.Close()
		before, err := image.Stat()
		if err != nil {
			return err
		}
		if !safeCandidateFileInfo(before) || before.Mode().Perm()&0111 == 0 {
			return errors.New("unsafe source build helper")
		}
		if engineInfo != nil && (!os.SameFile(engineInfo, before) || engineInfo.Mode() != before.Mode()) {
			return errors.New("source build helper changed")
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, image); err != nil {
			return err
		}
		after, err := image.Stat()
		if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() || before.Size() != after.Size() || hex.EncodeToString(hash.Sum(nil)) != verified.producerEvidence.EngineSHA256 {
			return errors.New("source build helper does not match normal tooling")
		}
		engineInfo = before
		return nil
	}
	if err := check(); err != nil {
		return fail(err)
	}
	return verified, check, nil
}

type rustSourceQuery struct {
	ConfigHome      []byte                `json:"configHome"`
	RuntimeRoot     []byte                `json:"runtimeRoot"`
	ConfigIdentity  releaseowner.Identity `json:"configIdentity"`
	RuntimeIdentity releaseowner.Identity `json:"runtimeIdentity"`
	Active          rustArtifactFact      `json:"active"`
	OriginalOwner   *rustArtifactFact     `json:"originalOwner"`
	OriginalSources *rustMigrationSources `json:"originalSources"`
}

type rustSourceProof struct {
	JournalRaw       []byte   `json:"journalRaw"`
	MetadataBinding  string   `json:"metadataBinding"`
	Binding          string   `json:"binding"`
	RetainedReleases []string `json:"retainedReleases"`
}

// The source mode is finite and read-only. It receives no grant, account
// barrier, update lease, operation options or native component factory.
func (runtime *Runtime) queryRustSource(ctx context.Context, tooling *verifiedPublishedCandidate, request rustSourceQuery, configRoot, runtimeRoot *os.File) (rustSourceProof, error) {
	var proof rustSourceProof
	if tooling == nil || tooling.releaseOwner == nil || tooling.engine == nil || configRoot == nil || runtimeRoot == nil {
		return proof, errors.New("verified Rust source tooling is unavailable")
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload)+1 > sourceQueryRequestLimit {
		return proof, errors.New("Rust source request exceeds its finite contract")
	}
	connection, child, err := socketPair()
	if err != nil {
		return proof, err
	}
	defer connection.Close()
	defer child.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/proc/self/fd/3", "_release-owner-source-v1")
	command.ExtraFiles = []*os.File{tooling.releaseOwner}
	command.Stdin, command.Stdout = child, child
	command.Stderr = runtime.config.Stderr
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return proof, err
	}
	// Only the child may keep its socket endpoint; retaining the parent's
	// duplicate would hide EOF after the finite query process exits.
	_ = child.Close()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = connection.SetDeadline(time.Now()) })
	defer stop()
	_, _, sendErr := connection.WriteMsgUnix([]byte{'S'}, unix.UnixRights(int(tooling.engine.Fd()), int(configRoot.Fd()), int(runtimeRoot.Fd())), nil)
	if sendErr == nil {
		_, sendErr = connection.Write(append(payload, '\n'))
	}
	if sendErr == nil {
		sendErr = connection.CloseWrite()
	}
	var raw []byte
	if sendErr == nil {
		raw, sendErr = io.ReadAll(io.LimitReader(connection, sourceQueryReplyLimit+1))
	}
	if sendErr != nil || len(raw) > sourceQueryReplyLimit {
		cancel()
		_ = command.Wait()
		return proof, errors.New("Rust source query transport refused")
	}
	if err := command.Wait(); err != nil {
		return proof, errors.New("Rust source query refused")
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 {
		return proof, errors.New("Rust source query response is not one finite frame")
	}
	// Enforce the exact response shape, including duplicate keys, before the
	// frozen journal codec sees any returned original bytes.
	if err := decodeClosedCandidateJSON(raw, &proof); err != nil {
		return rustSourceProof{}, err
	}
	if !lowerSHA256(proof.MetadataBinding) || !lowerSHA256(proof.Binding) || proof.RetainedReleases == nil {
		return rustSourceProof{}, errors.New("Rust source proof binding is invalid")
	}
	refs := make([]releasetransition.ReleaseID, len(proof.RetainedReleases))
	for i, ref := range proof.RetainedReleases {
		refs[i] = releasetransition.ReleaseID(ref)
	}
	if err := validateRustRetainedReleases(refs); err != nil {
		return rustSourceProof{}, err
	}
	return proof, nil
}
