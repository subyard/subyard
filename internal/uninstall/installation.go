package uninstall

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
)

const ManifestName = "installation.json"

// Installation records entrypoints outside the normal config/data roots.
// Values are paths and native operator identity, never settings or credentials.
type Installation struct {
	Schema      int      `json:"schema"`
	OperatorUID uint32   `json:"operatorUid"`
	DataHome    string   `json:"dataHome"`
	ConfigHome  string   `json:"configHome"`
	RuntimeRoot string   `json:"runtimeRoot"`
	CacheRoot   string   `json:"cacheRoot"`
	BinDir      string   `json:"binDir"`
	ShellFiles  []string `json:"shellFiles"`
	CacheFiles  []Entry  `json:"cacheFiles,omitempty"`
}

func ReadInstallation(dataHome string, uid uint32) (Installation, error) {
	return ReadInstallationFile(filepath.Join(dataHome, ManifestName), dataHome, uid)
}

func ReadInstallationFile(path, dataHome string, uid uint32) (Installation, error) {
	var installation Installation
	info, err := os.Lstat(path)
	if err != nil {
		return installation, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uid {
		return installation, errors.New("installation inventory must be an operator-owned regular mode 0600 file")
	}
	file, err := os.Open(path)
	if err != nil {
		return installation, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installation); err != nil {
		return installation, errors.New("invalid installation ownership inventory")
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return installation, errors.New("installation ownership inventory has trailing data")
	}
	if dataHome == "" {
		dataHome = installation.DataHome
	}
	if err := installation.Validate(dataHome, uid); err != nil {
		return installation, err
	}
	// Reject a changed manifest while it is read; no protected content is logged.
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) || current.ModTime() != info.ModTime() || current.Size() != info.Size() {
		return installation, errors.New("installation ownership inventory changed while reading")
	}
	return installation, nil
}

func (installation Installation) Validate(dataHome string, uid uint32) error {
	if installation.Schema != 1 || installation.OperatorUID != uid || installation.DataHome != dataHome {
		return errors.New("installation inventory identity does not match this operator and data root")
	}
	paths := []string{installation.DataHome, installation.ConfigHome, installation.RuntimeRoot, installation.CacheRoot}
	if installation.BinDir != "" {
		paths = append(paths, filepath.Join(installation.BinDir, "yard"), filepath.Join(installation.BinDir, "sy"))
	}
	for _, path := range paths {
		if err := safePath(path); err != nil {
			return fmt.Errorf("invalid installation artifact path: %w", err)
		}
	}
	if len(installation.ShellFiles) > 16 {
		return errors.New("installation shell inventory is oversized")
	}
	if len(installation.CacheFiles) > 4096 {
		return errors.New("installation cache ownership inventory is oversized")
	}
	cachePaths := map[string]bool{}
	for _, entry := range installation.CacheFiles {
		if entry.Path == "." || entry.Path == ".." || filepath.IsAbs(entry.Path) || filepath.Clean(entry.Path) != entry.Path || strings.HasPrefix(entry.Path, "../") || cachePaths[entry.Path] || entry.Mode&syscall.S_IFMT != syscall.S_IFREG || entry.UID != uid {
			return errors.New("invalid installation cache artifact ownership")
		}
		cachePaths[entry.Path] = true
		if err := safePath(filepath.Join(installation.CacheRoot, entry.Path)); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, path := range installation.ShellFiles {
		if err := safePath(path); err != nil {
			return err
		}
		if seen[path] {
			return errors.New("duplicate installation shell file")
		}
		seen[path] = true
	}
	return nil
}

// MarshalInstallation is shared with internal installer adapters.
func MarshalInstallation(installation Installation) ([]byte, error) {
	if err := installation.Validate(installation.DataHome, installation.OperatorUID); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	if err := encoder.Encode(installation); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
