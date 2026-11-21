package releaseruntime

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"golang.org/x/sys/unix"
)

const releaseDraftPrefix = ".subyard-release-draft-"

// The bootstrap describes the exact bundle and directory it verified, while
// the launcher supplies the repository root. Neither input grants consent.
func (runtime *Runtime) verifyBootstrapDraft(ctx context.Context, parsed options, bundleDigest string) (string, *verifiedPublishedCandidate, error) {
	if !parsed.offline || !parsed.versionExplicit || parsed.force {
		return "", nil, errors.New("bootstrap handoff requires a selected offline update")
	}
	if len(bundleDigest) != 64 {
		return "", nil, errors.New("invalid bootstrap draft descriptor")
	}
	if _, err := hex.DecodeString(bundleDigest); err != nil {
		return "", nil, errors.New("invalid bootstrap bundle digest")
	}
	release := releasetransition.ReleaseID(parsed.version + "-" + bundleDigest[:12])
	candidate := publishedCandidate{release: release, root: runtime.config.RepositoryRoot}
	draftRoot := filepath.Dir(filepath.Dir(candidate.root))
	suffix, prefixed := strings.CutPrefix(filepath.Base(draftRoot), releaseDraftPrefix)
	if !prefixed || !domain.SafeID(suffix) || draftRoot == parsed.root || filepath.Dir(draftRoot) != filepath.Dir(parsed.root) ||
		candidate.root != filepath.Join(draftRoot, "releases", string(release)) {
		return "", nil, errors.New("bootstrap draft is outside the private runtime staging layout")
	}
	if _, err := validateReleaseRoot(draftRoot, "bootstrap draft"); err != nil {
		return "", nil, errors.New("bootstrap draft has an unsafe directory")
	}
	info, err := os.Lstat(draftRoot)
	if err != nil || info.Mode() != os.ModeDir|0o700 {
		return "", nil, errors.New("bootstrap draft directory must be private")
	}
	root, err := openVerifiedCandidateRoot(candidate, draftRoot)
	if err != nil {
		return "", nil, err
	}
	manifest, err := bootstrapBundleManifest(parsed, bundleDigest)
	if err != nil {
		root.Close()
		return "", nil, err
	}
	verified := &verifiedPublishedCandidate{candidate: candidate, root: root}
	if err := verifyOpenCandidateFiles(verified, &manifest, true); err != nil {
		verified.Close()
		return "", nil, err
	}
	verified.version, err = runtime.runVerifiedCandidateVersion(ctx, verified, draftRoot)
	return draftRoot, verified, err
}

// Read the manifest and hash the archive through the same open file. An
// internally consistent but replaced draft must still match the bundle.
func bootstrapBundleManifest(parsed options, expected string) (releasetransition.Fingerprint, error) {
	name := fmt.Sprintf("subyard-%s-linux-%s.tar.gz", parsed.version, runtime.GOARCH)
	bundle, err := openReleaseBundle(filepath.Join(parsed.cache, parsed.version, name))
	if err != nil {
		return "", errors.New("verified bootstrap bundle is unavailable")
	}
	defer bundle.Close()
	digest := sha256.New()
	archive := io.TeeReader(bundle, digest)
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return "", errors.New("bootstrap bundle is invalid")
	}
	defer compressed.Close()
	members := tar.NewReader(compressed)
	for {
		header, err := members.Next()
		if err != nil {
			return "", errors.New("bootstrap bundle manifest is unavailable")
		}
		if strings.TrimPrefix(header.Name, "./") != "runtime-files.sha256" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 1<<20 {
			return "", errors.New("bootstrap bundle manifest is invalid")
		}
		manifest := sha256.New()
		if _, err := io.Copy(manifest, members); err != nil {
			return "", errors.New("bootstrap bundle manifest is unreadable")
		}
		if _, err := io.Copy(io.Discard, archive); err != nil || hex.EncodeToString(digest.Sum(nil)) != expected {
			return "", errors.New("bootstrap bundle changed after verification")
		}
		return releasetransition.Fingerprint(hex.EncodeToString(manifest.Sum(nil))), nil
	}
}

func unapprovedBootstrapPublication(selection *releaseSelection) bool {
	target := "releases/" + string(selection.request.Target)
	if selection.links.current.target == target || selection.links.previous.target == target ||
		selection.compatibility == nil || selection.compatibility.RegistryAdmitted() || selection.cause != nil {
		return false
	}
	if !selection.state.journal.Exists {
		return true
	}
	journal, err := releasetransition.ParseJournal(selection.state.journal.Payload)
	return err == nil && journal.Goal.Target != selection.request.Target
}

