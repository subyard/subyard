package config

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

// MaterializedAsset identifies a desired file and its policy in the developer home.
// Policy follows the consumer destination, including after import renames a source.
type MaterializedAsset struct {
	Name           string
	Setting        string
	Source         string
	Destination    string
	OwnedFormat    string
	FollowSymlinks bool
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
				if artifact.GitApprovalOffSource != "" {
					artifact.GitApprovalOffSource = filepath.Join(definition.Root, artifact.GitApprovalOffSource)
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
			if source == artifact.Source && artifact.GitApprovalOffSource != "" && values["CODING_TOOL_GIT_APPROVAL"] == "off" {
				source = artifact.GitApprovalOffSource
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
			assets = append(assets, MaterializedAsset{Name: agent + "." + artifact.ID, Setting: setting, Source: source, Destination: clean, OwnedFormat: format})
		}
	}
	if err := validateMaterializedDestinations(assets, values, developer, definitions); err != nil {
		return nil, err
	}
	return assets, nil
}

func validateMaterializedDestinations(assets []MaterializedAsset, values map[string]string, developer string, definitions []profile.IntegrationDefinition) error {
	destinations := make([]string, 0, len(assets))
	for _, asset := range assets {
		destinations = append(destinations, asset.Destination)
	}
	for _, reserved := range StructuredConfigDestinations(values, developer, definitions...) {
		if !slices.ContainsFunc(assets, func(asset MaterializedAsset) bool {
			return asset.OwnedFormat != "" && asset.Destination == reserved
		}) {
			destinations = append(destinations, reserved)
		}
	}
	return validateManagedDestinations(destinations, values["INTEGRATION_HOST_LINKS"], filepath.Join("/home", developer))
}

// SelectedStructuredConfigDestinations scopes permission requests to selected
// integrations. The guest independently checks each destination's receipt.
func SelectedStructuredConfigDestinations(values map[string]string, developer string, definitions ...profile.IntegrationDefinition) []string {
	selected := strings.Fields(values["CODING_TOOL_INTEGRATIONS"])
	return StructuredConfigDestinations(values, developer, slices.DeleteFunc(slices.Clone(definitions), func(definition profile.IntegrationDefinition) bool {
		return !slices.Contains(selected, definition.Name)
	})...)
}

// StructuredConfigDestinations includes declared native destinations even when
// a template source is disabled. Another owner can still contribute there.
func StructuredConfigDestinations(values map[string]string, developer string, definitions ...profile.IntegrationDefinition) []string {
	if !domain.SafeName(developer) {
		return nil
	}
	destinations := map[string]bool{}
	for _, definition := range definitions {
		for _, artifact := range definition.Artifacts {
			if artifact.Kind != "config" || artifact.Format == "raw" {
				continue
			}
			setting := "AGENT_" + definition.Name + "_" + strings.ToUpper(artifact.ID) + "_DEST"
			for _, destination := range []string{artifact.Destination, values[setting]} {
				clean, err := artifactDestination(developer, destination)
				if err == nil && (artifact.Format != "destination" || ownedDestinationFormat(clean) != "") {
					destinations[clean] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(destinations))
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
