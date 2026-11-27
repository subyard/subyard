package profile

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// IntegrationArtifact is semantic data; native sources are bound by the package loader.
// Modes and ownership retain the existing raw and structured writer policies.
// ApprovalSources are JSON overlays on the shipped Source, one per prompt gate.
// A "git sudo" entry identifies the published schema-2 complete-variant format;
// those sources replace Source for the exact set of active gates.
type IntegrationArtifact struct {
	ID              string            `json:"id"`
	Label           string            `json:"label,omitempty"`
	Kind            string            `json:"kind"`
	Source          string            `json:"source,omitempty"`
	ApprovalSources map[string]string `json:"approval_sources,omitempty"`
	// SourceSetting exists only to decode retired instruction artifacts.
	SourceSetting string `json:"source_setting,omitempty"`
	Destination   string `json:"destination"`
	Format        string `json:"format"`
}

// PackageSources lists the shipped variants that must be validated and bound.
func (artifact IntegrationArtifact) PackageSources() []string {
	if artifact.Source == "" {
		return nil
	}
	sources := []string{artifact.Source}
	for _, gates := range slices.Sorted(maps.Keys(artifact.ApprovalSources)) {
		sources = append(sources, artifact.ApprovalSources[gates])
	}
	return sources
}

func (definition IntegrationDefinition) validateArtifacts() error {
	if len(definition.Artifacts) == 0 {
		return nil
	}
	if definition.SchemaVersion != 2 || definition.Retired || len(definition.Artifacts) > 16 {
		return errors.New("artifacts require active integration schema 2")
	}
	ids, destinations := map[string]bool{}, map[string]bool{}
	for _, artifact := range definition.Artifacts {
		if !textSafe(artifact.Label) || len(artifact.Label) > 100 || !safeIdentifier(artifact.ID) || ids[artifact.ID] || (!textSafe(artifact.Destination) || !relative(artifact.Destination)) || destinations[artifact.Destination] {
			return errors.New("invalid or duplicate integration artifact")
		}
		for destination := range destinations {
			if ArtifactDestinationsOverlap(destination, artifact.Destination) {
				return errors.New("integration artifact destination collision")
			}
		}
		ids[artifact.ID], destinations[artifact.Destination] = true, true
		if artifact.Format != "raw" && artifact.Format != "json" && artifact.Format != "toml" && artifact.Format != "destination" {
			return errors.New("invalid integration artifact format")
		}
		for gate, path := range artifact.ApprovalSources {
			if artifact.Kind != "config" || (gate != "git" && gate != "sudo" && gate != "git sudo") || !textSafe(path) || !relative(path) {
				return errors.New("invalid approval artifact source")
			}
		}
		switch artifact.Kind {
		case "config":
			if (artifact.ID != "config" && artifact.ID != "rules") || (!textSafe(artifact.Source) || !relative(artifact.Source)) || artifact.SourceSetting != "" {
				return errors.New("invalid config artifact source")
			}
		default:
			return errors.New("unsupported integration artifact kind")
		}
	}
	return nil
}

// retiredInstructionCopies maps each setting that named a host instruction file
// in releases through 0.32.0 to the yard path, relative to the developer's home,
// that its copy was written to.
var retiredInstructionCopies = map[string]string{
	"HOST_CLAUDE_MD":          ".claude/CLAUDE.md",
	"HOST_CODEX_AGENTS_MD":    ".codex/AGENTS.md",
	"HOST_OPENCODE_AGENTS_MD": ".config/opencode/AGENTS.md",
}

// RetiredInstructionCopies are the sorted yard paths of retired host instruction copies.
func RetiredInstructionCopies() []string {
	return slices.Sorted(maps.Values(retiredInstructionCopies))
}

// RetiredInstructionSetting reports a setting that named a retired host instruction file.
func RetiredInstructionSetting(name string) bool {
	_, retired := retiredInstructionCopies[name]
	return retired
}

// retiredInstructionArtifact is a host instruction copy that published releases
// through 0.32.0 declare. Rollback preparation reads those target declarations,
// so decoding drops exactly those three shapes instead of refusing them.
func retiredInstructionArtifact(artifact IntegrationArtifact) bool {
	destination, retired := retiredInstructionCopies[artifact.SourceSetting]
	return retired && artifact.Kind == "instructions" && artifact.ID == "instructions" && artifact.Source == "" &&
		len(artifact.ApprovalSources) == 0 && artifact.Destination == destination && artifact.Format == "raw"
}

// ArtifactSourcePath rechecks the physical package boundary before source use.
func (definition IntegrationDefinition) ArtifactSourcePath(source string) (string, error) {
	path, err := definition.packageDefinition().sourcePath(source, false)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !safeArtifactFile(info) {
		return "", errors.New("integration artifact source must be bounded and singly linked")
	}
	return filepath.Clean(path), nil
}

func safeArtifactFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && stat.Nlink == 1 && info.Mode().Perm()&0o022 == 0 && info.Size() <= 4<<20
}

// ArtifactDestinationsOverlap rejects files that would need the same path to be
// both a regular file and a parent directory. Inputs are validated clean paths.
func ArtifactDestinationsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}
