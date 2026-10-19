package releaseowner

import (
	"bytes"
	"context"
	"encoding/base64"
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
