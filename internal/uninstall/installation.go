package uninstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
)

const ManifestName = "installation.json"

// ErrInstallationChanged distinguishes disappearance during the final identity
// check from an absent optional inventory, while retaining the read cause.
var ErrInstallationChanged = errors.New("installation ownership inventory changed while reading")

func InstallationAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrInstallationChanged)
}

// InstallationIdentity constructs the shared schema-1 identity without granting
// ownership of entrypoints, cache artifacts or native yard resources.
func InstallationIdentity(uid uint32, data, configuration, runtime, cache string, includeBootstrap bool) (Installation, error) {
	value := Installation{Schema: 1, OperatorUID: uid, DataHome: data, ConfigHome: configuration, RuntimeRoot: runtime, CacheRoot: cache, ShellFiles: []string{}}
	if err := value.Validate(data, uid); err != nil {
		return value, err
	}
	dataInfo, dataErr := os.Stat(data)
	runtimeInfo, runtimeErr := os.Stat(runtime)
	if dataErr == nil && runtimeErr == nil && os.SameFile(dataInfo, runtimeInfo) {
		return value, errors.New("runtime and bootstrap inventory require separate native directories")
	}
	// Download caches can differ per invocation. Existing protected ownership
	// records, rather than a new download location, retain the installation root.
	recordedCache := ""
	directories := []string{runtime}
	if includeBootstrap {
		directories = append(directories, data)
	}
	for _, directory := range directories {
		recorded, err := ReadInstallationFile(filepath.Join(directory, ManifestName), data, uid)
		if InstallationAbsent(err) {
			continue
		}
		if err != nil {
			return value, err
		}
		if err := recorded.CheckRoots(configuration, runtime, recordedCache); err != nil {
			return value, config.OwnerMetadataContext(filepath.Join(directory, ManifestName), "installation inventory", err)
		}
		recordedCache = recorded.CacheRoot
	}
	if recordedCache != "" {
		value.CacheRoot = recordedCache
	}
	return value, value.Validate(data, uid)
}

func (installation Installation) SameIdentity(other Installation) bool {
	return installation.Schema == other.Schema && installation.OperatorUID == other.OperatorUID && installation.DataHome == other.DataHome && installation.CacheRoot == other.CacheRoot && installation.CheckRoots(other.ConfigHome, other.RuntimeRoot, other.CacheRoot) == nil
}

// InstallationInspection retains bounded metadata and the opened publication
// directory. Inspection never creates a file or acquires a mutation lock.
type InstallationInspection struct {
	State        string
	Installation Installation
	Snapshot     config.PersistentFileSnapshot
	Directory    Tree
	Epoch        uint64
	root         *os.File
}

func (inspection *InstallationInspection) Close() error {
	if inspection.root == nil {
		return nil
	}
	err := inspection.root.Close()
	inspection.root = nil
	return err
}

func InspectInstallation(directory string, identity Installation) (*InstallationInspection, error) {
	if err := identity.Validate(identity.DataHome, identity.OperatorUID); err != nil {
		return nil, err
	}
	parent, err := openParent(directory)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	root, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	inspection := &InstallationInspection{State: "unreadable", root: root}
	fail := func(err error) (*InstallationInspection, error) { inspection.Close(); return inspection, err }
	inspection.Directory, err = CaptureRoot(directory)
	if err != nil {
		return fail(err)
	}
	opened, err := root.Stat()
	current, pathErr := os.Lstat(directory)
	if err != nil || pathErr != nil || !os.SameFile(opened, current) {
		return fail(config.OwnerMetadataError(directory, "installation inventory directory", "installation directory changed during inspection", "", errors.Join(ErrInstallationChanged, err, pathErr)))
	}
	inspection.Epoch, err = ReadOwnerEpoch(identity.DataHome)
	if err != nil {
		return fail(err)
	}
	inspection.Snapshot, err = config.ReadPersistentSnapshotAt(root, ManifestName)
	if err != nil {
		if diagnostic, ok := config.SourceDiagnostic(directory, err); ok {
			return fail(config.OwnerMetadataError(filepath.Join(directory, ManifestName), "installation inventory", "cannot inspect protected installation metadata", diagnostic, err))
		}
		return fail(err)
	}
	if !inspection.Snapshot.Exists {
		inspection.State, inspection.Installation = "missing", identity
		return inspection, nil
	}
	if inspection.Snapshot.Identity.Mode&07777 != 0600 || len(inspection.Snapshot.Content) > 65536 {
		return fail(config.OwnerMetadataError(filepath.Join(directory, ManifestName), "installation inventory", "must be a bounded mode 0600 file", fmt.Sprintf("observed mode=%04o, bytes=%d; required mode=0600, maximum bytes=65536", inspection.Snapshot.Identity.Mode&07777, len(inspection.Snapshot.Content)), nil))
	}
	inspection.Installation, err = decodeInstallation(bytes.NewReader(inspection.Snapshot.Content), identity.DataHome, identity.OperatorUID)
	if err != nil {
		return fail(config.OwnerMetadataContext(filepath.Join(directory, ManifestName), "installation inventory", err))
	}
	if err := inspection.Installation.CheckRoots(identity.ConfigHome, identity.RuntimeRoot, identity.CacheRoot); err != nil {
		inspection.State = "conflicting"
		return fail(config.OwnerMetadataContext(filepath.Join(directory, ManifestName), "installation inventory", err))
	}
	if directory == identity.RuntimeRoot {
		if err := inspection.Installation.CheckRuntimeScope(); err != nil {
			inspection.State = "conflicting"
			return fail(config.OwnerMetadataContext(filepath.Join(directory, ManifestName), "runtime installation inventory", err))
		}
	}
	inspection.State = "matching"
	return inspection, nil
}

