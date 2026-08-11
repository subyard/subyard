package configsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const (
	maxConfigBytes = 1 << 20
	maxAssetBytes  = 8 << 20
)

var forbiddenSourceComponents = map[string]struct{}{
	"projects": {}, "secrets": {}, "generated": {}, "keys": {}, "tools": {},
	"ssh": {}, "logs": {}, "storage": {}, "exports": {}, "desired_power": {},
}

type sourceSnapshot struct {
	root       string
	id         string
	commit     string
	digest     string
	hostID     string
	manifest   SourceManifest
	files      map[string]candidateFile
	yardNames  []string
	scalarPath map[string]string
}

func readSource(options Options, hostID string) (sourceSnapshot, error) {
	root, err := filepath.Abs(options.SourceRoot)
	if err != nil {
		return sourceSnapshot{}, err
	}
	root = filepath.Clean(root)
	if err := validateSourceDirectory(root); err != nil {
		return sourceSnapshot{}, err
	}
	identityRoot := root
	if options.SourceIdentityRoot != "" {
		identityRoot, err = filepath.Abs(options.SourceIdentityRoot)
		if err != nil {
			return sourceSnapshot{}, err
		}
		identityRoot = filepath.Clean(identityRoot)
		if identityRoot == string(filepath.Separator) {
			return sourceSnapshot{}, errors.New(
				"versioned configuration source identity cannot be the filesystem root",
			)
		}
	}
	commit, err := validateGitSource(root, hostID)
	if err != nil {
		return sourceSnapshot{}, err
	}
	manifestPath := filepath.Join(root, "subyard-config.json")
	manifestContent, err := readSourceFile(root, manifestPath, maxConfigBytes, false)
	if err != nil {
		return sourceSnapshot{}, fmt.Errorf("source manifest: %w", err)
	}
	var manifest SourceManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestContent))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return sourceSnapshot{}, fmt.Errorf("source manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return sourceSnapshot{}, fmt.Errorf("source manifest: %w", err)
	}
	if manifest.SchemaVersion != sourceSchema {
		return sourceSnapshot{}, fmt.Errorf(
			"unsupported source schema %d; expected %d", manifest.SchemaVersion, sourceSchema,
		)
	}
	if len(manifest.Policy) != 0 {
		return sourceSnapshot{}, errors.New("source schema 1 defines no policy keys")
	}
	allowedFiles := make(map[string]config.FileSettingMapping, len(options.FileSettings))
	for _, mapping := range options.FileSettings {
		allowedFiles[filepath.ToSlash(filepath.Clean(mapping.Relative))] = mapping
	}
	snapshot := sourceSnapshot{
		root: root, id: digestBytes([]byte(identityRoot)), commit: commit, hostID: hostID,
		manifest: manifest, files: map[string]candidateFile{},
		scalarPath: map[string]string{},
	}
	if err := validateTopLevelRoles(root); err != nil {
		return sourceSnapshot{}, err
	}
	if err := snapshot.readScope(
		filepath.Join(root, "shared"), "overrides/shared/config.env",
		"overrides/shared/agents", config.ScopeShared, allowedFiles, false,
	); err != nil {
		return sourceSnapshot{}, fmt.Errorf("shared source: %w", err)
	}
	hostsRoot := filepath.Join(root, "hosts")
	hostEntries, err := readOptionalDirectory(root, hostsRoot)
	if err != nil {
		return sourceSnapshot{}, fmt.Errorf("owner hosts: %w", err)
	}
	hostRoot := filepath.Join(root, "hosts", hostID)
	hostPresent := false
	for _, entry := range hostEntries {
		if entry.Name() == hostID {
			hostPresent = true
			break
		}
	}
	if hostPresent {
		if _, err := readOptionalDirectory(root, hostRoot); err != nil {
			return sourceSnapshot{}, fmt.Errorf("owner host %q: %w", hostID, err)
		}
		if err := validateChildren(hostRoot, map[string]bool{
			"config.env": false, "overrides": true, "yards": true,
		}); err != nil {
			return sourceSnapshot{}, fmt.Errorf("owner host %q: %w", hostID, err)
		}
		if err := snapshot.readConfig(
			filepath.Join(hostRoot, "config.env"), "config.env", config.ScopeHost, false,
		); err != nil {
			return sourceSnapshot{}, fmt.Errorf("owner host %q: %w", hostID, err)
		}
		if err := snapshot.readOverrides(
			filepath.Join(hostRoot, "overrides"), "overrides/host/agents", allowedFiles,
		); err != nil {
			return sourceSnapshot{}, fmt.Errorf("owner host %q overrides: %w", hostID, err)
		}
	}
	yardsRoot := filepath.Join(hostRoot, "yards")
	var entries []os.DirEntry
	if hostPresent {
		entries, err = readOptionalDirectory(root, yardsRoot)
		if err != nil {
			return sourceSnapshot{}, fmt.Errorf("owner host %q yards: %w", hostID, err)
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !domain.SafeName(name) ||
			name == "default" {
			return sourceSnapshot{}, fmt.Errorf("invalid versioned yard entry %q for host %s", name, hostID)
		}
		yardRoot := filepath.Join(yardsRoot, name)
		if err := validateChildren(yardRoot, map[string]bool{
			"config.env": false, "overrides": true,
		}); err != nil {
			return sourceSnapshot{}, fmt.Errorf("yard %s: %w", name, err)
		}
		sourceConfig := filepath.Join(yardRoot, "config.env")
		if _, err := os.Lstat(sourceConfig); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return sourceSnapshot{}, fmt.Errorf("yard %s has no config.env definition", name)
			}
			return sourceSnapshot{}, err
		}
		targetConfig := filepath.ToSlash(filepath.Join("yards", name, "config.env"))
		if err := snapshot.readConfig(sourceConfig, targetConfig, config.ScopeYard, true); err != nil {
			return sourceSnapshot{}, fmt.Errorf("yard %s: %w", name, err)
		}
		targetAssets := filepath.ToSlash(filepath.Join("yards", name, "overrides", "agents"))
		if err := snapshot.readOverrides(
			filepath.Join(yardRoot, "overrides"), targetAssets, allowedFiles,
		); err != nil {
			return sourceSnapshot{}, fmt.Errorf("yard %s overrides: %w", name, err)
		}
		snapshot.yardNames = append(snapshot.yardNames, name)
	}
	sort.Strings(snapshot.yardNames)
	if err := snapshot.ensureTracked(); err != nil {
		return sourceSnapshot{}, err
	}
	snapshot.digest = snapshotDigest(snapshot.files, manifestContent)
	return snapshot, nil
}

