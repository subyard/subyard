package cli

import (
	"bytes"
	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"os"
	"path/filepath"
)

// This is the original owning virtual-input provenance, not a callback or a
// path authority supplied during execution. Only preparation can construct it.
type preparedOwnerInputProjection struct {
	root     string
	pinned   *os.File
	approved map[string][]byte
}

func prepareOwnerInputProjection(baseline *ownerInputBaseline, files *config.ProspectiveFiles, pinned *os.File) (*preparedOwnerInputProjection, error) {
	if baseline == nil || files == nil || pinned == nil || files.Root != baseline.initial.Context.Paths.ConfigHome {
		return nil, domain.ErrPlanStale
	}
	p := &preparedOwnerInputProjection{root: files.Root, pinned: pinned, approved: map[string][]byte{}}
	if files.ReadPreparedInput == nil {
		return p, nil
	}
	for path := range baseline.files {
		relative, err := filepath.Rel(p.root, path)
		if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == "../" {
			continue
		}
		snapshot, matched, err := files.ReadPreparedInput(path)
		if err != nil {
			return nil, err
		}
		if matched && snapshot.Exists && snapshot.Identity == (config.PersistentFileIdentity{}) {
			p.approved[path] = bytes.Clone(snapshot.Content)
		}
	}
	return p, nil
}

func (p *preparedOwnerInputProjection) actual(path string) ([]byte, error) {
	if err := p.checkRoot(); err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(p.root, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == "../" {
		return nil, domain.ErrPlanStale
	}
	snapshot, err := config.ReadPersistentSnapshotAt(p.pinned, filepath.ToSlash(relative))
	if err != nil {
		return nil, err
	}
	if !snapshot.Exists {
		return nil, domain.ErrPlanStale
	}
	return snapshot.Content, nil
}

func (p *preparedOwnerInputProjection) checkRoot() error {
	if p == nil || p.pinned == nil {
		return domain.ErrPlanStale
	}
	root, err := os.Lstat(p.root)
	if err != nil {
		return err
	}
	retained, err := p.pinned.Stat()
	if err != nil {
		return err
	}
	if root.Mode()&os.ModeSymlink != 0 || !root.IsDir() || !os.SameFile(root, retained) {
		return domain.ErrPlanStale
	}
	return nil
}

func (p *preparedOwnerInputProjection) validate(actual, original map[string]ownerInputFile) error {
	if p == nil {
		return errors.New("prepared owner projection unavailable")
	}
	if err := p.checkRoot(); err != nil {
		return domain.ErrPlanStale
	}
	for path, approved := range p.approved {
		before, ok := original[path]
		if !ok {
			return domain.ErrPlanStale
		}
		current, ok := actual[path]
		if !ok {
			return domain.ErrPlanStale
		}
		if current == before {
			continue
		}
		body, err := p.actual(path)
		if err != nil || !bytes.Equal(body, approved) {
			return domain.ErrPlanStale
		}
		actual[path] = before
	}
	return nil
}

// Future init is callable only after the owning registration publication.
// An unchanged absent/preimage snapshot is sufficient for readonly prepare,
// but never for executing a child against its future canonical destination.
func (p *preparedOwnerInputProjection) requirePublished() error {
	if p == nil {
		return domain.ErrPlanStale
	}
	for path, approved := range p.approved {
		body, err := p.actual(path)
		if err != nil || !bytes.Equal(body, approved) {
			return domain.ErrPlanStale
		}
	}
	return nil
}
