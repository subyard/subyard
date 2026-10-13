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
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// SourceAdmission binds a read-only protected proof to exact installed assets
// and links. It cannot execute a historical transaction or confer readiness.
type SourceAdmission struct {
	Metadata releasetransition.SourceMetadata
	Binding  string
}

// InspectSourceTooling admits only a verified immutable published runtime,
// including staged tooling. A source checkout cannot acquire source repair authority.
func (runtime *Runtime) InspectSourceTooling(ctx context.Context, root, repository string) (string, string, error) {
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil || filepath.Dir(resolved) != filepath.Join(root, "releases") {
		return "", "", sourceAdmissionError{"assets", errors.New("repair tooling is not a published immutable runtime")}
	}
	candidate := publishedCandidate{release: releasetransition.ReleaseID(filepath.Base(resolved)), root: resolved}
	verified, err := runtime.verifyPublishedCandidate(ctx, candidate, root, nil)
	if err != nil {
		return "", "", sourceAdmissionError{"assets", err}
	}
	defer verified.Close()
	if !strings.HasPrefix(string(candidate.release), verified.version+"-") {
		return "", "", sourceAdmissionError{"assets", errors.New("tooling identity is inconsistent")}
	}
	inputs, err := sourceCandidateInputs(verified)
	if err != nil {
		return "", "", sourceAdmissionError{"assets", err}
	}
	if _, bound := inputs[filepath.Join(resolved, "config", "commands.registry")]; !bound {
		return "", "", sourceAdmissionError{"assets", errors.New("repair command contract is not sealed")}
	}
	payload, err := json.Marshal(struct {
		Artifact releasetransition.Fingerprint
		Files    map[string]sourceCandidateInput
	}{verified.manifestDigest, inputs})
	if err != nil {
		return "", "", sourceAdmissionError{"assets", err}
	}
	identity := sha256.Sum256(payload)
	return filepath.Join(resolved, "config"), hex.EncodeToString(identity[:]), nil
}

type sourceAdmissionError struct {
	phase string
	cause error
}

func (err sourceAdmissionError) Error() string {
	switch err.phase {
	case "capability":
		return "release source admission: original-owner capability is unsupported; source repair of unfinished history requires schema 2 activation recovery from the exact sealed original owner; staged tooling cannot upgrade an old caller"
	case "catalog":
		return "release source admission: registry or catalog is unsupported; restore the exact original registry and use a release supporting that catalog"
	case "assets":
		return "release source admission: sealed original assets are unavailable or inconsistent; restore the exact original release without changing protected history"
	case "links":
		return "release source admission: runtime links do not match protected history; restore the exact active and previous releases before source repair"
	default:
		return "release source admission: protected metadata is unsupported or unresolved; finish pending publications or recovery intent with the exact sealed original owner before source repair"
	}
}
func (err sourceAdmissionError) Unwrap() error { return err.cause }