func (snapshot *sourceSnapshot) readScope(
	sourceRoot, targetConfig, targetAssets string,
	scope config.SettingScope,
	allowed map[string]config.FileSettingMapping,
	required bool,
) error {
	entries, err := readOptionalDirectory(snapshot.root, sourceRoot)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if required {
			return errors.New("scope directory is required")
		}
		return nil
	}
	if err := validateChildren(sourceRoot, map[string]bool{
		"config.env": false, "overrides": true,
	}); err != nil {
		return err
	}
	if err := snapshot.readConfig(
		filepath.Join(sourceRoot, "config.env"), targetConfig, scope, false,
	); err != nil {
		return err
	}
	return snapshot.readOverrides(
		filepath.Join(sourceRoot, "overrides"), targetAssets, allowed,
	)
}

func (snapshot *sourceSnapshot) readConfig(
	source, target string,
	scope config.SettingScope,
	required bool,
) error {
	content, err := readOptionalSourceFile(snapshot.root, source, maxConfigBytes, false)
	if err != nil {
		return err
	}
	if content == nil {
		if required {
			return fmt.Errorf("%s is required", filepath.Base(source))
		}
		return nil
	}
	applications, err := settingApplications(source)
	if err != nil {
		return err
	}
	names, err := config.AssignedSettingNames(source)
	if err != nil {
		return err
	}
	for _, name := range names {
		definition, err := config.ValidateSettingName(scope, name, true)
		if err != nil {
			return err
		}
		if definition.Kind != config.SettingScalar {
			return fmt.Errorf(
				"file setting %s must be represented under overrides/agents, not config.env", name,
			)
		}
	}
	target = filepath.ToSlash(target)
	snapshot.files[target] = candidateFile{
		SourcePath: source, Content: content, Digest: digestBytes(content), Mode: 0o600,
		Applications: applications,
	}
	snapshot.scalarPath[target] = source
	return nil
}

