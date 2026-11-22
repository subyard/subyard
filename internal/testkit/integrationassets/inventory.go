// Package integrationassets provides one source inventory for repository checks
// and published rollback snapshots. It never reads operator settings.
package integrationassets

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
)

func TrackedFiles(root string) ([]string, error) {
	output, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00"), nil
}

// DigestFiles sorts full relative-path records, independently of directory walk
// order. No directory-name exclusion exists: a referenced tests/ asset is shipped.
func DigestFiles(root string, files []string) (string, error) {
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)
	hash := sha256.New()
	for _, path := range files {
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("integration source is not regular: %s", path)
		}
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return "", err
		}
		mode := 0
		if info.Mode().Perm()&0o100 != 0 {
			mode = 0o111
		}
		fmt.Fprintf(hash, "%s %03o %x\n", filepath.ToSlash(path), mode, sha256.Sum256(data))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type Inventory struct {
	Digest   string                         `json:"digest"`
	Sources  []string                       `json:"sources"`
	Profiles map[string]profile.RuntimeHook `json:"profiles"`
}

// Inspect uses a tracked source mirror so untracked metadata cannot influence
// discovery. The same operation handles a published tag and a dirty checkout.
func Inspect(root string, tracked []string) (Inventory, error) {
	mirror, err := os.MkdirTemp("/tmp", "subyard-integration-inventory-")
	if err != nil {
		return Inventory{}, err
	}
	defer os.RemoveAll(mirror)
	for _, path := range tracked {
		// Git's index can contain links; source loaders apply their normal guards.
		info, err := os.Lstat(filepath.Join(root, path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Inventory{}, err
		}
		destination := filepath.Join(mirror, path)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return Inventory{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(filepath.Join(root, path))
			if err != nil {
				return Inventory{}, err
			}
			if err := os.Symlink(link, destination); err != nil {
				return Inventory{}, err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return Inventory{}, fmt.Errorf("unsupported tracked input %s", path)
		}
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return Inventory{}, err
		}
		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o100 != 0 {
			mode = 0o755
		}
		if err := os.WriteFile(destination, data, mode); err != nil {
			return Inventory{}, err
		}
		if err := os.Chmod(destination, mode); err != nil {
			return Inventory{}, err
		}
	}
	profiles, err := profile.Load(mirror)
	if err != nil {
		return Inventory{}, err
	}
	formats := map[string]profile.RuntimeHook{}
	for _, definition := range profiles {
		if definition.Runtime != nil {
			formats[definition.Runtime.ActivationID] = *definition.Runtime
		}
	}
	definitions, err := profile.LoadIntegrations(mirror)
	if err != nil {
		return Inventory{}, err
	}
	var ids []string
	for _, definition := range definitions {
		ids = append(ids, definition.Name)
	}
	home := filepath.Join(mirror, "operator")
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: mirror, OperatorHome: home, Environment: map[string]string{
		"HOME": home, "SUBYARD_CONFIG_HOME": filepath.Join(home, "config"), "SUBYARD_HOME": filepath.Join(home, "data"),
		"ENVIRONMENT_PROFILES": "", "CODING_TOOL_INTEGRATIONS": strings.Join(ids, " "), "DEV_USER": "dev", "DEV_UID": "1000", "DEV_GID": "1000",
	}})
	if err != nil {
		return Inventory{}, err
	}
	var environment []string
	for key, value := range loaded.Environment {
		environment = append(environment, key+"="+value)
	}
	runtime := reconcileruntime.Runtime{RepositoryRoot: mirror, Integrations: definitions, Environment: environment}
	sources, err := runtime.IntegrationAssetSources()
	if err != nil {
		return Inventory{}, err
	}
	sources = append(sources, filepath.Join(mirror, "config", "agents.env"))
	var files []string
	for _, source := range sources {
		relative, err := filepath.Rel(mirror, source)
		if err != nil {
			return Inventory{}, err
		}
		if slices.Contains(tracked, relative) {
			files = append(files, relative)
		}
	}
	slices.Sort(files)
	files = slices.Compact(files)
	digest, err := DigestFiles(root, files)
	return Inventory{Digest: digest, Sources: files, Profiles: formats}, err
}
