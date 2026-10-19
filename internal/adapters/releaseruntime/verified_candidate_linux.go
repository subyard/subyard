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
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

type verifiedPublishedCandidate struct {
	commandManifestBound  bool
	commandManifestDigest releasetransition.Fingerprint
	projectCreatorDigest  [sha256.Size]byte
	producerEvidence      uninstall.ProducerEvidence
	candidate             publishedCandidate
	root                  *os.File
	engine                *os.File
	releaseOwner          *os.File
	manifestDigest        releasetransition.Fingerprint
	registryDigest        releasetransition.Fingerprint
	checkpointRaw         []byte
	checkpointDigest      [sha256.Size]byte
	version               string
}

func (candidate *verifiedPublishedCandidate) Close() error {
	if candidate == nil {
		return nil
	}
	var result error
	if candidate.engine != nil {
		result = candidate.engine.Close()
	}
	if candidate.releaseOwner != nil {
		result = errors.Join(result, candidate.releaseOwner.Close())
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

const (
	releaseOwnerDeclarationPath = "config/release-owner.json"
	releaseOwnerImagePath       = "bin/subyard-release-owner"
	releaseOwnerSchemaPath      = "config/release-owner.schema.json"
	releaseOwnerBuildPath       = "config/release-owner-build.json"
)

type releaseOwnerDeclaration struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Kind            string `json:"kind"`
	ProtocolVersion int    `json:"protocolVersion"`
	Owner           string `json:"owner"`
	Helper          string `json:"helper"`
	Schema          string `json:"schema"`
	BuildInputs     string `json:"buildInputs"`
}

func expectedReleaseOwnerDeclaration() releaseOwnerDeclaration {
	return releaseOwnerDeclaration{1, "rust-release-owner", 1, releaseOwnerImagePath,
		"bin/yard-engine", releaseOwnerSchemaPath, releaseOwnerBuildPath}
}

type releaseOwnerBuild struct {
	SchemaVersion   int      `json:"schemaVersion"`
	Kind            string   `json:"kind"`
	Compiler        string   `json:"compiler"`
	Cargo           string   `json:"cargo"`
	Target          string   `json:"target"`
	Profile         string   `json:"profile"`
	Features        []string `json:"features"`
	Locked          bool     `json:"locked"`
	StaticELF       bool     `json:"staticELF"`
	Codegen         []string `json:"codegen"`
	ImageSHA256     string   `json:"imageSHA256"`
	CargoLockSHA256 string   `json:"cargoLockSHA256"`
	Inputs          []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"inputs"`
}

// Declaration absence is the legacy format. A partially added owner is never legacy.
// These assets are checked before either executable's version probe can run.
func verifyReleaseOwnerDeclaration(rootFD int, entries []candidateManifestEntry) error {
	digests := make(map[string][sha256.Size]byte, len(entries))
	for _, entry := range entries {
		digests[entry.path] = entry.digest
	}
	_, declared := digests[releaseOwnerDeclarationPath]
	for _, path := range []string{releaseOwnerImagePath, releaseOwnerSchemaPath, releaseOwnerBuildPath} {
		_, present := digests[path]
		if present != declared {
			return errors.New("published runtime release owner assets are incomplete")
		}
	}
	if !declared {
		return nil
	}
	if _, present := digests["bin/yard-engine"]; !present {
		return errors.New("published runtime release owner helper is unbound")
	}
	readJSON := func(path string, value any) error {
		file, err := openCandidateFile(rootFD, path)
		if err != nil {
			return err
		}
		defer file.Close()
		payload, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
		if err != nil || len(payload) > 1<<20 || sha256.Sum256(payload) != digests[path] {
			return errors.New("published runtime release owner metadata changed")
		}
		return decodeClosedCandidateJSON(payload, value)
	}
	var declaration releaseOwnerDeclaration
	if err := readJSON(releaseOwnerDeclarationPath, &declaration); err != nil ||
		declaration != expectedReleaseOwnerDeclaration() {
		return errors.New("published runtime release owner declaration is invalid")
	}
	// The distributed schema describes this exact closed declaration, not a replacement
	// protocol or a request to load an arbitrary executable or JSON schema implementation.
	var schema any
	if err := readJSON(releaseOwnerSchemaPath, &schema); err != nil {
		return errors.New("published runtime release owner schema is invalid")
	}
	declarationPayload, _ := json.Marshal(declaration)
	var constants map[string]any
	_ = json.Unmarshal(declarationPayload, &constants)
	properties := make(map[string]any, len(constants))
	for key, value := range constants {
		properties[key] = map[string]any{"const": value}
	}
	expectedSchema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://subyard.dev/schemas/release-owner-v1.json", "type": "object",
		"additionalProperties": false,
		"required":             []string{"schemaVersion", "kind", "protocolVersion", "owner", "helper", "schema", "buildInputs"},
		"properties":           properties,
	}
	actualSchema, _ := json.Marshal(schema)
	wantedSchema, _ := json.Marshal(expectedSchema)
	if !bytes.Equal(actualSchema, wantedSchema) {
		return errors.New("published runtime release owner schema is unsupported")
	}
	var build releaseOwnerBuild
	if err := readJSON(releaseOwnerBuildPath, &build); err != nil {
		return errors.New("published runtime release owner build inputs are invalid")
	}
	ownerDigest := digests[releaseOwnerImagePath]
	if build.SchemaVersion != 1 || build.Kind != "rust-release-owner-build" ||
		!strings.HasPrefix(build.Compiler, "rustc ") || !strings.Contains(build.Compiler, "\nhost: ") ||
		!strings.HasPrefix(build.Cargo, "cargo ") ||
		(build.Target != "x86_64-unknown-linux-musl" && build.Target != "aarch64-unknown-linux-musl") ||
		build.Profile != "release" || build.Features == nil || len(build.Features) != 0 ||
		!build.Locked || !build.StaticELF || build.ImageSHA256 != hex.EncodeToString(ownerDigest[:]) ||
		strings.Join(build.Codegen, "\n") != "linker=rust-lld\ntarget-feature=+crt-static\nremap-path-prefix=repository=." ||
		!lowerSHA256(build.CargoLockSHA256) || len(build.Inputs) < 6 {
		return errors.New("published runtime release owner build inputs are unsupported")
	}
	previous := ""
	inputs := map[string]string{}
	for _, input := range build.Inputs {
		if !safeCandidateRelativePath(input.Path) || input.Path <= previous || !lowerSHA256(input.SHA256) {
			return errors.New("published runtime release owner source inventory is invalid")
		}
		previous, inputs[input.Path] = input.Path, input.SHA256
	}
	for _, required := range []string{"release-owner/Cargo.toml", "release-owner/Cargo.lock", "rust-toolchain.toml",
		"dev/build-release-owner.sh", "release-owner/src/lib.rs", "release-owner/src/bin/subyard-release-owner.rs"} {
		if inputs[required] == "" {
			return errors.New("published runtime release owner source inventory is incomplete")
		}
	}
	if inputs["release-owner/Cargo.lock"] != build.CargoLockSHA256 {
		return errors.New("published runtime release owner lockfile is unbound")
	}
	return nil
}

func lowerSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// encoding/json otherwise accepts duplicate object keys. Reject them at every depth,
// then apply the typed closed schema and refuse trailing JSON values.
func decodeClosedCandidateJSON(payload []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var consume func() error
	consume = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON field")
				}
				seen[name] = true
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
		case json.Delim('['):
			for decoder.More() {
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
		}
		return err
	}
	if err := consume(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	decoder = json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	// Require the exact JSON field spelling and presence, including explicit empty
	// feature lists. Struct decoding alone treats field names case-insensitively.
	var original any
	if err := json.Unmarshal(payload, &original); err != nil {
		return err
	}
	before, _ := json.Marshal(original)
	encoded, err := json.Marshal(value)
	var normalized any
	if err == nil {
		err = json.Unmarshal(encoded, &normalized)
	}
	after, _ := json.Marshal(normalized)
	if err != nil || !bytes.Equal(before, after) {
		return errors.New("JSON fields do not match the closed schema")
	}
	return nil
}

func (runtime *Runtime) verifyPublishedCandidate(
	ctx context.Context,
	candidate publishedCandidate,
	runtimeRoot string,
	expectedDigest *releasetransition.Fingerprint,
) (*verifiedPublishedCandidate, error) {
	if relocated, ok := runtime.relocatedCandidates[candidate.root]; ok {
		candidate.root = relocated
	}
	root, err := openVerifiedCandidateRoot(candidate, runtimeRoot)
	if err != nil {
		return nil, errors.New("published runtime directory is unavailable")
	}
	verified := &verifiedPublishedCandidate{
		candidate: candidate,
		root:      root,
	}
	selectedRust := false
	fail := func(err error) (*verifiedPublishedCandidate, error) {
		verified.Close()
		if selectedRust {
			err = markSelectedRustOwnerError(err)
		}
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
	if err := verifyCandidateInventory(rootFD, entries); err != nil {
		return fail(err)
	}
	if err := verifyReleaseOwnerDeclaration(rootFD, entries); err != nil {
		return fail(err)
	}
	for _, entry := range entries {
		if entry.path == releaseOwnerDeclarationPath {
			selectedRust = true
		}
	}
	for _, entry := range entries {
		file, openErr := openCandidateFile(rootFD, entry.path)
		if openErr != nil {
			return fail(errors.New("published runtime contains an unavailable manifest entry"))
		}
		if entry.path == "bin/yard-engine" || entry.path == releaseOwnerImagePath {
			info, statErr := file.Stat()
			if statErr != nil || info.Mode().Perm()&0o111 == 0 {
				file.Close()
				return fail(errors.New("published runtime engine is unavailable"))
			}
			if entry.path == releaseOwnerImagePath {
				verified.releaseOwner, err = sealVerifiedEngine(file, entry.digest)
			} else {
				verified.producerEvidence.EngineSHA256 = hex.EncodeToString(entry.digest[:])
				verified.engine, err = sealVerifiedEngine(file, entry.digest)
			}
		} else {
			err = verifyCandidateFile(file, entry.digest)
			if err == nil && entry.path == "scripts/02-create-project.sh" {
				verified.projectCreatorDigest = entry.digest
			}
			if err == nil && entry.path == "config/commands.registry" {
				verified.commandManifestBound = true
				verified.commandManifestDigest = releasetransition.Fingerprint(hex.EncodeToString(entry.digest[:]))
			}
			if err == nil && (entry.path == "config/systemd/subyard-power-reconcile.service.in" ||
				entry.path == "config/systemd/subyard-keys-sync.service.in" || entry.path == "config/systemd/subyard-keys-sync.timer.in") {
				body, readErr := io.ReadAll(io.NewSectionReader(file, 0, 1<<20+1))
				if readErr != nil || len(body) > 1<<20 || sha256.Sum256(body) != entry.digest {
					err = errors.New("published runtime producer template changed")
				} else {
					if verified.producerEvidence.Templates == nil {
						verified.producerEvidence.Templates = map[string]string{}
					}
					verified.producerEvidence.Templates[filepath.Base(entry.path)] = string(body)
				}
			}
			if err == nil && entry.path == "config/release-transition.json" {
				verified.registryDigest = releasetransition.Fingerprint(
					hex.EncodeToString(entry.digest[:]),
				)
			}
			if err == nil && entry.path == "config/release-checkpoint.json" {
				verified.checkpointRaw, err = io.ReadAll(io.NewSectionReader(file, 0, releasetransition.MaxMigrationCheckpointCapabilityBytes+1))
				if err == nil && (len(verified.checkpointRaw) > releasetransition.MaxMigrationCheckpointCapabilityBytes || sha256.Sum256(verified.checkpointRaw) != entry.digest) {
					err = errors.New("published runtime checkpoint capability changed")
				}
				verified.checkpointDigest = entry.digest
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
	if verified.registryDigest == "" {
		registry, err := openCandidateFile(rootFD, "config/release-transition.json")
		if registry != nil {
			registry.Close()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fail(errors.New("published runtime registry is not bound by its manifest"))
		}
	}
	version, err := runtime.runVerifiedCandidateVersion(ctx, verified, runtimeRoot)
	if err != nil {
		return fail(err)
	}
	verified.version = version
	return verified, nil
}

func verifyCandidateInventory(rootFD int, entries []candidateManifestEntry) error {
	remaining := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		remaining[entry.path] = struct{}{}
	}
	var walk func(int, string, string) error
	walk = func(parentFD int, name, relative string) error {
		fd, err := unix.Openat(parentFD, name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return errors.New("published runtime inventory is unavailable")
		}
		directory := os.NewFile(uintptr(fd), "verified-release-directory")
		defer directory.Close()
		names, err := directory.Readdirnames(-1)
		if err != nil {
			return errors.New("published runtime inventory is unavailable")
		}
		for _, name := range names {
			path := filepath.Join(relative, name)
			var stat unix.Stat_t
			if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return errors.New("published runtime inventory is unavailable")
			}
			switch stat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				if err := walk(fd, name, path); err != nil {
					return err
				}
			case unix.S_IFREG:
				if path == "runtime-files.sha256" {
					continue
				}
				if _, listed := remaining[path]; !listed {
					return errors.New("published runtime file manifest is not exact")
				}
				delete(remaining, path)
			default:
				return errors.New("published runtime contains a non-regular entry")
			}
		}
		return nil
	}
	if err := walk(rootFD, ".", ""); err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("published runtime file manifest is not exact")
	}
	return nil
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
	return runtime.runVerifiedRuntimeEngine(
		ctx, candidate, candidate, runtimeRoot, arguments, stdin, stdout, grant,
	)
}

func (runtime *Runtime) runVerifiedRuntimeEngine(
	ctx context.Context,
	assets, engine *verifiedPublishedCandidate,
	runtimeRoot string,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	grant releasetransition.Authorization,
) error {
	if assets == nil || assets.root == nil || engine == nil || engine.engine == nil {
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
	extra = append(extra, assets.root)
	engineChildFD := 3 + len(extra)
	extra = append(extra, engine.engine)
	enginePath := fmt.Sprintf("/proc/self/fd/%d", engineChildFD)
	command := exec.CommandContext(ctx, enginePath, arguments...)
	command.Args[0] = "yard-engine"
	command.ExtraFiles = extra
	repositoryRoot := fmt.Sprintf("/proc/self/fd/%d", rootChildFD)
	if runtime.pinnedCandidateRoot != nil &&
		runtime.pinnedCandidateRelease == assets.candidate.release {
		repositoryRoot = fmt.Sprintf(
			"/proc/%d/fd/%d", os.Getpid(), runtime.pinnedCandidateRoot.Fd(),
		)
	}
	command.Env = releaseMigrationEnvironment(
		runtime.config.Inputs, runtime.config.Context, repositoryRoot, runtimeRoot,
	)
	if grant != "" {
		command.Env = append(command.Env, "SUBYARD_RELEASE_TRANSITION_GRANT_FD=3")
	}
	home := runtime.config.Inputs.value("SUBYARD_HOME", runtime.config.Context)
	if home == "" {
		home = filepath.Join(runtime.config.Inputs.value("HOME", runtime.config.Context), ".subyard")
	}
	owner, borrowErr := uninstall.BorrowOwnerLock(home)
	if borrowErr != nil {
		return borrowErr
	}
	if owner != nil {
		defer owner.Close()
		command.Env = append(command.Env, fmt.Sprintf("SUBYARD_OWNER_LOCK_FD=%d", 3+len(command.ExtraFiles)))
		command.ExtraFiles = append(command.ExtraFiles, owner)
	}
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, runtime.config.Stderr
	err := command.Run()
	if err != nil && ctx.Err() != nil {
		return errors.Join(err, ctx.Err())
	}
	return err
}

func (runtime *Runtime) invokeVerifiedCandidateTransition(
	ctx context.Context,
	candidate *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest,
	grant releasetransition.Authorization,
) (releasetransition.ProcessResponse, error) {
	return runtime.invokeVerifiedRuntimeTransition(ctx, candidate, candidate, request, grant)
}

func (runtime *Runtime) invokeVerifiedRuntimeTransition(
	ctx context.Context,
	assets, engine *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest,
	grant releasetransition.Authorization,
) (releasetransition.ProcessResponse, error) {
	if assets == nil || assets.root == nil || engine == nil || engine.engine == nil {
		return releasetransition.ProcessResponse{},
			errors.New("verified published runtime is unavailable")
	}
	if !releaseOwnerRoutingEnabled && (assets.releaseOwner != nil || engine.releaseOwner != nil) {
		return releasetransition.ProcessResponse{}, markSelectedRustOwnerError(errors.New("experimental Rust release ownership is disabled; use an ordinary Go runtime"))
	}
	if assets.registryDigest == "" {
		return releasetransition.ProcessResponse{},
			errors.New("published runtime manifest does not bind release transition registry")
	}
	if engine.registryDigest != assets.registryDigest {
		return releasetransition.ProcessResponse{},
			errors.New("verified runtime engine and assets have incompatible release transition registries")
	}
	if err := runtime.pinCandidateRoot(assets); err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	if request.RegistryDigest != "" && request.RegistryDigest != assets.registryDigest {
		return releasetransition.ProcessResponse{},
			errors.New("published runtime registry changed after inspection")
	}
	request.RegistryDigest = assets.registryDigest
	if releaseOwnerRoutingEnabled && engine.releaseOwner != nil {
		target := assets
		if target.candidate.release != request.Target || target.manifestDigest != request.ArtifactDigest {
			var err error
			target, err = runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: request.Target,
				root: filepath.Join(request.RuntimeRoot, "releases", string(request.Target))}, request.RuntimeRoot, &request.ArtifactDigest)
			if err != nil {
				return releasetransition.ProcessResponse{}, err
			}
			defer target.Close()
		}
		return runtime.invokeRustOwnerTransition(ctx, assets, engine, target, request, nil, grant)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	var stdout boundedResponseBuffer
	if err := runtime.runVerifiedRuntimeEngine(
		ctx, assets, engine, request.RuntimeRoot, []string{"_release-transition"},
		bytes.NewReader(append(payload, '\n')), &stdout, grant,
	); err != nil {
		return releasetransition.ProcessResponse{},
			fmt.Errorf("candidate release transition failed: %w", err)
	}
	if stdout.Len() > maximumCandidateResponseBytes {
		return releasetransition.ProcessResponse{}, errors.New("candidate release transition response is too large")
	}
	var response releasetransition.ProcessResponse
	// ProcessResponse's frozen V1 codec owns schema, shape and trailing-data checks.
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
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
