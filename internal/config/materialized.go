package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

// MaterializedAsset identifies a desired file and its policy in the developer home.
// Policy follows the consumer destination, including after import renames a source.
type MaterializedAsset struct {
	Name            string
	Setting         string
	Source          string
	Destination     string
	OwnedFormat     string
	FollowSymlinks  bool
	ApprovalSources []string
}

// MaterializedAssets resolves CONFIG and RULES assets. Host instruction files are
// separate consumers, and deliberately do not belong to `config status`'s scope.
func MaterializedAssets(values map[string]string, developer string, definitions ...profile.IntegrationDefinition) ([]MaterializedAsset, error) {
	if !domain.SafeName(developer) {
		return nil, errors.New("invalid developer user")
	}
	for _, definition := range definitions {
		if len(definition.Artifacts) != 0 && !filepath.IsAbs(definition.Root) {
			return nil, errors.New("integration artifact package source binding is unavailable")
		}
	}
	var assets []MaterializedAsset
	for _, agent := range strings.Fields(values["CODING_TOOL_INTEGRATIONS"]) {
		if !domain.SafeName(agent) {
			return nil, fmt.Errorf("invalid agent name %q", agent)
		}
		declaredKinds := map[string]bool{}
		var artifacts []profile.IntegrationArtifact
		for _, definition := range definitions {
			if definition.Name != agent || len(definition.Artifacts) == 0 {
				continue
			}
			for _, artifact := range definition.Artifacts {
				if artifact.Kind != "config" {
					continue
				}
				declaredKinds[artifact.ID] = true
				artifact.Source = filepath.Join(definition.Root, artifact.Source)
				if len(artifact.ApprovalSources) > 0 {
					sources := make(map[string]string, len(artifact.ApprovalSources))
					for gate, path := range artifact.ApprovalSources {
						sources[gate] = filepath.Join(definition.Root, path)
					}
					artifact.ApprovalSources = sources
				}
				artifacts = append(artifacts, artifact)
			}
		}
		for _, kind := range []string{"config", "rules"} {
			if declaredKinds[kind] {
				continue
			}
			format := "raw"
			if kind == "config" {
				format = "destination"
			}
			artifacts = append(artifacts, profile.IntegrationArtifact{ID: kind, Format: format})
		}
		for _, artifact := range artifacts {
			setting := "AGENT_" + agent + "_" + strings.ToUpper(artifact.ID)
			source, present := values[setting]
			if !present {
				source = artifact.Source
			}
			destination, present := values[setting+"_DEST"]
			if !present {
				destination = artifact.Destination
			}
			if source == "" || destination == "" {
				continue
			}
			clean, err := artifactDestination(developer, destination)
			if err != nil {
				return nil, err
			}
			format := artifact.Format
			if format == "raw" {
				format = ""
			} else if format == "destination" {
				format = ownedDestinationFormat(clean)
			}
			asset := MaterializedAsset{Name: agent + "." + artifact.ID, Setting: setting, Source: source, Destination: clean, OwnedFormat: format}
			if source == artifact.Source {
				for _, gate := range []struct {
					name   string
					active bool
				}{
					{"git", !GitApprovalOff(values["CODING_TOOL_GIT_APPROVAL"])},
					{"sudo", SudoApprovalPrompt(values["DEV_SUDO"])},
				} {
					if path := artifact.ApprovalSources[gate.name]; gate.active && path != "" {
						asset.ApprovalSources = append(asset.ApprovalSources, path)
						assets = append(assets, MaterializedAsset{Name: asset.Name + "." + gate.name,
							Source: path, Destination: filepath.Join(filepath.Dir(clean), filepath.Base(path))})
					}
				}
			}
			assets = append(assets, asset)
		}
	}
	destinations := make([]string, 0, len(assets))
	for _, asset := range assets {
		destinations = append(destinations, asset.Destination)
	}
	if err := validateManagedDestinations(destinations, values["INTEGRATION_HOST_LINKS"], filepath.Join("/home", developer)); err != nil {
		return nil, err
	}
	return assets, nil
}

// ReadContent composes installed JSON overlays using the same source reader as
// the base and the independently managed copies of each overlay.
func (asset MaterializedAsset) ReadContent(read func(MaterializedAsset) ([]byte, error)) ([]byte, error) {
	payload, err := read(MaterializedAsset{Source: asset.Source, FollowSymlinks: asset.FollowSymlinks})
	if err != nil || len(asset.ApprovalSources) == 0 {
		return payload, err
	}
	for _, path := range asset.ApprovalSources {
		fragment, err := read(MaterializedAsset{Source: path})
		if err != nil {
			return nil, fmt.Errorf("read approval overlay: %v", err)
		}
		payload, err = mergeApprovalJSON(payload, fragment)
		if err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func mergeApprovalJSON(base, overlay json.RawMessage) (json.RawMessage, error) {
	var fields, extra map[string]json.RawMessage
	if json.Unmarshal(base, &fields) != nil || fields == nil ||
		json.Unmarshal(overlay, &extra) != nil || extra == nil {
		return nil, errors.New("approval overlay requires JSON objects")
	}
	for name, value := range extra {
		if previous := fields[name]; len(previous) > 0 && previous[0] == '{' && value[0] == '{' {
			merged, err := mergeApprovalJSON(previous, value)
			if err != nil {
				return nil, err
			}
			value = merged
		}
		fields[name] = value
	}
	payload, err := json.MarshalIndent(fields, "", "  ")
	return append(payload, '\n'), err
}

// validateManagedDestinations keeps files and managed links from replacing each
// other or their parent directories. Explicit operator HOST_LINKS are separate.
func validateManagedDestinations(destinations []string, links, home string) error {
	if err := validateLinkList(links); err != nil {
		return err
	}
	for _, link := range strings.Fields(links) {
		relative, _, _ := strings.Cut(link, ":")
		destinations = append(destinations, filepath.Join(home, relative))
	}
	for i, destination := range destinations {
		for _, previous := range destinations[:i] {
			if profile.ArtifactDestinationsOverlap(previous, destination) {
				return errors.New("managed artifact destination collision")
			}
		}
	}
	return nil
}

func artifactDestination(developer, destination string) (string, error) {
	clean := filepath.Clean(destination)
	if filepath.IsAbs(destination) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("artifact destination leaves the developer home")
	}
	return filepath.Join("/home", developer, clean), nil
}

// ReadSource opens the validated inode without following config symlinks or
// blocking on a substituted FIFO. Explicit host instruction links remain supported.
func (asset MaterializedAsset) ReadSource() ([]byte, error) {
	info, err := os.Lstat(asset.Source)
	if err != nil {
		return nil, err
	}
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !asset.FollowSymlinks {
		flags |= syscall.O_NOFOLLOW
		if !info.Mode().IsRegular() {
			return nil, errors.New("source is not a regular file")
		}
	}
	file, err := os.OpenFile(asset.Source, flags, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("source is not a regular file")
		}
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !asset.FollowSymlinks && !os.SameFile(info, opened) {
		return nil, errors.New("source is not a regular file")
	}
	return io.ReadAll(file)
}

func ownedDestinationFormat(destination string) string {
	switch filepath.Ext(destination) {
	case ".json":
		return "json"
	case ".toml":
		return "toml"
	}
	return ""
}

// GitApprovalOff resolves the mode before any applied snapshot projection.
func GitApprovalOff(value string) bool {
	return value == "" || value == "off"
}

// SudoApprovalPrompt reports whether DEV_SUDO asks coding agents to prompt before
// escalation. Unset means the default, prompt.
func SudoApprovalPrompt(value string) bool {
	return value == "" || value == "prompt"
}
