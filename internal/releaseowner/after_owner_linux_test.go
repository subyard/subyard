//go:build linux

package releaseowner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

type afterOwnerFixture struct {
	*ownerResultsFixture
	calls    int
	drift    bool
	deferred bool
	absent   bool
	recipe   string
}

func (f *afterOwnerFixture) ResolveAfterOwner(_ context.Context, t Target, c AfterOwnerCompletion) (Target, string, error) {
	f.calls++
	t.InstanceUUID = "original-created-uuid"
	if f.absent {
		t.InstanceUUID = "absent"
	}
	t.State = "stale"
	if f.deferred {
		t.State = "deferred"
	}
	t.PreparedRecipe = strings.Repeat("d", 64)
	if f.recipe != "" {
		t.PreparedRecipe = f.recipe
	}
	if f.drift {
		t.Source = strings.Repeat("e", 64)
	}
	return t, t.PreparedRecipe, nil
}
func TestAfterOwnerResolutionUsesStrictSessionAdmissionAndClosedProducer(t *testing.T) {
	for _, mode := range []string{"valid", "deferred", "missing-parent", "failed-parent", "source-drift", "missing-lease", "extra-field", "ordinary-target", "unresolved-absent"} {
		t.Run(mode, func(t *testing.T) {
			s, r, o, base, _ := ownerResultsOriginal(t)
			state := "complete"
			if mode == "failed-parent" {
				state = "failed"
			}
			if mode != "missing-parent" {
				ownerResultsTerminal(t, s, o, state)
			}
			fixture := &afterOwnerFixture{ownerResultsFixture: base, drift: mode == "source-drift", deferred: mode == "deferred", absent: mode == "unresolved-absent"}
			s.components = fixture
			root, err := IdentityOf(s.root)
			if err != nil {
				t.Fatal(err)
			}
			s.context.Root = root
			r.Session = s.context.Session
			reply, err := s.bindOwnerResults(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			original := Target{Owner: "materialized-config", Yard: "test-yard", Class: "materialized-files", InstanceUUID: "absent", PreparedRecipe: strings.Repeat("b", 64), AfterOwner: "v1:materialized-files:" + strings.Repeat("b", 64), Source: strings.Repeat("a", 64), Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64), State: "deferred"}
			request := Request{Op: "resolve-after-owner", Session: s.context.Session, Target: &original}
			if mode == "missing-lease" {
				request.Binding = r.Binding
				s.ownerRelease = nil
			}
			if mode == "ordinary-target" {
				request.Target.AfterOwner = ""
			}
			raw, _ := json.Marshal(request)
			if mode == "extra-field" {
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				fields["producer"] = r.Producer
				raw, _ = json.Marshal(fields)
			}
			reply, err = s.handle(context.Background(), raw, nil)
			if mode != "valid" && mode != "deferred" {
				if err == nil && reply.Code == "" {
					t.Fatal("unproven resolution admitted", mode)
				}
				return
			}
			if err != nil || reply.Code != "" || len(reply.Targets) != 1 || reply.ResolvedRecipe == "" || fixture.calls != 1 {
				t.Fatal("closed readonly resolution refused", reply, err)
			}
			if original.InstanceUUID != "absent" || original.PreparedRecipe != strings.Repeat("b", 64) {
				t.Fatal("original target mutated")
			}
			if mode == "deferred" && reply.Targets[0].State != "deferred" {
				t.Fatal("deferred resolution became effectful")
			}
		})
	}
}

type afterOwnerFramedFixture struct {
	*afterOwnerFixture
	bound       []Target
	completions int
}

func (f *afterOwnerFramedFixture) InstalledRuntimeRecipe(string) ([]byte, error) {
	return nil, fmt.Errorf("fixture has no recipe read")
}

func (f *afterOwnerFramedFixture) BindInstalledRuntimeRecipe(context.Context, string, []byte) error {
	return nil
}
func (f *afterOwnerFramedFixture) BindInstalledRuntimeRecipeForTarget(_ context.Context, hash string, raw []byte, target Target) error {
	if digest(raw) != hash || target.PreparedRecipe != hash {
		return fmt.Errorf("framed recipe mismatch")
	}
	f.bound = append(f.bound, target)
	return nil
}
func (f *afterOwnerFramedFixture) BindAfterOwnerCompletion(AfterOwnerCompletion) error {
	f.completions++
	return nil
}

