package releaseowner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type rollbackReaderFixture struct {
	noEffects
	calls *int
}

func (f rollbackReaderFixture) CaptureRollbackSupport(_ context.Context, source, target string, _ []string) (ports.RollbackSupport, error) {
	(*f.calls)++
	left, err := os.ReadFile(filepath.Join(source, "asset"))
	if err != nil {
		return ports.RollbackSupport{}, err
	}
	right, err := os.ReadFile(filepath.Join(target, "asset"))
	if err != nil {
		return ports.RollbackSupport{}, err
	}
	return ports.RollbackSupport{Consumers: []ports.RollbackConsumer{}, Binding: digest(append(left, right...))}, nil
}

func TestRollbackSupportUsesExactRepositoryRolesAndBoundObservations(t *testing.T) {
	configRoot, sourceRoot, targetRoot := testkit.TempDir(t), testkit.TempDir(t), testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(sourceRoot, "asset"), []byte("source"), 0o600)
	testkit.WriteFile(t, filepath.Join(targetRoot, "asset"), []byte("target"), 0o600)
	open := func(path string) *os.File {
		t.Helper()
		f, e := os.Open(path)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	config, source, target := open(configRoot), open(sourceRoot), open(targetRoot)
	rootID, _ := IdentityOf(config)
	sourceID, _ := IdentityOf(source)
	targetID, _ := IdentityOf(target)
	hash := strings.Repeat("a", 64)
	c := Context{Session: hash, DataHome: configRoot, ConfigHome: configRoot, RuntimeRoot: configRoot, RepositoryRoot: targetRoot, Yard: "default", Target: "new", Artifact: hash, Root: rootID, Repository: targetID, Releases: releasetransition.ReleasePair{From: "old", Target: "new"}}
	calls := 0
	s := session{factory: func(context.Context, Context, *os.File) (Components, error) {
		return rollbackReaderFixture{calls: &calls}, nil
	}}
	defer s.close()
	invoke := func(r Request, files ...*os.File) (Reply, error) {
		raw, _ := json.Marshal(r)
		return s.handle(context.Background(), raw, files)
	}
	if _, err := invoke(Request{Op: "initialize", Context: &c}, config, target); err != nil {
		t.Fatal(err)
	}
	repositories := RollbackRepositories{Source: RollbackRepository{"old", strings.Repeat("b", 64), sourceID}, Target: RollbackRepository{"new", hash, targetID}}
	r := Request{Op: "capture-rollback-support", Session: hash, RollbackRepositories: &repositories, TargetOwners: []string{"synthetic-runtime"}}
	first, err := invoke(r, source, target)
	if err != nil || first.RollbackSupport == nil {
		t.Fatal(first, err)
	}
	second, err := invoke(r, open(sourceRoot), open(targetRoot))
	if err != nil || second.RollbackSupport.Binding != first.RollbackSupport.Binding {
		t.Fatal("descriptor numbers changed original binding", second, err)
	}
	for _, files := range [][]*os.File{{target, source}, {source}, {source, target, config}} {
		if _, err := invoke(r, files...); err == nil {
			t.Fatal("wrong descriptor roles/count accepted")
		}
	}
	if calls != 2 {
		t.Fatal("rejected repository request reached component", calls)
	}
	r.Session = strings.Repeat("c", 64)
	if _, err := invoke(r, source, target); err == nil {
		t.Fatal("foreign session accepted")
	}
	r.Session = hash
	repositories.Source.Release = "other"
	if _, err := invoke(r, source, target); err == nil {
		t.Fatal("unapproved source accepted")
	}
	if calls != 2 {
		t.Fatal("foreign source/session reached component", calls)
	}
	for _, direction := range []releasetransition.Direction{releasetransition.DirectionActivateTarget, releasetransition.DirectionActivatePrevious} {
		ownerContext := c
		ownerContext.Repository = sourceID
		ownerContext.RepositoryRoot = sourceRoot
		ownerContext.Direction = direction
		readerCalls := 0
		owner := session{factory: func(context.Context, Context, *os.File) (Components, error) {
			return rollbackReaderFixture{calls: &readerCalls}, nil
		}}
		initialize, _ := json.Marshal(Request{Op: "initialize", Context: &ownerContext})
		if _, err := owner.handle(context.Background(), initialize, []*os.File{config, source}); err != nil {
			t.Fatal(err)
		}
		repositories.Source.Release = "old"
		request, _ := json.Marshal(r)
		reply, err := owner.handle(context.Background(), request, []*os.File{source, target})
		owner.close()
		if direction == releasetransition.DirectionActivatePrevious {
			if err != nil || reply.RollbackSupport == nil || readerCalls != 1 {
				t.Fatal("read-only retained target was confused with active owner root", reply, err)
			}
		} else if err == nil || readerCalls != 0 {
			t.Fatal("forward capture accepted a foreign target repository")
		}
	}
}

func TestRollbackSupportCompletedSameReleaseRequiresExactOriginalTuple(t *testing.T) {
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "asset"), []byte("retained"), 0600)
	file, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	identity, _ := IdentityOf(file)
	hash := strings.Repeat("a", 64)
	original := RollbackRepository{Release: "retained", ArtifactDigest: hash, Root: identity}
	for _, change := range []string{"same-retained", "forward", "different-digest", "different-root"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			c := Context{Session: hash, Target: "retained", Artifact: hash, Repository: identity, Direction: releasetransition.DirectionActivatePrevious, Releases: releasetransition.ReleasePair{From: "retained", Target: "retained"}}
			repositories := RollbackRepositories{Source: original, Target: original}
			switch change {
			case "forward":
				c.Direction = releasetransition.DirectionActivateTarget
			case "different-digest":
				repositories.Source.ArtifactDigest = strings.Repeat("b", 64)
			case "different-root":
				repositories.Source.Root.Inode++
			}
			s := session{context: &c, components: rollbackReaderFixture{calls: &calls}}
			reply, err := s.captureRollbackSupport(context.Background(), Request{RollbackRepositories: &repositories}, []*os.File{file, file})
			if change == "same-retained" {
				if err != nil || reply.RollbackSupport == nil || calls != 1 {
					t.Fatal("completed exact readonly tuple refused", err)
				}
			} else if err == nil || calls != 0 {
				t.Fatal("foreign same-release tuple reached component")
			}
		})
	}
}
