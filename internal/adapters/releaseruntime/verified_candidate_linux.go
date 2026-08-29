//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"golang.org/x/sys/unix"
)

type verifiedPublishedCandidate struct {
	candidate      publishedCandidate
	root           *os.File
	engine         *os.File
	manifestDigest releasetransition.Fingerprint
	registryDigest releasetransition.Fingerprint
	version        string
}

func (candidate *verifiedPublishedCandidate) Close() error {
	if candidate == nil {
		return nil
	}
	var result error
	if candidate.engine != nil {
		result = candidate.engine.Close()
	}
	if candidate.root != nil {
		result = errors.Join(result, candidate.root.Close())
	}
	return result
}

type candidateManifestEntry struct {
	path   string
	digest [sha256.Size]byte
}

func (runtime *Runtime) verifyPublishedCandidate(
	ctx context.Context,
	candidate publishedCandidate,
	runtimeRoot string,
	expectedDigest *releasetransition.Fingerprint,
) (*verifiedPublishedCandidate, error) {
	root, err := openVerifiedCandidateRoot(candidate, runtimeRoot)
	if err != nil {
		return nil, errors.New("published runtime directory is unavailable")
	}
	verified := &verifiedPublishedCandidate{
		candidate: candidate,
		root:      root,
	}
	fail := func(err error) (*verifiedPublishedCandidate, error) {
		verified.Close()
		return nil, err
	}
	rootFD := int(verified.root.Fd())
	manifest, err := openCandidateFile(rootFD, "runtime-files.sha256")
	if err != nil {
		return fail(errors.New("published runtime file manifest is unavailable"))
	}
	payload, err := io.ReadAll(io.LimitReader(manifest, 1<<20+1))
	closeErr := manifest.Close()
	if err != nil || closeErr != nil || len(payload) == 0 || len(payload) > 1<<20 {
		return fail(errors.New("published runtime file manifest is invalid"))
	}
	manifestDigest := sha256.Sum256(payload)
	verified.manifestDigest = releasetransition.Fingerprint(hex.EncodeToString(manifestDigest[:]))
	if expectedDigest != nil && verified.manifestDigest != *expectedDigest {
		return fail(errors.New("published runtime manifest does not match the protected transition"))
	}
	entries, err := parseCandidateManifest(payload)
	if err != nil {
		return fail(err)
	}
	for _, entry := range entries {
		file, openErr := openCandidateFile(rootFD, entry.path)
		if openErr != nil {
			return fail(errors.New("published runtime contains an unavailable manifest entry"))
		}
		if entry.path == "bin/yard-engine" {
			info, statErr := file.Stat()
			if statErr != nil || info.Mode().Perm()&0o111 == 0 {
				file.Close()
				return fail(errors.New("published runtime engine is unavailable"))
			}
			verified.engine, err = sealVerifiedEngine(file, entry.digest)
		} else {
			err = verifyCandidateFile(file, entry.digest)
			if err == nil && entry.path == "config/release-transition.json" {
				verified.registryDigest = releasetransition.Fingerprint(
					hex.EncodeToString(entry.digest[:]),
				)
			}
		}
		file.Close()
		if err != nil {
			return fail(err)
		}
	}
	if verified.engine == nil {
		return fail(errors.New("published runtime manifest does not bind yard-engine"))
	}
	version, err := runtime.runVerifiedCandidateVersion(ctx, verified, runtimeRoot)
	if err != nil {
		return fail(err)
	}
	verified.version = version
	return verified, nil
}

