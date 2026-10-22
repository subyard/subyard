package profile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// IntegrationArtifact is semantic data; native sources are bound by the package loader.
// Modes and ownership retain the existing raw and structured writer policies.
type IntegrationArtifact struct {
	ID            string `json:"id"`
	Label         string `json:"label,omitempty"`
	Kind          string `json:"kind"`
	Source        string `json:"source,omitempty"`
	SourceSetting string `json:"source_setting,omitempty"`
	Destination   string `json:"destination"`
	Format        string `json:"format"`
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
		switch artifact.Kind {
		case "config":
			if (artifact.ID != "config" && artifact.ID != "rules") || (!textSafe(artifact.Source) || !relative(artifact.Source)) || artifact.SourceSetting != "" {
				return errors.New("invalid config artifact source")
			}
		case "instructions":
			if artifact.ID != "instructions" || artifact.Source != "" || !settingNamePattern.MatchString(artifact.SourceSetting) || artifact.Format != "raw" {
				return errors.New("invalid instruction artifact source")
			}
		default:
			return errors.New("unsupported integration artifact kind")
		}
	}
	return nil
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