func TestAfterOwnerActualFramedSourceResolveAndDerivedBind(t *testing.T) {
	for _, kind := range []string{"materialized-files", "materialized-integrations", "runtime", "broker", "routes"} {
		t.Run(kind, func(t *testing.T) { testAfterOwnerActualFramed(t, kind) })
	}
}
func testAfterOwnerActualFramed(t *testing.T, kind string) {
	s, request, observer, base, path := ownerResultsOriginal(t)
	ownerResultsTerminal(t, s, observer, "complete")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	imageHash := fmt.Sprintf("%x", sha256.Sum256(image))
	oldHelper := request.Producer.Helper
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := filepath.Join(path, entry.Name())
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.ReplaceAll(raw, []byte(`"helper":"`+oldHelper+`"`), []byte(`"helper":"`+imageHash+`"`))
		testkit.WriteFile(t, name, raw, 0600)
	}
	producer := *request.Producer
	producer.Helper = imageHash
	originalRaw, derivedRaw := []byte("original conditional source"), []byte("derived same source and measured physical record")
	base.Components = noEffects{}
	fixture := &afterOwnerFramedFixture{afterOwnerFixture: &afterOwnerFixture{ownerResultsFixture: base, recipe: digest(derivedRaw)}}
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(sockets[0]), "after owner peer")
	defer parent.Close()
	child := os.NewFile(uintptr(sockets[1]), "after owner native")
	done := make(chan error, 1)
	go func() {
		defer child.Close()
		done <- Serve(context.Background(), child, child, func(context.Context, Context, *os.File) (Components, error) { return fixture, nil })
	}()
	root, _ := IdentityOf(s.root)
	contextValue := Context{Session: producer.Session, Artifact: producer.Artifact, ConfigHome: s.root.Name(), DataHome: s.root.Name(), RuntimeRoot: s.root.Name(), RepositoryRoot: s.root.Name(), Yard: "test-yard", Target: "target", Root: root, Repository: root}
	peer := nativeTestPeer{t: t, socket: parent}
	peer.request(Request{Op: "initialize", Context: &contextValue}, s.root, s.root)
	peer.request(Request{Op: "bind-owner-results", Session: producer.Session, Producer: &producer, Before: request.Before, Token: request.Token})
	original := Target{Owner: "materialized-config", Yard: "test-yard", Class: "materialized-files", InstanceUUID: "absent", State: "deferred", Source: producer.Artifact, Actual: producer.Artifact, Desired: producer.Artifact, PreparedRecipe: digest(originalRaw), AfterOwner: "v1:" + kind + ":" + digest(originalRaw)}
	bind := func(raw []byte, target Target) {
		t.Helper()
		reply := peer.request(Request{Op: "bind-installed-recipe", Session: producer.Session, RecipeHash: digest(raw), Total: uint64(len(raw)), Content: base64.StdEncoding.EncodeToString(raw), Target: &target})
		if reply.InstalledChunk == nil || !reply.InstalledChunk.Complete {
			t.Fatal("missing framed binding")
		}
	}
	bind(originalRaw, original)
	reply := peer.request(Request{Op: "resolve-after-owner", Session: producer.Session, Target: &original})
	if len(reply.Targets) != 1 || reply.ResolvedRecipe != digest(derivedRaw) {
		t.Fatal("missing physical resolution")
	}
	bind(derivedRaw, reply.Targets[0])
	parent.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("framed native did not drain")
	}
	if len(fixture.bound) != 2 || fixture.bound[0] != original || fixture.bound[1] != reply.Targets[0] || fixture.completions != 1 {
		t.Fatal("original/physical bind protocol changed")
	}
}

func TestAfterOwnerMetadataUsesPreparedBlobCapacity(t *testing.T) {
	f := strings.Repeat("a", 64)
	b := Binding{Session: f, Artifact: f, Helper: f, Journal: f, Plan: "plan-v1-" + f, Transaction: "original", Ordinal: 1}
	value := AfterOwnerMetadata{Version: 1, Phase: "resolved", Original: f, Producer: b, ProducerTerminal: f, Physical: make([]byte, 2<<20)}
	raw, _ := json.Marshal(value)
	decoded, err := DecodeAfterOwnerMetadata(raw)
	if err != nil || !bytes.Equal(decoded.Physical, value.Physical) {
		t.Fatal("prepared metadata incorrectly used protocol/record limit", err)
	}
	if _, err = DecodeAfterOwnerMetadata(append(raw, ' ')); err == nil {
		t.Fatal("noncanonical prepared metadata admitted")
	}
	capacity := AfterOwnerCapacity{Version: 1, Phase: "template", MetadataBytes: len(raw), DescriptorBytes: len(raw)}
	template, _ := json.Marshal(capacity)
	if _, err = DecodeAfterOwnerCapacity(template); err != nil {
		t.Fatal("exact prepared capacity refused", err)
	}
	capacity.MetadataBytes = 32<<20 + 1
	template, _ = json.Marshal(capacity)
	if _, err = DecodeAfterOwnerCapacity(template); err == nil {
		t.Fatal("existing prepared blob bound exceeded")
	}
}
