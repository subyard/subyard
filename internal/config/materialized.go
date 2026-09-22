package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
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
func MaterializedAssets(values map[string]string, developer string) ([]MaterializedAsset, error) {
	if !domain.SafeName(developer) {
		return nil, errors.New("invalid developer user")
	}
	var assets []MaterializedAsset
	for _, agent := range strings.Fields(values["CODING_TOOL_INTEGRATIONS"]) {
		if !domain.SafeName(agent) {
			return nil, fmt.Errorf("invalid agent name %q", agent)
		}
		for _, kind := range []string{"CONFIG", "RULES"} {
			setting := "AGENT_" + agent + "_" + kind
			source, destination := values[setting], values[setting+"_DEST"]
			if source == "" || destination == "" {
				continue
			}
			clean := filepath.Clean(destination)
			if filepath.IsAbs(destination) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("agent %s %s destination leaves the developer home", agent, strings.ToLower(kind))
			}
			ownedFormat := ""
			if kind == "CONFIG" {
				switch filepath.Ext(clean) {
				case ".json":
					ownedFormat = "json"
				case ".toml":
					ownedFormat = "toml"
				}
			}
			assets = append(assets, MaterializedAsset{
				Name: agent + "." + strings.ToLower(kind), Setting: setting, Source: source,
				Destination: filepath.Join("/home", developer, clean),
				OwnedFormat: ownedFormat,
			})
		}
	}
	return assets, nil
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