// Publication is the final mutation of the verified draft. The validated
// ancestry limits writers to the operator and root, and the owner barrier
// serializes supported writers. Reverify the live tree before convergence.
func publishApprovedDraft(candidate *verifiedPublishedCandidate, root string, rootBefore, releasesBefore os.FileInfo) error {
	releasesFD, err := openApprovedReleaseDirectory(root, rootBefore, releasesBefore)
	if err != nil {
		return err
	}
	defer unix.Close(releasesFD)
	check := &verifiedPublishedCandidate{root: candidate.root}
	if err := verifyOpenCandidateFiles(check, &candidate.manifestDigest, false); err != nil {
		return fmt.Errorf("%w: reverify approved release draft: %w", domain.ErrPlanStale, err)
	}
	draftFD, err := openCandidateDraftParent(candidate)
	if err != nil {
		return err
	}
	defer unix.Close(draftFD)
	name := string(candidate.candidate.release)
	if err := unix.Renameat2(draftFD, name, releasesFD, name, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("release draft and runtime releases must share a mount for atomic publication: %w", err)
		}
		return err
	}
	candidate.published = true
	return errors.Join(unix.Fsync(releasesFD), unix.Fsync(draftFD))
}

func openCandidateDraftParent(candidate *verifiedPublishedCandidate) (int, error) {
	return unix.Openat(int(candidate.root.Fd()), "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

func (candidate *verifiedPublishedCandidate) cleanupDraft() error {
	if candidate.published {
		return nil
	}
	parent, err := openCandidateDraftParent(candidate)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	children := make(map[string][]string)
	for path := range candidate.inventory {
		directory := filepath.Dir(path)
		children[directory] = append(children[directory], path)
	}
	var remove func(int, string) error
	remove = func(fd int, directory string) error {
		slices.Sort(children[directory])
		for _, path := range children[directory] {
			name := filepath.Base(path)
			flags := 0
			if candidate.inventory[path] {
				child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if err != nil {
					return err
				}
				err = remove(child, path)
				if err := errors.Join(err, unix.Close(child)); err != nil {
					return err
				}
				flags = unix.AT_REMOVEDIR
			}
			if err := unix.Unlinkat(fd, name, flags); err != nil {
				return err
			}
		}
		return nil
	}
	if err := remove(int(candidate.root.Fd()), "."); err != nil {
		return err
	}
	return unix.Unlinkat(parent, string(candidate.candidate.release), unix.AT_REMOVEDIR)
}

// Only the publisher's fixed wrapper entries belong to the staging root.
func cleanupDraftWrapper(root string) error {
	for _, name := range []string{"installation.json", "update.lock", "releases", "."} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func openApprovedReleaseDirectory(root string, rootBefore, releasesBefore os.FileInfo) (int, error) {
	rootFD, err := openApprovedDirectory(root, rootBefore)
	if err != nil {
		return -1, err
	}
	defer unix.Close(rootFD)
	releasesFD, err := unix.Openat(rootFD, "releases", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	info, err := approvedDirectoryInfo(releasesFD)
	if err != nil || !os.SameFile(info, releasesBefore) || info.Mode() != releasesBefore.Mode() {
		return -1, errors.Join(fmt.Errorf("%w: runtime release directory changed", domain.ErrPlanStale), unix.Close(releasesFD))
	}
	return releasesFD, nil
}

func openApprovedDirectory(path string, expected os.FileInfo) (int, error) {
	if expected == nil || !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return -1, fmt.Errorf("%w: approved directory is invalid", domain.ErrPlanStale)
	}
	stat, ok := expected.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || expected.Mode().Perm()&0o022 != 0 {
		return -1, fmt.Errorf("%w: unsafe approved directory", domain.ErrPlanStale)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	info, err := approvedDirectoryInfo(fd)
	if err != nil || !os.SameFile(info, expected) || info.Mode() != expected.Mode() {
		return -1, errors.Join(fmt.Errorf("%w: approved directory changed", domain.ErrPlanStale), unix.Close(fd))
	}
	return fd, nil
}

func approvedDirectoryInfo(fd int) (os.FileInfo, error) {
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "approved-directory")
	info, statErr := file.Stat()
	return info, errors.Join(statErr, file.Close())
}