// InspectSourceAdmission avoids ordinary operational inspection. An unfinished
// activation requires a capability-only probe of the ORIGINAL sealed owner.
func (runtime *Runtime) InspectSourceAdmission(ctx context.Context, root, configHome string) (proof SourceAdmission, result error) {
	phase := "links"
	defer func() {
		if result != nil {
			result = sourceAdmissionError{phase, result}
		}
	}()
	links, err := runtime.inspectRuntimeLinks(root)
	if err != nil {
		return proof, err
	}
	actual := releaseLinksFromRuntimeSnapshot(links)
	if actual.Active == "" {
		return proof, errors.New("verified current runtime is required")
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return proof, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return proof, err
	}
	selected := actual.Active
	var artifact *releasetransition.Fingerprint
	var journal *releasetransition.JournalRecord
	if snapshot.Exists {
		value, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return proof, err
		}
		journal = &value
		if value.Goal.Target != actual.Active {
			return proof, errors.New("protected journal does not name the active runtime")
		}
		previous := value.Releases.Previous
		if value.Releases.From != value.Releases.Target {
			from := value.Releases.From
			previous = &from
		}
		if !sameRecoveryReleaseIDs(actual.Previous, previous) {
			return proof, errors.New("actual previous runtime differs from protected history")
		}
		selected, artifact = value.Goal.Target, &value.ArtifactDigest
	}
	phase = "assets"
	verified, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: selected, root: filepath.Join(root, "releases", string(selected))}, root, artifact)
	if err != nil {
		return proof, err
	}
	defer verified.Close()
	if !strings.HasPrefix(string(selected), verified.version+"-") {
		return proof, errors.New("sealed release identity is inconsistent")
	}
	inputs, err := sourceCandidateInputs(verified)
	if err != nil {
		return proof, err
	}
	if journal != nil && journal.RegistryDigest != verified.registryDigest {
		if journal.Goal.Direction != releasetransition.DirectionActivatePrevious {
			return proof, errors.New("sealed target does not own the original registry")
		}
		owner := journal.Releases.From
		original, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: owner, root: filepath.Join(root, "releases", string(owner))}, root, nil)
		if err != nil {
			return proof, err
		}
		defer original.Close()
		if original.registryDigest != journal.RegistryDigest || !strings.HasPrefix(string(owner), original.version+"-") {
			return proof, errors.New("sealed original owner does not match protected history")
		}
		ownerInputs, err := sourceCandidateInputs(original)
		if err != nil {
			return proof, err
		}
		for path, input := range ownerInputs {
			inputs[path] = input
		}
		verified = original
	}
	phase = "catalog"
	registry, digest, err := readVerifiedTransitionRegistry(verified)
	if err != nil {
		return proof, err
	}
	if journal != nil {
		if _, supported := releasetransition.SupportedBuiltinCatalog(journal.CatalogDigest); !supported {
			return proof, errors.New("unsupported original catalog")
		}
	}
	phase = "protected metadata"
	proof.Metadata, err = store.InspectSourceMetadata(registry, digest)
	if err != nil {
		return proof, err
	}
	if proof.Metadata.Journal != nil {
		payload, marshalErr := releasetransition.MarshalJournal(*proof.Metadata.Journal)
		if marshalErr != nil || !snapshot.Exists || !bytes.Equal(payload, snapshot.Payload) {
			return proof, releasetransition.ErrProtectedStoreStale
		}
	} else if snapshot.Exists {
		return proof, releasetransition.ErrProtectedStoreStale
	}
	legacy, err := migration.InspectSourceHistory(ctx, migration.ReleaseOptions{RuntimeRoot: root, ConfigHome: configHome, Version: verified.version, RegistryPath: filepath.Join(verified.candidate.root, "config", "release-transition.json")})
	if err != nil {
		return proof, err
	}
	if journal != nil && journal.Checkpoint != releasetransition.JournalComplete {
		phase = "capability"
		if journal.Goal.Direction != releasetransition.DirectionActivateTarget {
			return proof, errors.New("unfinished source repair is forward-only")
		}
		probe := releasetransition.RecoveryProcessRequest{SchemaVersion: releasetransition.ProcessRecoverySchemaV2, Mode: releasetransition.RecoveryProcessLifecycleCapabilities}
		response, err := runtime.invokeVerifiedRecoveryTransition(ctx, verified, root, probe, "")
		if err != nil {
			probe.Mode = releasetransition.RecoveryProcessCapabilities
			response, err = runtime.invokeVerifiedRecoveryTransition(ctx, verified, root, probe, "")
		}
		if err != nil || response.Capabilities == nil || response.Capabilities.Validate() != nil || response.Inspection != nil || response.Outcome != nil {
			return proof, errors.New("original sealed owner must support schema 2 activation recovery before source repair; staged tooling cannot add that capability")
		}
	}
	phase = "assets"
	for _, name := range []string{"", "releases", "current", "previous"} {
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && name == "previous" {
			inputs[path] = sourceCandidateInput{}
			continue
		}
		if err != nil {
			return proof, err
		}
		input, err := sourceInputInfo(info)
		if err != nil {
			return proof, err
		}
		if name == "" || name == "releases" {
			// These validated containers can gain an approved release directory
			// after inspection. Their identity and permissions remain bound.
			input.Links = 0
		}
		inputs[path] = input
	}
	afterLinks, err := runtime.inspectRuntimeLinks(root)
	if err != nil || afterLinks != links {
		return proof, releasetransition.ErrProtectedStoreStale
	}
	payload, err := json.Marshal(struct {
		Metadata releasetransition.Fingerprint
		Legacy   releasetransition.Fingerprint
		Links    releasetransition.ReleaseLinks
		Files    map[string]sourceCandidateInput
	}{proof.Metadata.Binding, legacy, actual, inputs})
	if err != nil {
		return proof, err
	}
	binding := sha256.Sum256(payload)
	proof.Binding = hex.EncodeToString(binding[:])
	return proof, nil
}

type sourceCandidateInput struct {
	Present              bool
	Device, Inode, Links uint64
	Mode, UID, GID       uint32
}

func sourceInputInfo(info os.FileInfo) (sourceCandidateInput, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return sourceCandidateInput{}, errors.New("sealed input identity is unavailable")
	}
	return sourceCandidateInput{true, uint64(stat.Dev), stat.Ino, uint64(stat.Nlink), stat.Mode, stat.Uid, stat.Gid}, nil
}
func sourceCandidateInputs(verified *verifiedPublishedCandidate) (map[string]sourceCandidateInput, error) {
	manifest, err := openCandidateFile(int(verified.root.Fd()), "runtime-files.sha256")
	if err != nil {
		return nil, err
	}
	payload, err := io.ReadAll(io.LimitReader(manifest, 1<<20+1))
	manifest.Close()
	if err != nil {
		return nil, err
	}
	entries, err := parseCandidateManifest(payload)
	if err != nil {
		return nil, err
	}
	inputs := map[string]sourceCandidateInput{}
	manifestDigest := sha256.Sum256(payload)
	if releasetransition.Fingerprint(hex.EncodeToString(manifestDigest[:])) != verified.manifestDigest {
		return nil, errors.New("sealed manifest changed during admission")
	}
	digests := map[string][sha256.Size]byte{"runtime-files.sha256": manifestDigest}
	paths := []string{"runtime-files.sha256"}
	for _, entry := range entries {
		paths = append(paths, entry.path)
		digests[entry.path] = entry.digest
	}
	info, err := verified.root.Stat()
	if err != nil {
		return nil, err
	}
	inputs[verified.candidate.root], err = sourceInputInfo(info)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		file, err := openCandidateFile(int(verified.root.Fd()), path)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		before, err := sourceInputInfo(info)
		if err == nil {
			err = verifyCandidateFile(file, digests[path])
		}
		afterInfo, statErr := file.Stat()
		file.Close()
		if err != nil || statErr != nil {
			return nil, errors.New("sealed input changed during admission")
		}
		after, err := sourceInputInfo(afterInfo)
		if err != nil || before != after {
			return nil, errors.New("sealed input identity changed during admission")
		}
		inputs[filepath.Join(verified.candidate.root, path)] = before
		if err != nil {
			return nil, err
		}
	}
	return inputs, nil
}
