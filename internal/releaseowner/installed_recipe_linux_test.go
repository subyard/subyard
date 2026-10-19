package releaseowner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

type installedRecipeFixture struct {
	noEffects
	raw, bound []byte
}

func (f *installedRecipeFixture) InstalledRuntimeRecipe(hash string) ([]byte, error) {
	if digest(f.raw) != hash {
		return nil, errors.New("unknown recipe")
	}
	return f.raw, nil
}
func (f *installedRecipeFixture) BindInstalledRuntimeRecipe(_ context.Context, hash string, raw []byte) error {
	if digest(raw) != hash || !bytes.Equal(raw, f.raw) {
		return errors.New("wrong original recipe")
	}
	f.bound = bytes.Clone(raw)
	return nil
}

func TestInstalledRecipeTransferRetainsExactOriginalAndRefusesConflicts(t *testing.T) {
	fixture := &installedRecipeFixture{raw: bytes.Repeat([]byte("original approved source"), 30000)}
	s := &session{components: fixture}
	hash := digest(fixture.raw)
	var offset uint64
	for offset < uint64(len(fixture.raw)) {
		reply, err := s.installedRecipe(context.Background(), Request{Op: "read-installed-recipe", RecipeHash: hash, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		chunk := reply.InstalledChunk
		bound, err := s.installedRecipe(context.Background(), Request{Op: "bind-installed-recipe", RecipeHash: hash, Offset: offset, Total: chunk.Total, Content: chunk.Content})
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := base64.StdEncoding.DecodeString(chunk.Content)
		offset += uint64(len(payload))
		if bound.InstalledChunk.Complete != (offset == uint64(len(fixture.raw))) {
			t.Fatal("premature recipe completion")
		}
	}
	if !bytes.Equal(fixture.raw, fixture.bound) || s.installedTransfer != nil {
		t.Fatal("original recipe not restored")
	}
	request := Request{Op: "bind-installed-recipe", RecipeHash: hash, Total: uint64(len(fixture.raw)), Content: base64.StdEncoding.EncodeToString(fixture.raw[:installedRecipeChunkSize])}
	if _, err := s.installedRecipe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.installedRecipe(context.Background(), request); err == nil {
		t.Fatal("overlap admitted")
	}
	request.Offset = installedRecipeChunkSize
	request.RecipeHash = digest([]byte("different"))
	if _, err := s.installedRecipe(context.Background(), request); err == nil {
		t.Fatal("interleaved recipe admitted")
	}
}

type targetRecipeFixture struct {
	installedRecipeFixture
	target *Target
}

func (f *targetRecipeFixture) BindInstalledRuntimeRecipeForTarget(_ context.Context, hash string, raw []byte, target Target) error {
	if digest(raw) != hash || !bytes.Equal(raw, f.raw) {
		return errors.New("original bytes changed")
	}
	f.target = &target
	f.bound = bytes.Clone(raw)
	return nil
}
func TestInstalledRecipeOriginalTargetOnlyOnFinalChunk(t *testing.T) {
	f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: bytes.Repeat([]byte("x"), installedRecipeChunkSize+3)}}
	hash := digest(f.raw)
	target := Target{PreparedRecipe: hash, InstanceUUID: "absent", Owner: "original-owner", Source: digest([]byte("source"))}
	request := Request{Op: "bind-installed-recipe", RecipeHash: hash, Total: uint64(len(f.raw)), Content: base64.StdEncoding.EncodeToString(f.raw[:installedRecipeChunkSize]), Target: &target}
	s := &session{components: f}
	if _, err := s.installedRecipe(context.Background(), request); err == nil || s.installedTransfer != nil || f.target != nil {
		t.Fatal("early target gained authority")
	}
	request.Target = nil
	if _, err := s.installedRecipe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Offset = installedRecipeChunkSize
	request.Content = base64.StdEncoding.EncodeToString(f.raw[installedRecipeChunkSize:])
	request.Target = &target
	if _, err := s.installedRecipe(context.Background(), request); err != nil || f.target == nil || *f.target != target || !bytes.Equal(f.bound, f.raw) {
		t.Fatal("final original target/body lost", err)
	}
	wrong := target
	wrong.PreparedRecipe = digest([]byte("foreign"))
	request.Offset = 0
	request.Target = &wrong
	if _, err := s.installedRecipe(context.Background(), request); err == nil {
		t.Fatal("foreign recipe target accepted")
	}
}

func TestInstalledRecipeTargetPassesStrictSessionAdmission(t *testing.T) {
	s, b, _ := nativeFactFixture(t)
	root, err := IdentityOf(s.root)
	if err != nil {
		t.Fatal(err)
	}
	s.context.Root = root
	f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: []byte("original body")}}
	s.components = f
	hash := digest(f.raw)
	target := Target{PreparedRecipe: hash, InstanceUUID: "absent"}
	raw, _ := json.Marshal(Request{Op: "bind-installed-recipe", Session: b.Session, RecipeHash: hash, Total: uint64(len(f.raw)), Content: base64.StdEncoding.EncodeToString(f.raw), Target: &target})
	if reply, err := s.handle(context.Background(), raw, nil); err != nil || reply.InstalledChunk == nil || !reply.InstalledChunk.Complete || f.target == nil || *f.target != target {
		t.Fatal("final target refused at strict admission", err)
	}
	f.target = nil
	raw, _ = json.Marshal(Request{Op: "bind-installed-recipe", Session: b.Session, RecipeHash: hash, Total: uint64(len(f.raw) + 1), Content: base64.StdEncoding.EncodeToString(f.raw), Target: &target})
	if _, err = s.handle(context.Background(), raw, nil); err == nil || f.target != nil || s.installedTransfer != nil {
		t.Fatal("nonfinal target passed strict admission")
	}
}