func (snapshot *sourceSnapshot) readOverrides(
	sourceRoot, targetRoot string,
	allowed map[string]config.FileSettingMapping,
) error {
	entries, err := readOptionalDirectory(snapshot.root, sourceRoot)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if err := validateChildren(sourceRoot, map[string]bool{"agents": true}); err != nil {
		return err
	}
	agentsRoot := filepath.Join(sourceRoot, "agents")
	if _, err := os.Lstat(agentsRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(agentsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == agentsRoot {
			return nil
		}
		relative, err := filepath.Rel(agentsRoot, path)
		if err != nil || !safeRelative(relative) {
			return fmt.Errorf("unsafe override path %q", path)
		}
		if forbiddenPath(relative) {
			return fmt.Errorf("forbidden source role in %q", filepath.ToSlash(relative))
		}
		if entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("override directory is a symlink: %s", path)
			}
			return nil
		}
		mapping, ok := allowed[filepath.ToSlash(relative)]
		if !ok {
			return fmt.Errorf("unknown file setting %q", filepath.ToSlash(relative))
		}
		content, err := readSourceFile(snapshot.root, path, maxAssetBytes, false)
		if err != nil {
			return err
		}
		if err := config.ValidateNonSecretContent(mapping.Name, string(content)); err != nil {
			return fmt.Errorf("%s: %w", filepath.ToSlash(relative), err)
		}
		target := filepath.ToSlash(filepath.Join(targetRoot, relative))
		snapshot.files[target] = candidateFile{
			SourcePath: path, Content: content, Digest: digestBytes(content), Mode: 0o644,
			Applications: []config.SettingApplication{mapping.Application},
		}
		return nil
	})
}

func validateSourceDirectory(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("versioned configuration source must be a real directory")
	}
	return validateOwnedMode(root, info, true)
}

func validateGitSource(root, hostID string) (string, error) {
	top, err := gitOutput(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New("versioned configuration source must be a Git worktree")
	}
	if filepath.Clean(strings.TrimSpace(top)) != root {
		return "", errors.New("source path must be the root of its Git worktree")
	}
	commit, err := gitOutput(root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve source commit: %w", err)
	}
	paths := []string{"--", "subyard-config.json", "shared", filepath.ToSlash(filepath.Join("hosts", hostID))}
	statusArgs := append([]string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, paths...)
	status, err := gitOutputBytes(root, statusArgs...)
	if err != nil {
		return "", fmt.Errorf("inspect source worktree: %w", err)
	}
	if len(status) != 0 {
		return "", errors.New("versioned configuration source has tracked or untracked changes in managed paths")
	}
	ignoredArgs := append([]string{
		"ls-files", "-z", "--others", "--ignored", "--exclude-standard",
	}, paths...)
	ignored, err := gitOutputBytes(root, ignoredArgs...)
	if err != nil {
		return "", fmt.Errorf("inspect ignored source files: %w", err)
	}
	if len(ignored) != 0 {
		return "", errors.New("managed source paths contain ignored untracked files")
	}
	return strings.TrimSpace(commit), nil
}

func (snapshot sourceSnapshot) ensureTracked() error {
	paths := []string{"subyard-config.json"}
	for _, file := range snapshot.files {
		relative, err := filepath.Rel(snapshot.root, file.SourcePath)
		if err != nil || !safeRelative(relative) {
			return errors.New("source file escaped its worktree")
		}
		paths = append(paths, filepath.ToSlash(relative))
	}
	sort.Strings(paths)
	for _, path := range paths {
		if _, err := gitOutput(snapshot.root, "ls-files", "--error-unmatch", "--", path); err != nil {
			return fmt.Errorf("managed source file is not tracked by Git: %s", path)
		}
	}
	return nil
}

func validateTopLevelRoles(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if _, forbidden := forbiddenSourceComponents[name]; forbidden {
			return fmt.Errorf("forbidden top-level source role %q", entry.Name())
		}
	}
	return nil
}

