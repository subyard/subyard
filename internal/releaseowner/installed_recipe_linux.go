package releaseowner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
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
	// Strict decoding checks padding bits; reject newlines explicitly because
	// the standard decoder otherwise ignores them.
	if err != nil || len(payload) == 0 || strings.ContainsAny(r.Content, "\r\n") || len(payload) > installedRecipeChunkSize || r.Offset+uint64(len(payload)) > r.Total {
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
		if reply, err := s.bindInstalledBytes(ctx, components, t.hash, t.bytes, r.Target); err != nil || reply.Code != "" {
			return reply, err
		}
	}
	return Reply{InstalledChunk: &InstalledRecipeChunk{RecipeHash: r.RecipeHash, Offset: r.Offset, Total: r.Total, Complete: complete}}, nil
}

func (s *session) bindInstalledBytes(ctx context.Context, components installedRecipeComponents, hash string, raw []byte, target *Target) (Reply, error) {
	if digest(raw) != hash {
		return Reply{}, errors.New("installed recipe digest")
	}
	if target != nil {
		binder, ok := s.components.(interface {
			BindInstalledRuntimeRecipeForTarget(context.Context, string, []byte, Target) error
		})
		if !ok {
			return Reply{}, errors.New("installed target recipe capability")
		}
		if target.AfterOwner != "" {
			_, original, err := ParseAfterOwner(target.AfterOwner)
			if err != nil {
				return Reply{}, err
			}
			if original != hash {
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
		if err := binder.BindInstalledRuntimeRecipeForTarget(ctx, hash, raw, *target); err != nil {
			return unavailable(err)
		}
	} else if err := components.BindInstalledRuntimeRecipe(ctx, hash, raw); err != nil {
		return unavailable(err)
	}
	var header struct {
		Owner string `json:"owner"`
	}
	if json.Unmarshal(raw, &header) == nil && header.Owner == "owner-registration" {
		s.ownerOrigins = nil
		s.ownerCompleted = nil
		s.ownerCompletedTerminal = ""
		s.ownerResultsPayload = ""
		s.ownerResultsRecipe = ""
		s.ownerResultsSource = ""
	}
	return Reply{}, nil
}

func (s *session) bindSealedInstalledRecipe(ctx context.Context, r Request, file *os.File) (Reply, error) {
	components, ok := s.components.(installedRecipeComponents)
	if !ok || !fingerprint(r.RecipeHash) || r.Total == 0 || r.Total > installedRecipeMaximum || s.installedTransfer != nil ||
		(r.Target != nil && r.Target.PreparedRecipe != r.RecipeHash) {
		return Reply{}, errors.New("sealed installed recipe binding")
	}
	var stat unix.Stat_t
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	const required = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if err != nil || seals&required != required || unix.Fstat(int(file.Fd()), &stat) != nil ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Nlink != 0 ||
		stat.Uid != uint32(os.Getuid()) || stat.Size <= 0 || uint64(stat.Size) != r.Total {
		return Reply{}, errors.New("sealed installed recipe descriptor")
	}
	raw := make([]byte, int(r.Total))
	if _, err := io.ReadFull(io.NewSectionReader(file, 0, stat.Size), raw); err != nil {
		return Reply{}, errors.New("sealed installed recipe read")
	}
	if reply, err := s.bindInstalledBytes(ctx, components, r.RecipeHash, raw, r.Target); err != nil || reply.Code != "" {
		return reply, err
	}
	return Reply{InstalledChunk: &InstalledRecipeChunk{RecipeHash: r.RecipeHash, Total: r.Total, Complete: true}}, nil
}