// Publish applies only an assessed identity under the caller's owner barrier.
// Exact absence uses no-replace; matching content does not write or fsync.
func (inspection *InstallationInspection) Publish(value Installation) error {
	if inspection.root == nil || !inspection.Installation.SameIdentity(value) {
		return errors.New("installation publication differs from inspected identity")
	}
	barrier, err := BorrowOwnerLock(value.DataHome)
	if err != nil {
		return err
	}
	if barrier == nil {
		return errors.New("installation publication requires its existing owner barrier")
	}
	defer barrier.Close()
	if err := syscall.Flock(int(inspection.root.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(inspection.root.Fd()), syscall.LOCK_UN)
	if err := Admit(value.DataHome, []Tree{inspection.Directory}, inspection.Epoch, false, false); err != nil {
		return err
	}
	if inspection.Snapshot.Exists && value.BinDir == inspection.Installation.BinDir && slices.Equal(value.ShellFiles, inspection.Installation.ShellFiles) && slices.Equal(value.CacheFiles, inspection.Installation.CacheFiles) {
		current, err := config.ReadPersistentSnapshotAt(inspection.root, ManifestName)
		if err != nil {
			return err
		}
		if config.BindPersistentSnapshot(current) != config.BindPersistentSnapshot(inspection.Snapshot) {
			return config.ErrPersistentTargetStale
		}
		return nil
	}
	body, err := MarshalInstallation(value)
	if err != nil {
		return err
	}
	return config.CompareAndSwapLocalSettingsAt(inspection.root, ManifestName, config.BindPersistentSnapshot(inspection.Snapshot), body, nil, nil, nil)
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
	if !ok || !info.Mode().IsRegular() || stat.Mode&07777 != 0600 || stat.Uid != uid || stat.Nlink != 1 {
		details := config.FileMetadata(info)
		if ok {
			details += fmt.Sprintf("; observed native mode=%04o, links=%d", stat.Mode&07777, stat.Nlink)
		}
		return failure(fmt.Sprintf("must be an operator-owned regular mode 0600 file with one link; %s; required type=regular, mode=0600, uid=%d, links=1", details, uid), nil)
	}
	if info.Size() > 65536 {
		return failure(fmt.Sprintf("installation ownership inventory exceeds its size bound; observed bytes=%d, required maximum=65536", info.Size()), nil)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("installation ownership inventory changed while opening: "+config.MetadataAccessReason(err), errors.Join(ErrInstallationChanged, err))
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return failure("installation ownership inventory changed while opening: "+config.MetadataAccessReason(err), errors.Join(ErrInstallationChanged, err))
	}
	if !os.SameFile(info, opened) {
		return failure("installation ownership inventory changed while opening (inode)", ErrInstallationChanged)
	}
	installation, err = decodeInstallation(file, dataHome, uid)
	if err != nil {
		return installation, config.OwnerMetadataContext(path, "installation inventory", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return failure("installation ownership inventory changed while reading: "+config.MetadataAccessReason(err), errors.Join(ErrInstallationChanged, err))
	}
	if !os.SameFile(info, current) || current.ModTime() != info.ModTime() || current.Size() != info.Size() {
		return installation, config.OwnerMetadataError(path, "installation inventory", "installation ownership inventory changed while reading (inode, mtime or size)", fmt.Sprintf("stored size=%d, mtime=%d; current size=%d, mtime=%d", info.Size(), info.ModTime().UnixNano(), current.Size(), current.ModTime().UnixNano()), ErrInstallationChanged)
	}
	fresh, ok := current.Sys().(*syscall.Stat_t)
	if !ok || fresh.Mode != stat.Mode || fresh.Uid != stat.Uid || fresh.Gid != stat.Gid || fresh.Nlink != stat.Nlink || fresh.Ctim != stat.Ctim {
		return installation, config.OwnerMetadataError(path, "installation inventory", "installation ownership inventory security metadata changed while reading", "stored "+config.FileMetadata(info)+"; current "+config.FileMetadata(current), ErrInstallationChanged)
	}
	return installation, nil
}

func decodeInstallation(reader io.Reader, dataHome string, uid uint32) (Installation, error) {
	var installation Installation
	failure := func(reason string, cause error) (Installation, error) {
		return installation, config.OwnerMetadataError("", "installation inventory", reason, "", cause)
	}
	decoder := json.NewDecoder(io.LimitReader(reader, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installation); err != nil {
		return failure("invalid installation ownership inventory (JSON format)", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return failure("installation ownership inventory has trailing data", err)
	}
	if dataHome == "" {
		dataHome = installation.DataHome
	}
	if err := installation.Validate(dataHome, uid); err != nil {
		return installation, err
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
	if installation.DataHome == installation.RuntimeRoot {
		return config.OwnerMetadataError("", "installation inventory", "runtime and bootstrap inventories require separate roots", "dataHome and runtimeRoot identify the same path", nil)
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
	if buffer.Len() > 65536 {
		return nil, errors.New("installation ownership inventory exceeds its size bound")
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

// CheckRuntimeScope prevents a runtime-only record from acquiring bootstrap
// entrypoint claims. Cache artifact claims retain their existing scope.
func (installation Installation) CheckRuntimeScope() error {
	if installation.BinDir != "" || len(installation.ShellFiles) != 0 {
		return config.OwnerMetadataError("", "runtime installation inventory", "runtime inventory cannot claim bootstrap entrypoints", fmt.Sprintf("stored binDir=%s, shellFiles count=%d; required empty binDir and shellFiles", config.QuoteMetadataPath(installation.BinDir), len(installation.ShellFiles)), nil)
	}
	return nil
}