func openVerifiedCandidateRoot(
	candidate publishedCandidate,
	runtimeRoot string,
) (*os.File, error) {
	cleanRoot := filepath.Clean(runtimeRoot)
	release := string(candidate.release)
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) || cleanRoot == string(filepath.Separator) ||
		!domain.SafeID(release) || candidate.root != filepath.Join(cleanRoot, "releases", release) {
		return nil, errors.New("invalid published runtime directory")
	}
	components := strings.Split(strings.TrimPrefix(candidate.root, string(filepath.Separator)),
		string(filepath.Separator))
	current, err := unix.Open(
		string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		return nil, err
	}
	operatorUID := uint32(os.Getuid())
	privateAncestor := false
	for index, component := range components {
		next, openErr := unix.Openat(
			current, component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0,
		)
		unix.Close(current)
		if openErr != nil {
			return nil, openErr
		}
		current = next
		var stat unix.Stat_t
		if statErr := unix.Fstat(current, &stat); statErr != nil {
			unix.Close(current)
			return nil, statErr
		}
		final := index == len(components)-1
		permissions := os.FileMode(stat.Mode).Perm()
		stickySystemAncestor := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0 && !final
		containedOperatorAncestor := stat.Uid == operatorUID && privateAncestor && !final
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != 0 && stat.Uid != operatorUID ||
			permissions&0o022 != 0 && !stickySystemAncestor && !containedOperatorAncestor ||
			final && stat.Uid != operatorUID {
			unix.Close(current)
			return nil, errors.New("unsafe published runtime directory")
		}
		if stat.Uid == operatorUID && permissions&0o077 == 0 {
			privateAncestor = true
		}
		if final {
			return os.NewFile(uintptr(current), "verified-release-root"), nil
		}
	}
	unix.Close(current)
	return nil, errors.New("invalid published runtime directory")
}

func parseCandidateManifest(payload []byte) ([]candidateManifestEntry, error) {
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		return nil, errors.New("published runtime file manifest is invalid")
	}
	lines := bytes.Split(payload[:len(payload)-1], []byte{'\n'})
	entries := make([]candidateManifestEntry, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		if len(line) < sha256.Size*2+4 || string(line[sha256.Size*2:sha256.Size*2+4]) != "  ./" {
			return nil, errors.New("published runtime file manifest is invalid")
		}
		path := string(line[sha256.Size*2+4:])
		if !safeCandidateRelativePath(path) {
			return nil, errors.New("published runtime file manifest has an unsafe entry")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, errors.New("published runtime file manifest has a duplicate entry")
		}
		seen[path] = struct{}{}
		decoded, decodeErr := hex.DecodeString(string(line[:sha256.Size*2]))
		if decodeErr != nil || len(decoded) != sha256.Size {
			return nil, errors.New("published runtime file manifest is invalid")
		}
		var digest [sha256.Size]byte
		copy(digest[:], decoded)
		entries = append(entries, candidateManifestEntry{path: path, digest: digest})
	}
	return entries, nil
}

func safeCandidateRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "\\") {
		return false
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func openCandidateFile(rootFD int, path string) (*os.File, error) {
	if !safeCandidateRelativePath(path) {
		return nil, errors.New("unsafe candidate path")
	}
	current, err := unix.Dup(rootFD)
	if err != nil {
		return nil, err
	}
	components := strings.Split(path, "/")
	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if index != len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(current, component, flags, 0)
		unix.Close(current)
		if openErr != nil {
			return nil, openErr
		}
		current = next
	}
	file := os.NewFile(uintptr(current), "verified-release-file")
	info, err := file.Stat()
	if err != nil || !safeCandidateFileInfo(info) {
		file.Close()
		return nil, errors.New("candidate manifest entry is not a regular file")
	}
	return file, nil
}

func safeCandidateFileInfo(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uint32(os.Getuid())) && stat.Nlink == 1
}

func verifyCandidateFile(file *os.File, expected [sha256.Size]byte) error {
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return errors.New("read published runtime manifest entry")
	}
	if !bytes.Equal(digest.Sum(nil), expected[:]) {
		return errors.New("published runtime files do not match their manifest")
	}
	return nil
}

