package config

import (
	"bytes"
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
				_, variants := artifact.ApprovalSources["git sudo"]
				var activeGates []string
				for _, gate := range []struct {
					name   string
					active bool
				}{
					{"git", !GitApprovalOff(values["CODING_TOOL_GIT_APPROVAL"])},
					{"sudo", SudoApprovalPrompt(values["DEV_SUDO"])},
				} {
					if gate.active {
						activeGates = append(activeGates, gate.name)
					}
					if path := artifact.ApprovalSources[gate.name]; !variants && gate.active && path != "" {
						asset.ApprovalSources = append(asset.ApprovalSources, path)
					}
				}
				if variant := artifact.ApprovalSources[strings.Join(activeGates, " ")]; variants && variant != "" {
					asset.Source = variant
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

// ReadContent merges the shipped approval fragments into the base with the same
// source reader. Fragments are not installed on their own: the agent reads only
// the composed file. Key order follows the base, then each fragment in turn.
func (asset MaterializedAsset) ReadContent(read func(MaterializedAsset) ([]byte, error)) ([]byte, error) {
	payload, err := read(MaterializedAsset{Source: asset.Source, FollowSymlinks: asset.FollowSymlinks})
	if err != nil || len(asset.ApprovalSources) == 0 {
		return payload, err
	}
	merged, err := parseApprovalObject(payload)
	if err != nil {
		return nil, fmt.Errorf("approval policy base %s: %v", asset.Source, err)
	}
	for _, path := range asset.ApprovalSources {
		fragment, err := read(MaterializedAsset{Source: path})
		if err != nil {
			return nil, fmt.Errorf("read approval overlay: %v", err)
		}
		overlay, err := parseApprovalObject(fragment)
		if err != nil {
			return nil, fmt.Errorf("approval overlay %s: %v", path, err)
		}
		merged.merge(overlay)
	}
	var out strings.Builder
	merged.render(&out, "")
	out.WriteByte('\n')
	return []byte(out.String()), nil
}

// approvalObject is a JSON object that keeps its key order; nested objects are
// parsed so that overlays merge into them instead of replacing them.
type approvalObject struct {
	keys   []string
	values map[string]json.RawMessage
	nested map[string]*approvalObject
}

func parseApprovalObject(payload []byte) (*approvalObject, error) {
	if !json.Valid(payload) {
		return nil, errors.New("approval policies must be plain JSON objects; comments and trailing commas are not supported")
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("approval policies must be JSON objects")
	}
	object := &approvalObject{values: map[string]json.RawMessage{}, nested: map[string]*approvalObject{}}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if _, seen := object.values[key]; seen {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		object.keys = append(object.keys, key)
		object.values[key] = value
		if value[0] == '{' {
			if object.nested[key], err = parseApprovalObject(value); err != nil {
				return nil, err
			}
		}
	}
	return object, nil
}

func (object *approvalObject) merge(overlay *approvalObject) {
	for _, key := range overlay.keys {
		if _, present := object.values[key]; !present {
			object.keys = append(object.keys, key)
		}
		if base, extra := object.nested[key], overlay.nested[key]; base != nil && extra != nil {
			base.merge(extra)
			continue
		}
		object.values[key] = overlay.values[key]
		object.nested[key] = overlay.nested[key]
	}
}

// render matches json.MarshalIndent with a two-space indent.
func (object *approvalObject) render(out *strings.Builder, indent string) {
	if len(object.keys) == 0 {
		out.WriteString("{}")
		return
	}
	out.WriteString("{")
	for index, key := range object.keys {
		if index > 0 {
			out.WriteString(",")
		}
		name, _ := json.Marshal(key)
		out.WriteString("\n" + indent + "  " + string(name) + ": ")
		if nested := object.nested[key]; nested != nil {
			nested.render(out, indent+"  ")
			continue
		}
		var value bytes.Buffer
		_ = json.Indent(&value, object.values[key], indent+"  ", "  ")
		out.Write(value.Bytes())
	}
	out.WriteString("\n" + indent + "}")
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
