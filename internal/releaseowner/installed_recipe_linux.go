package releaseowner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
)

const installedRecipeChunkSize = 512 << 10
const installedRecipeMaximum = 96 << 20

type installedRecipeTransfer struct {
	hash  string
	total uint64
	bytes []byte
}
type installedRecipeComponents interface {
	InstalledRuntimeRecipe(string) ([]byte, error)
	BindInstalledRuntimeRecipe(context.Context, string, []byte) error
}

func (s *session) installedRecipe(ctx context.Context, r Request) (Reply, error) {
	components, ok := s.components.(installedRecipeComponents)
	if !ok || !fingerprint(r.RecipeHash) {
		return Reply{}, errors.New("installed recipe capability")
	}
	if r.Op == "read-installed-recipe" {
		if s.installedRead == nil || s.installedRead.hash != r.RecipeHash {
			raw, err := components.InstalledRuntimeRecipe(r.RecipeHash)
			if err != nil {
				return Reply{}, err
			}
			if len(raw) == 0 || len(raw) > installedRecipeMaximum || digest(raw) != r.RecipeHash {
				return Reply{}, errors.New("installed recipe read bound")
			}
			s.installedRead = &installedRecipeTransfer{hash: r.RecipeHash, total: uint64(len(raw)), bytes: raw}
		}
		raw := s.installedRead.bytes
		// The retained immutable buffer was hashed once above. Rehashing every
		// chunk makes a bounded large recipe quadratic in transferred bytes.
		if len(raw) == 0 || len(raw) > installedRecipeMaximum || r.Offset >= uint64(len(raw)) || r.Offset%installedRecipeChunkSize != 0 {
			return Reply{}, errors.New("installed recipe read bound")
		}
		end := min(r.Offset+installedRecipeChunkSize, uint64(len(raw)))
		if end == uint64(len(raw)) {
			defer func() { s.installedRead = nil }()
		}
		return Reply{InstalledChunk: &InstalledRecipeChunk{r.RecipeHash, r.Offset, uint64(len(raw)), base64.StdEncoding.EncodeToString(raw[r.Offset:end]), end == uint64(len(raw))}}, nil
	}
	if r.Total == 0 || r.Total > installedRecipeMaximum || len(r.Content) > base64.StdEncoding.EncodedLen(installedRecipeChunkSize) {
		return Reply{}, errors.New("installed recipe transfer bound")
	}
	if r.Target != nil && (r.Op != "bind-installed-recipe" || r.Target.PreparedRecipe != r.RecipeHash) {
		return Reply{}, errors.New("installed recipe target binding")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(r.Content)
	if err != nil || len(payload) == 0 || base64.StdEncoding.EncodeToString(payload) != r.Content || len(payload) > installedRecipeChunkSize || r.Offset+uint64(len(payload)) > r.Total {
		return Reply{}, errors.New("installed recipe chunk")
	}
	if r.Target != nil && r.Offset+uint64(len(payload)) != r.Total {
		return Reply{}, errors.New("installed recipe target requires final chunk")
	}
	if r.Offset+uint64(len(payload)) < r.Total && len(payload) != installedRecipeChunkSize {
		return Reply{}, errors.New("short installed recipe chunk")
	}
	if s.installedTransfer == nil {
		if r.Offset != 0 {
			return Reply{}, errors.New("installed recipe initial offset")
		}
		s.installedTransfer = &installedRecipeTransfer{hash: r.RecipeHash, total: r.Total}
	}
	t := s.installedTransfer
	if t.hash != r.RecipeHash || t.total != r.Total || r.Offset != uint64(len(t.bytes)) {
		return Reply{}, errors.New("conflicting installed recipe transfer")
	}
	t.bytes = append(t.bytes, payload...)
	complete := uint64(len(t.bytes)) == t.total
	if complete {
		defer func() { s.installedTransfer = nil }()
		if digest(t.bytes) != t.hash {
			return Reply{}, errors.New("installed recipe digest")
		}
		if r.Target != nil {
			binder, ok := s.components.(interface {
				BindInstalledRuntimeRecipeForTarget(context.Context, string, []byte, Target) error
			})
			if !ok {
				return Reply{}, errors.New("installed target recipe capability")
			}
			if r.Target.AfterOwner != "" {
				_, original, err := ParseAfterOwner(r.Target.AfterOwner)
				if err != nil {
					return Reply{}, err
				}
				if original != r.RecipeHash {
					complete, err := s.ownerCompletion()
					if err != nil {
						return Reply{Code: "unknown"}, nil
					}
					receiver, ok := s.components.(interface {
						BindAfterOwnerCompletion(AfterOwnerCompletion) error
					})
					if !ok {
						return Reply{}, errors.New("after-owner completion capability")
					}
					if err := receiver.BindAfterOwnerCompletion(complete); err != nil {
						return Reply{}, err
					}
				}
			}
			if err := binder.BindInstalledRuntimeRecipeForTarget(ctx, t.hash, t.bytes, *r.Target); err != nil {
				return Reply{}, err
			}
		} else if err := components.BindInstalledRuntimeRecipe(ctx, t.hash, t.bytes); err != nil {
			return Reply{}, err
		}
		var header struct {
			Owner string `json:"owner"`
		}
		if json.Unmarshal(t.bytes, &header) == nil && header.Owner == "owner-registration" {
			s.ownerOrigins = nil
			s.ownerCompleted = nil
			s.ownerCompletedTerminal = ""
			s.ownerResultsPayload = ""
			s.ownerResultsRecipe = ""
			s.ownerResultsSource = ""
		}
	}
	return Reply{InstalledChunk: &InstalledRecipeChunk{RecipeHash: r.RecipeHash, Offset: r.Offset, Total: r.Total, Complete: complete}}, nil
}