func validateChildren(root string, allowed map[string]bool) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a real source directory", root)
	}
	if err := validateOwnedMode(root, info, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		directory, ok := allowed[entry.Name()]
		if !ok {
			return fmt.Errorf("unexpected source path %q", filepath.Join(root, entry.Name()))
		}
		if directory != entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			kind := "regular file"
			if directory {
				kind = "directory"
			}
			return fmt.Errorf("%s must be a real %s", filepath.Join(root, entry.Name()), kind)
		}
	}
	return nil
}

func readOptionalDirectory(sourceRoot, path string) ([]os.DirEntry, error) {
	if err := validateSourceAncestors(sourceRoot, path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !pathWithin(path, sourceRoot) {
		return nil, fmt.Errorf("%s must be a real source directory", path)
	}
	if err := validateOwnedMode(path, info, true); err != nil {
		return nil, err
	}
	return os.ReadDir(path)
}

func readOptionalSourceFile(root, path string, maximum int64, executable bool) ([]byte, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return readSourceFile(root, path, maximum, executable)
}

func readSourceFile(root, path string, maximum int64, executable bool) ([]byte, error) {
	if !pathWithin(path, root) {
		return nil, errors.New("source path escaped its worktree")
	}
	if err := validateSourceAncestors(root, path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular non-symlink file", path)
	}
	if executable != (info.Mode().Perm()&0o111 != 0) {
		if executable {
			return nil, fmt.Errorf("%s must be executable", path)
		}
		if info.Mode().Perm()&0o111 != 0 {
			return nil, fmt.Errorf("%s must not be executable", path)
		}
	}
	if info.Size() > maximum {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", path, maximum)
	}
	if err := validateOwnedMode(path, info, false); err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return nil, fmt.Errorf("%s must not be hard-linked", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maximum {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", path, maximum)
	}
	return content, nil
}

func validateSourceAncestors(root, path string) error {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("source path escaped its worktree")
	}
	if relative == "." {
		return nil
	}
	current := filepath.Clean(root)
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("source path has a non-directory or symlink ancestor: %s", current)
		}
		if err := validateOwnedMode(current, info, true); err != nil {
			return err
		}
	}
	return nil
}

func validateOwnedMode(path string, info os.FileInfo, directory bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("source path is not operator-owned: %s", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		kind := "file"
		if directory {
			kind = "directory"
		}
		return fmt.Errorf("source %s is group/world writable: %s", kind, path)
	}
	return nil
}

func settingApplications(path string) ([]config.SettingApplication, error) {
	names, err := config.AssignedSettingNames(path)
	if err != nil {
		return nil, err
	}
	seen := map[config.SettingApplication]struct{}{}
	var result []config.SettingApplication
	for _, name := range names {
		definition, ok := config.LookupSetting(name)
		if !ok {
			continue
		}
		if _, exists := seen[definition.Application]; exists {
			continue
		}
		seen[definition.Application] = struct{}{}
		result = append(result, definition.Application)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result, nil
}

func snapshotDigest(files map[string]candidateFile, manifest []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("manifest\x00"))
	_, _ = hash.Write(manifest)
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		_, _ = hash.Write([]byte("\x00" + path + "\x00" + files[path].Digest))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func gitOutput(root string, arguments ...string) (string, error) {
	content, err := gitOutputBytes(root, arguments...)
	return string(content), err
}

func gitOutputBytes(root string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	// Source inspection is action planning. Git status may otherwise refresh
	// the source checkout's index even when it reports a clean worktree.
	command.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	)
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git %s failed: %s", strings.Join(arguments, " "),
				strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	return output, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}

func safeRelative(path string) bool {
	clean := filepath.Clean(path)
	return path != "" && clean != "." && !filepath.IsAbs(path) && clean != ".." &&
		!strings.HasPrefix(clean, ".."+string(filepath.Separator)) &&
		!strings.ContainsAny(path, "\x00\n\r\t")
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func forbiddenPath(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if _, forbidden := forbiddenSourceComponents[strings.ToLower(component)]; forbidden {
			return true
		}
	}
	return false
}
