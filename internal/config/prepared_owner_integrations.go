package config

import (
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
)

type ownerIntegrationWriteRecord struct {
	Version                          int
	Root, Yard, Path, Source         string
	Before, SourceBefore, FlatBefore PersistentFileSnapshot
	Content                          []byte
}

// RecordOwnerIntegrationWrite retains the selected original CAS and source.
// It does not discover a registration or choose a replacement target.
func RecordOwnerIntegrationWrite(plan *YardIntegrationWrite) ([]byte, error) {
	if plan == nil {
		return nil, nil
	}
	if err := plan.Check(); err != nil {
		return nil, err
	}
	d := ownerIntegrationWriteRecord{1, plan.root, plan.yard, plan.Path, plan.SourcePath,
		plan.Before, plan.SourceBefore, plan.FlatBefore, plan.Content}
	return nativejson.EncodeLosslessNativeJSON(d, 32<<20)
}

func RestoreOwnerIntegrationWrite(raw []byte, configDir, root, yard string) (*YardIntegrationWrite, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var d ownerIntegrationWriteRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &d, 32<<20); err != nil {
		return nil, err
	}
	if d.Version != 1 || !filepath.IsAbs(root) || !domain.SafeName(yard) || d.Root != root || d.Yard != yard || d.Path != filepath.Join(root, "yards", yard, "config.env") {
		return nil, ErrPersistentTargetStale
	}
	if d.Source != "" {
		candidates := append(YardFileCandidates(configDir, root, yard), filepath.Join(root, GitSettingsRelativePath, "yards", yard, "config.env"))
		if !slices.Contains(candidates, d.Source) {
			return nil, ErrPersistentTargetStale
		}
	}
	p := &YardIntegrationWrite{Path: d.Path, Before: d.Before, Content: d.Content, SourcePath: d.Source, SourceBefore: d.SourceBefore, FlatBefore: d.FlatBefore, root: root, yard: yard}
	if err := p.Check(); err != nil {
		return nil, err
	}
	return p, nil
}
