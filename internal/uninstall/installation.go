package uninstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const ManifestName = "installation.json"

// ErrInstallationChanged distinguishes disappearance during the final identity
// check from an absent optional inventory, while retaining the read cause.
var ErrInstallationChanged = errors.New("installation ownership inventory changed while reading")

func InstallationAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrInstallationChanged)
}

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
	failure := func(reason string, cause error) (Installation, error) {
		return installation, config.OwnerMetadataError(path, "installation inventory", reason, "", cause)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return failure(config.MetadataAccessReason(err), err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uid {
		return failure(fmt.Sprintf("must be an operator-owned regular mode 0600 file; %s; required type=regular, mode=0600, uid=%d", config.FileMetadata(info), uid), nil)
	}
	file, err := os.Open(path)
	if err != nil {
		return failure(config.MetadataAccessReason(err), err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installation); err != nil {
		return failure("invalid installation ownership inventory (JSON format)", err)
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return failure("installation ownership inventory has trailing data", nil)
	}
	if dataHome == "" {
		dataHome = installation.DataHome
	}
	if err := installation.Validate(dataHome, uid); err != nil {
		return installation, config.OwnerMetadataContext(path, "installation inventory", err)
	}
	// Reject a changed manifest while it is read; no protected content is logged.
	current, err := os.Lstat(path)
	if err != nil {
		return failure("installation ownership inventory changed while reading: "+config.MetadataAccessReason(err), errors.Join(ErrInstallationChanged, err))
	}
	if !os.SameFile(info, current) || current.ModTime() != info.ModTime() || current.Size() != info.Size() {
		return installation, config.OwnerMetadataError(path, "installation inventory", "installation ownership inventory changed while reading (inode, mtime or size)", fmt.Sprintf("stored size=%d, mtime=%d; current size=%d, mtime=%d", info.Size(), info.ModTime().UnixNano(), current.Size(), current.ModTime().UnixNano()), ErrInstallationChanged)
	}
	return installation, nil
}

func (installation Installation) Validate(dataHome string, uid uint32) error {
	var differences []string
	if installation.Schema != 1 {
		differences = append(differences, fmt.Sprintf("schema: stored=%d, required=1", installation.Schema))
	}
	if installation.OperatorUID != uid {
		differences = append(differences, fmt.Sprintf("operatorUid: stored=%d, current=%d", installation.OperatorUID, uid))
	}
	if installation.DataHome != dataHome {
		differences = append(differences, pathDifference("dataHome", installation.DataHome, dataHome))
	}
	if len(differences) != 0 {
		return config.OwnerMetadataError("", "installation inventory", "identity does not match this operator and data root", strings.Join(differences, "; "), nil)
	}
	paths := []struct{ name, path string }{{"dataHome", installation.DataHome}, {"configHome", installation.ConfigHome}, {"runtimeRoot", installation.RuntimeRoot}, {"cacheRoot", installation.CacheRoot}}
	if installation.BinDir != "" {
		paths = append(paths, struct{ name, path string }{"binDir/yard", filepath.Join(installation.BinDir, "yard")}, struct{ name, path string }{"binDir/sy", filepath.Join(installation.BinDir, "sy")})
	}
	for _, field := range paths {
		if err := safePath(field.path); err != nil {
			return config.OwnerMetadataContext(field.path, "installation inventory "+field.name, err)
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
			return config.OwnerMetadataError(entry.Path, "installation inventory cache artifact", "invalid installation cache artifact ownership", fmt.Sprintf("path=%s, observed type=%#o, uid=%d; required unique clean relative path, type=%#o, uid=%d", config.QuoteMetadataPath(entry.Path), entry.Mode&syscall.S_IFMT, entry.UID, syscall.S_IFREG, uid), nil)
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

// CheckRoots compares already-read installation metadata with its invoking
// owner context. An empty cache root means that caller does not bind caches.
func (installation Installation) CheckRoots(configHome, runtimeRoot, cacheRoot string) error {
	var differences []string
	if installation.ConfigHome != configHome {
		differences = append(differences, pathDifference("configHome", installation.ConfigHome, configHome))
	}
	if installation.RuntimeRoot != runtimeRoot {
		differences = append(differences, pathDifference("runtimeRoot", installation.RuntimeRoot, runtimeRoot))
	}
	if cacheRoot != "" && installation.CacheRoot != cacheRoot {
		differences = append(differences, pathDifference("cacheRoot", installation.CacheRoot, cacheRoot))
	}
	if len(differences) == 0 {
		return nil
	}
	return config.OwnerMetadataError("", "installation inventory", "roots conflict with the installed owner context", strings.Join(differences, "; "), nil)
}

func pathDifference(field, stored, current string) string {
	return field + ": stored=" + config.QuoteMetadataPath(stored) + ", current=" + config.QuoteMetadataPath(current)
}