func sealVerifiedEngine(file *os.File, expected [sha256.Size]byte) (*os.File, error) {
	fd, err := createExecutableMemfd(unix.MemfdCreate)
	if err != nil {
		return nil, errors.New("create verified runtime execution image")
	}
	sealed := os.NewFile(uintptr(fd), "verified-yard-engine")
	fail := func(err error) (*os.File, error) {
		sealed.Close()
		return nil, err
	}
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(digest, sealed), file); err != nil {
		return fail(errors.New("copy verified runtime engine"))
	}
	if !bytes.Equal(digest.Sum(nil), expected[:]) {
		return fail(errors.New("published runtime files do not match their manifest"))
	}
	if err := sealed.Chmod(0o500); err != nil {
		return fail(errors.New("make verified runtime execution image executable"))
	}
	if _, err := unix.FcntlInt(sealed.Fd(), unix.F_ADD_SEALS,
		unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL,
	); err != nil {
		return fail(errors.New("seal verified runtime execution image"))
	}
	if _, err := sealed.Seek(0, io.SeekStart); err != nil {
		return fail(errors.New("rewind verified runtime execution image"))
	}
	return sealed, nil
}

func createExecutableMemfd(
	create func(string, int) (int, error),
) (int, error) {
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_EXEC
	fd, err := create("subyard-verified-yard-engine", flags)
	if errors.Is(err, unix.EINVAL) {
		return create(
			"subyard-verified-yard-engine",
			unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING,
		)
	}
	return fd, err
}

func (runtime *Runtime) runVerifiedCandidateVersion(
	ctx context.Context,
	candidate *verifiedPublishedCandidate,
	runtimeRoot string,
) (string, error) {
	var output bytes.Buffer
	if err := runtime.runVerifiedCandidate(
		ctx, candidate, runtimeRoot, []string{"--version"}, nil, &output, "",
	); err != nil {
		return "", errors.New("published runtime self-check failed")
	}
	fields := strings.Fields(output.String())
	if len(fields) != 2 || fields[0] != "yard-engine" || !safeVersion(fields[1]) {
		return "", errors.New("published runtime returned an invalid version")
	}
	return fields[1], nil
}

func (runtime *Runtime) runVerifiedCandidate(
	ctx context.Context,
	candidate *verifiedPublishedCandidate,
	runtimeRoot string,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	grant releasetransition.Authorization,
) error {
	if candidate == nil || candidate.root == nil || candidate.engine == nil {
		return errors.New("verified published runtime is unavailable")
	}
	extra := make([]*os.File, 0, 3)
	var grantReader *os.File
	if grant != "" {
		reader, writer, err := os.Pipe()
		if err != nil {
			return err
		}
		if _, err = io.WriteString(writer, string(grant)+"\n"); err != nil {
			reader.Close()
			writer.Close()
			return err
		}
		if err = writer.Close(); err != nil {
			reader.Close()
			return err
		}
		grantReader = reader
		defer grantReader.Close()
		extra = append(extra, grantReader)
	} else {
		// Keep the engine on the same child descriptor with or without a grant.
		// The repository uses the updater-owned pin below so its path remains
		// stable across the separate inspection and convergence processes.
		placeholder, err := os.Open(os.DevNull)
		if err != nil {
			return err
		}
		defer placeholder.Close()
		extra = append(extra, placeholder)
	}
	rootChildFD := 3 + len(extra)
	extra = append(extra, candidate.root)
	engineChildFD := 3 + len(extra)
	extra = append(extra, candidate.engine)
	enginePath := fmt.Sprintf("/proc/self/fd/%d", engineChildFD)
	command := exec.CommandContext(ctx, enginePath, arguments...)
	command.Args[0] = "yard-engine"
	command.ExtraFiles = extra
	repositoryRoot := fmt.Sprintf("/proc/self/fd/%d", rootChildFD)
	if runtime.pinnedCandidateRoot != nil &&
		runtime.pinnedCandidateRelease == candidate.candidate.release {
		repositoryRoot = fmt.Sprintf(
			"/proc/%d/fd/%d", os.Getpid(), runtime.pinnedCandidateRoot.Fd(),
		)
	}
	command.Env = releaseMigrationEnvironment(
		runtime.config.Environment, repositoryRoot, runtimeRoot,
	)
	if grant != "" {
		command.Env = append(command.Env, "SUBYARD_RELEASE_TRANSITION_GRANT_FD=3")
	}
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, runtime.config.Stderr
	return command.Run()
}

