package releaseowner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

type retainedRepositoryFixture struct {
	noEffects
	source string
}

func (c retainedRepositoryFixture) CaptureInventory(context.Context) ([]string, []string, error) {
	value, err := os.ReadFile(filepath.Join(c.source, "asset"))
	return []string{string(value)}, []string{}, err
}

func TestNativeRepositoryReadsRetainedOFDWithoutOriginalOwnerPath(t *testing.T) {
	configRoot, parent := testkit.TempDir(t), testkit.TempDir(t)
	repositoryRoot := filepath.Join(parent, "repository")
	if err := os.Mkdir(repositoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(repositoryRoot, "asset"), []byte("approved"), 0o600)
	configFD, err := os.Open(configRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer configFD.Close()
	repositoryFD, err := os.Open(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootID, _ := IdentityOf(configFD)
	repoID, _ := IdentityOf(repositoryFD)
	hash := strings.Repeat("a", 64)
	ctx := Context{Session: hash, DataHome: configRoot, ConfigHome: configRoot, RuntimeRoot: configRoot, RepositoryRoot: repositoryRoot, Yard: "default", Target: "new", Artifact: hash, Root: rootID, Repository: repoID}
	session := session{factory: func(_ context.Context, c Context, _ *os.File) (Components, error) {
		if c.RepositoryRoot != repositoryRoot || !strings.HasPrefix(c.PhysicalRepositoryRoot, "/proc/self/fd/") {
			t.Fatal("logical and physical roots were conflated")
		}
		return retainedRepositoryFixture{source: c.PhysicalRepositoryRoot}, nil
	}}
	defer session.close()
	raw, _ := json.Marshal(Request{Op: "initialize", Context: &ctx})
	if _, err := session.handle(context.Background(), raw, []*os.File{configFD, repositoryFD}); err != nil {
		t.Fatal(err)
	}
	repositoryFD.Close()
	if err := os.Rename(repositoryRoot, repositoryRoot+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repositoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(repositoryRoot, "asset"), []byte("replacement"), 0o600)
	raw, _ = json.Marshal(Request{Op: "capture-inventory", Session: hash})
	reply, err := session.handle(context.Background(), raw, nil)
	if err != nil || len(reply.Yards) != 1 || reply.Yards[0] != "approved" {
		t.Fatal("source was reopened through replaced original owner path", reply, err)
	}
}
