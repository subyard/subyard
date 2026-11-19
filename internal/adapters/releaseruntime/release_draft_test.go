package releaseruntime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestBootstrapDraftHandoffVerifiesBundleAndDirectory(t *testing.T) {
	for _, change := range []string{"none", "rewritten manifest", "changed bundle"} {
		t.Run(change, func(t *testing.T) {
			rt, candidate, _, _, arguments := bootstrapDraftFixture(t)
			parsed, _, err := rt.parse(arguments)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "rewritten manifest":
				testkit.WriteFile(t, filepath.Join(candidate.root, "bin/yard-engine"), []byte("#!/bin/sh\nexit 91\n"), 0o700)
				rewriteReleaseOwnerManifest(t, candidate)
			case "changed bundle":
				bundle := filepath.Join(parsed.cache, parsed.version, "subyard-"+parsed.version+"-linux-"+goruntime.GOARCH+".tar.gz")
				testkit.WriteFile(t, bundle, append(mustReadFile(t, bundle), ' '), 0o600)
			}
			_, verified, err := rt.verifyBootstrapDraft(context.Background(), parsed, rt.config.Inputs.bootstrapDraft)
			if change != "none" {
				if err == nil {
					verified.Close()
					t.Fatal("changed handoff was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer verified.Close()
			if verified.candidate != candidate || verified.version != parsed.version || verified.registryDigest == "" {
				t.Fatal("handoff did not select the exact verified release")
			}
		})
	}
}

func TestBootstrapDraftNamesUnapprovedCanonicalPublication(t *testing.T) {
	rt, candidate, request, called, arguments := bootstrapDraftFixture(t)
	canonical := filepath.Join(request.RuntimeRoot, "releases", string(candidate.release))
	if err := os.CopyFS(canonical, os.DirFS(candidate.root)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.PrepareTransition(context.Background(), arguments, request.ConfigHome, request.Yard)
	if err == nil || !strings.Contains(err.Error(), "rollback-incompatible") ||
		!strings.Contains(err.Error(), "already published without a matching authorized transition") ||
		!strings.Contains(err.Error(), "different verified forward release") {
		t.Fatalf("orphan publication did not give guarded retry advice: %v", err)
	}
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused candidate ran its transition: %v", err)
	}
	if after, err := os.Stat(canonical); err != nil || !os.SameFile(before, after) {
		t.Fatalf("refusal changed the canonical candidate: %v", err)
	}
}

func bootstrapDraftFixture(t *testing.T) (*Runtime, publishedCandidate, releasetransition.ProcessRequest, string, []string) {
	t.Helper()
	rt, candidate, request, called, arguments := retainedVersionFixture(t)
	if err := os.Remove(filepath.Join(request.RuntimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	manifest := mustReadFile(t, filepath.Join(candidate.root, "runtime-files.sha256"))
	version := arguments[2]
	digest := writeBootstrapFixtureBundle(t, rt.config.Inputs.values["YARD_RELEASE_CACHE"], version, manifest)
	draftRoot := filepath.Join(filepath.Dir(request.RuntimeRoot), releaseDraftPrefix+"fixture")
	if err := os.MkdirAll(filepath.Join(draftRoot, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(draftRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	id := releasetransition.ReleaseID(version + "-" + digest[:12])
	draft := publishedCandidate{release: id, root: filepath.Join(draftRoot, "releases", string(id))}
	if err := os.Rename(candidate.root, draft.root); err != nil {
		t.Fatal(err)
	}
	rt.config.RepositoryRoot = draft.root
	rt.config.Inputs.bootstrapDraft = digest
	rt.config.Installer = "/unavailable"
	return rt, draft, request, called, arguments
}

func writeBootstrapFixtureBundle(t *testing.T, cache, version string, manifest []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cache, version), 0o700); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	compressed := gzip.NewWriter(&payload)
	archived := tar.NewWriter(compressed)
	if err := archived.WriteHeader(&tar.Header{Name: "./runtime-files.sha256", Mode: 0o644, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archived.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(archived.Close(), compressed.Close()); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(cache, version, "subyard-"+version+"-linux-"+goruntime.GOARCH+".tar.gz"), payload.Bytes(), 0o600)
	return fmt.Sprintf("%x", sha256.Sum256(payload.Bytes()))
}

func TestBootstrapDraftCleanupOnSameTargetResume(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	writeProtectedRuntimeFixtureEngine(t, fixture, `#!/bin/sh
case "$1" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat >/dev/null; printf '%s\n' "$TEST_RECOVERY_RESPONSE" ;;
  *) exit 2 ;;
esac
`)
	published := filepath.Dir(filepath.Dir(fixture.engine))
	manifest := mustReadFile(t, filepath.Join(published, "runtime-files.sha256"))
	digest := writeBootstrapFixtureBundle(t, fixture.cache, "1.2.3", manifest)
	target := "1.2.3-" + digest[:12]
	journal := bytes.ReplaceAll(mustReadFile(t, fixture.journalPath), []byte(fixture.target), []byte(target))
	testkit.WriteFile(t, fixture.journalPath, journal, 0o600)
	canonical := filepath.Join(fixture.runtimeRoot, "releases", target)
	if err := os.Rename(published, canonical); err != nil {
		t.Fatal(err)
	}
	fixture.target = target
	draft := filepath.Join(filepath.Dir(fixture.runtimeRoot), releaseDraftPrefix+"resume")
	if err := os.Mkdir(draft, 0o700); err != nil {
		t.Fatal(err)
	}
	draftCandidate := filepath.Join(draft, "releases", target)
	if err := os.CopyFS(draftCandidate, os.DirFS(canonical)); err != nil {
		t.Fatal(err)
	}
	inputs := fixture.environment()
	inputs[BootstrapDraftInput] = digest
	inputs["TEST_RECOVERY_RESPONSE"] = fixture.recoveringResponse()
	rt := New(Config{Inputs: fixtureCommandInputs(inputs), RepositoryRoot: draftCandidate})
	defer rt.Close()
	prepared, err := rt.PrepareTransition(context.Background(), []string{"--offline", "--version", "1.2.3", "--runtime-root", fixture.runtimeRoot}, fixture.configHome, "default")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Changed || prepared.TargetRelease != target || prepared.nativePlan != releasetransition.PlanToken(fixture.resumePlan) ||
		prepared.BootstrapRepositoryRoot != canonical || prepared.Close == nil {
		t.Fatal("expected the exact published resume with ownership of the unused draft")
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(draftCandidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume left the unpacked draft: %v", err)
	}
	if !bytes.Equal(mustReadFile(t, fixture.journalPath), journal) ||
		!bytes.Equal(mustReadFile(t, filepath.Join(canonical, "runtime-files.sha256")), manifest) {
		t.Fatal("draft cleanup changed the journal or published owner")
	}
}

func TestBootstrapDraftCleanupOnExactTargetRefusal(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	journal := mustReadFile(t, fixture.journalPath)
	canonical := filepath.Dir(filepath.Dir(fixture.engine))
	manifest := mustReadFile(t, filepath.Join(canonical, "runtime-files.sha256"))
	parent := filepath.Dir(fixture.runtimeRoot)
	template := filepath.Join(parent, "other-release")
	if err := os.CopyFS(template, os.DirFS(canonical)); err != nil {
		t.Fatal(err)
	}
	const version = "1.2.2"
	testkit.WriteFile(t, filepath.Join(template, "bin", "yard-engine"), []byte(`#!/bin/sh
[ "$1" = --version ] || exit 64
printf 'yard-engine 1.2.2\n'
`), 0o700)
	rewriteReleaseOwnerManifest(t, publishedCandidate{root: template})
	digest := writeBootstrapFixtureBundle(t, fixture.cache, version, mustReadFile(t, filepath.Join(template, "runtime-files.sha256")))
	inputs := fixture.environment()
	inputs[BootstrapDraftInput] = digest
	inputs["SUBYARD_INSTALL_ENTRYPOINTS"] = "{}"
	for attempt := range 2 {
		draft, err := os.MkdirTemp(parent, releaseDraftPrefix)
		if err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(draft, "releases", version+"-"+digest[:12])
		if err := os.CopyFS(candidate, os.DirFS(template)); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(draft, "update.lock"), nil, 0o600)
		testkit.WriteFile(t, filepath.Join(draft, "installation.json"), nil, 0o600)
		rt := New(Config{Inputs: fixtureCommandInputs(inputs), RepositoryRoot: candidate})
		_, err = rt.PrepareTransition(context.Background(), []string{"--offline", "--version", version, "--runtime-root", fixture.runtimeRoot}, fixture.configHome, "default")
		rt.Close()
		if err == nil || !strings.Contains(err.Error(), "permits only exact target "+fixture.target) {
			t.Fatalf("attempt %d did not refuse the different target: %v", attempt+1, err)
		}
		if err := cleanupDraftWrapper(draft); err != nil {
			t.Fatalf("attempt %d left the wrapper or unpacked draft: %v", attempt+1, err)
		}
		if _, err := os.Lstat(draft); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("attempt %d left a draft wrapper: %v", attempt+1, err)
		}
	}
	if !bytes.Equal(mustReadFile(t, fixture.journalPath), journal) ||
		!bytes.Equal(mustReadFile(t, filepath.Join(canonical, "runtime-files.sha256")), manifest) {
		t.Fatal("refused drafts changed the journal or published owner")
	}
}

func TestApprovedDraftPublication(t *testing.T) {
	for _, change := range []string{"none", "destination exists", "runtime replaced"} {
		t.Run(change, func(t *testing.T) {
			rt, candidate, request, _, args := bootstrapDraftFixture(t)
			parsed, _, err := rt.parse(args)
			if err != nil {
				t.Fatal(err)
			}
			_, verified, err := rt.verifyBootstrapDraft(context.Background(), parsed, rt.config.Inputs.bootstrapDraft)
			if err != nil {
				t.Fatal(err)
			}
			defer verified.Close()
			root := request.RuntimeRoot
			releases := filepath.Join(root, "releases")
			rootInfo, _ := os.Lstat(root)
			releasesInfo, _ := os.Lstat(releases)
			target := filepath.Join(releases, string(candidate.release))
			var want error
			switch change {
			case "destination exists":
				err, want = os.Mkdir(target, 0o700), unix.EEXIST
			case "runtime replaced":
				if err = os.Rename(root, root+"-old"); err == nil {
					err = os.MkdirAll(releases, 0o700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if change != "none" && want == nil {
				want = domain.ErrPlanStale
			}
			if err := publishApprovedDraft(verified, root, rootInfo, releasesInfo); !errors.Is(err, want) {
				t.Fatalf("publish = %v, want %v", err, want)
			}
			actual, err := os.Lstat(target)
			switch change {
			case "none":
				before, statErr := verified.root.Stat()
				if err != nil || statErr != nil || !os.SameFile(before, actual) {
					t.Fatalf("publication changed the approved inode: %v %v", statErr, err)
				}
			case "destination exists":
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 {
					t.Fatalf("publication changed the existing destination: %v %v", entries, err)
				}
			default:
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale publication installed a target: %v", err)
				}
			}
		})
	}
}

func TestApprovedDraftCleanupPreservesUnlistedFile(t *testing.T) {
	rt, candidate, _, _, args := bootstrapDraftFixture(t)
	parsed, _, err := rt.parse(args)
	if err != nil {
		t.Fatal(err)
	}
	_, verified, err := rt.verifyBootstrapDraft(context.Background(), parsed, rt.config.Inputs.bootstrapDraft)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	other := filepath.Join(candidate.root, "config", "unlisted")
	testkit.WriteFile(t, other, []byte("preserve"), 0o600)
	if err := verified.cleanupDraft(); !errors.Is(err, unix.ENOTEMPTY) {
		t.Fatalf("cleanup did not report the nonempty draft: %v", err)
	}
	if got := string(mustReadFile(t, other)); got != "preserve" {
		t.Fatalf("cleanup changed the unlisted file: %q", got)
	}
}