func (runtime *Runtime) invokeVerifiedCandidateTransition(
	ctx context.Context,
	candidate *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest,
	grant releasetransition.Authorization,
) (releasetransition.ProcessResponse, error) {
	if candidate == nil || candidate.registryDigest == "" {
		return releasetransition.ProcessResponse{},
			errors.New("published runtime manifest does not bind release transition registry")
	}
	if err := runtime.pinCandidateRoot(candidate); err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	if request.RegistryDigest != "" && request.RegistryDigest != candidate.registryDigest {
		return releasetransition.ProcessResponse{},
			errors.New("published runtime registry changed after inspection")
	}
	request.RegistryDigest = candidate.registryDigest
	payload, err := json.Marshal(request)
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	var stdout boundedResponseBuffer
	if err := runtime.runVerifiedCandidate(
		ctx, candidate, request.RuntimeRoot, []string{"_release-transition"},
		bytes.NewReader(append(payload, '\n')), &stdout, grant,
	); err != nil {
		return releasetransition.ProcessResponse{},
			fmt.Errorf("candidate release transition failed: %w", err)
	}
	if stdout.Len() > maximumCandidateResponseBytes {
		return releasetransition.ProcessResponse{}, errors.New("candidate release transition response is too large")
	}
	var response releasetransition.ProcessResponse
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil ||
		response.SchemaVersion != releasetransition.ProcessProtocolSchemaV1 {
		return releasetransition.ProcessResponse{},
			errors.New("candidate returned an invalid release transition response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return releasetransition.ProcessResponse{},
			errors.New("candidate returned an invalid release transition response")
	}
	return response, nil
}

const maximumCandidateResponseBytes = 1 << 20

type boundedResponseBuffer struct{ bytes.Buffer }

func (buffer *boundedResponseBuffer) Write(payload []byte) (int, error) {
	written := len(payload)
	remaining := maximumCandidateResponseBytes + 1 - buffer.Len()
	if remaining > len(payload) {
		remaining = len(payload)
	}
	if remaining > 0 {
		_, _ = buffer.Buffer.Write(payload[:remaining])
	}
	return written, nil
}

func (runtime *Runtime) pinCandidateRoot(candidate *verifiedPublishedCandidate) error {
	if candidate == nil || candidate.root == nil {
		return errors.New("verified published runtime is unavailable")
	}
	if runtime.pinnedCandidateRoot != nil {
		if runtime.pinnedCandidateRelease == candidate.candidate.release {
			pinned, pinnedErr := runtime.pinnedCandidateRoot.Stat()
			current, currentErr := candidate.root.Stat()
			if pinnedErr != nil || currentErr != nil || !os.SameFile(pinned, current) {
				return errors.New("published runtime directory changed after inspection")
			}
			return nil
		}
		if err := runtime.pinnedCandidateRoot.Close(); err != nil {
			return err
		}
		runtime.pinnedCandidateRoot = nil
		runtime.pinnedCandidateRelease = ""
	}
	descriptor, err := unix.Dup(int(candidate.root.Fd()))
	if err != nil {
		return errors.New("pin published runtime directory")
	}
	unix.CloseOnExec(descriptor)
	runtime.pinnedCandidateRoot = os.NewFile(uintptr(descriptor), "pinned-release-root")
	runtime.pinnedCandidateRelease = candidate.candidate.release
	return nil
}
