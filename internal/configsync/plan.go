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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func BuildPlan(options Options) (Plan, error) {
	if options.SourceRoot == "" || options.ConfigHome == "" ||
		options.RepositoryRoot == "" || options.OperatorHome == "" {
		return Plan{}, errors.New("source root, config root, repository root and operator home are required")
	}
	if !options.ConfigLocked {
		unlock, err := config.LockRoot(options.ConfigHome, false)
		if err != nil {
			return Plan{}, err
		}
		defer unlock()
		options.ConfigLocked = true
	}
	if err := validateExistingConfigurationRoot(options.ConfigHome); err != nil {
		return Plan{}, err
	}
	if err := validateConfigurationAncestors(
		options.ConfigHome, TransactionPath(options.ConfigHome),
	); err != nil {
		return Plan{}, err
	}
	if _, err := os.Lstat(TransactionPath(options.ConfigHome)); err == nil {
		return Plan{}, ErrRecoveryPending
	} else if !errors.Is(err, os.ErrNotExist) {
		return Plan{}, err
	}
	hostID, initializeHostID, err := ResolveHostID(options.ConfigHome, options.Environment)
	if err != nil {
		return Plan{}, err
	}
	source, err := readSource(options, hostID)
	if err != nil {
		return Plan{}, err
	}
	previous, err := readManifest(options.ConfigHome)
	if err != nil {
		return Plan{}, err
	}
	if previous.SchemaVersion != 0 && previous.HostID != hostID {
		return Plan{}, fmt.Errorf(
			"managed configuration belongs to owner host %q, not %q", previous.HostID, hostID,
		)
	}
	if err := validateCandidate(options, source, previous); err != nil {
		return Plan{}, fmt.Errorf("candidate configuration: %w", err)
	}
	plan := Plan{
		SourceRoot: source.root, SourceID: source.id, SourceCommit: source.commit,
		SourceDigest: source.digest, HostID: hostID, InitializeHostID: initializeHostID,
		PreviousGeneration: previous.Generation, Generation: previous.Generation + 1,
		Adopt: options.Adopt, options: options, desired: source.files, previous: previous,
	}
	previousFiles := make(map[string]ManagedFile, len(previous.Files))
	for _, file := range previous.Files {
		if !safeRelative(file.Path) {
			return Plan{}, fmt.Errorf("managed manifest contains unsafe path %q", file.Path)
		}
		if _, duplicate := previousFiles[file.Path]; duplicate {
			return Plan{}, fmt.Errorf("managed manifest repeats path %q", file.Path)
		}
		previousFiles[file.Path] = file
	}
	desiredPaths := make([]string, 0, len(source.files))
	for path := range source.files {
		desiredPaths = append(desiredPaths, path)
	}
	sort.Strings(desiredPaths)
	for _, path := range desiredPaths {
		desired := source.files[path]
		target := filepath.Join(options.ConfigHome, filepath.FromSlash(path))
		live, exists, err := inspectLiveFile(options.ConfigHome, target)
		if err != nil {
			return Plan{}, err
		}
		managed, wasManaged := previousFiles[path]
		if !wasManaged {
			if exists && !options.Adopt {
				return Plan{}, fmt.Errorf(
					"unmanaged target %s already exists; repeat with --adopt after reviewing the exact plan",
					path,
				)
			}
			action, detail := "add", ""
			if exists && live.Digest == desired.Digest && live.Mode == desired.Mode {
				action, detail = "adopt", "existing content is identical"
			} else if exists {
				action, detail = "adopt-update", "existing unmanaged content differs"
			}
			plan.Changes = append(plan.Changes, Change{
				Path: path, Action: action, BeforeDigest: live.Digest,
				AfterDigest: desired.Digest, Mode: desired.Mode,
				Applications: desired.Applications, Detail: detail,
			})
			continue
		}
		delete(previousFiles, path)
		switch {
		case !exists:
			plan.Changes = append(plan.Changes, Change{
				Path: path, Action: "restore-missing", BeforeDigest: managed.Digest,
				AfterDigest: desired.Digest, Mode: desired.Mode,
				Applications: desired.Applications,
			})
		case live.Digest != managed.Digest || live.Mode != managed.Mode:
			action := "restore-drift"
			detail := "local managed content changed"
			if live.Digest == desired.Digest && live.Mode == desired.Mode {
				action = "record-converged"
				detail = "live content already matches the new source"
			}
			plan.Changes = append(plan.Changes, Change{
				Path: path, Action: action, BeforeDigest: live.Digest,
				AfterDigest: desired.Digest, Mode: desired.Mode,
				Applications: desired.Applications, Detail: detail,
			})
		case live.Digest != desired.Digest || live.Mode != desired.Mode:
			plan.Changes = append(plan.Changes, Change{
				Path: path, Action: "update", BeforeDigest: live.Digest,
				AfterDigest: desired.Digest, Mode: desired.Mode,
				Applications: desired.Applications,
			})
		}
	}
	removed := make([]string, 0, len(previousFiles))
	for path := range previousFiles {
		removed = append(removed, path)
	}
	sort.Strings(removed)
	for _, path := range removed {
		managed := previousFiles[path]
		target := filepath.Join(options.ConfigHome, filepath.FromSlash(path))
		live, exists, err := inspectLiveFile(options.ConfigHome, target)
		if err != nil {
			return Plan{}, err
		}
		if err := guardDeletedYardDefinition(options, path); err != nil {
			return Plan{}, err
		}
		if !exists {
			plan.Changes = append(plan.Changes, Change{
				Path: path, Action: "record-deleted", BeforeDigest: managed.Digest,
				Mode: managed.Mode, Applications: applicationsForPath(path),
				Detail: "managed live path is already missing",
			})
			continue
		}
		if live.Digest != managed.Digest || live.Mode != managed.Mode {
			return Plan{}, fmt.Errorf(
				"managed target %s has local drift and cannot be deleted", path,
			)
		}
		plan.Changes = append(plan.Changes, Change{
			Path: path, Action: "delete", BeforeDigest: live.Digest,
			Mode: managed.Mode, Applications: applicationsForPath(path),
		})
	}
	sort.Slice(plan.Changes, func(left, right int) bool {
		return plan.Changes[left].Path < plan.Changes[right].Path
	})
	plan.ManifestChanged = initializeHostID || previous.SchemaVersion == 0 ||
		previous.SourceID != source.id || previous.SourceCommit != source.commit ||
		previous.SourceDigest != source.digest || len(plan.Changes) != 0
	if !plan.ManifestChanged {
		plan.Generation = previous.Generation
	}
	plan.Digest = planDigest(plan)
	return plan, nil
}

func guardDeletedYardDefinition(options Options, path string) error {
	yard, definition := deletedYardDefinition(path)
	if !definition || options.YardInUse == nil {
		return nil
	}
	reason, inUse, err := options.YardInUse(yard)
	if err != nil {
		return fmt.Errorf("check yard %s deletion: %w", yard, err)
	}
	if inUse {
		return fmt.Errorf(
			"cannot delete yard %s definition while authoritative state exists: %s",
			yard, reason,
		)
	}
	return nil
}

func validateCandidate(options Options, source sourceSnapshot, previous Manifest) error {
	previousFiles := map[string]struct{}{}
	for _, file := range previous.Files {
		previousFiles[file.Path] = struct{}{}
	}
	selectScalar := func(target string) (string, error) {
		if candidate, ok := source.files[target]; ok {
			return candidate.SourcePath, nil
		}
		if _, managed := previousFiles[target]; managed {
			return "", nil
		}
		path := filepath.Join(options.ConfigHome, filepath.FromSlash(target))
		if _, exists, err := inspectLiveFile(options.ConfigHome, path); err != nil {
			return "", err
		} else if exists {
			return path, nil
		}
		return "", nil
	}
	shared, err := selectScalar("overrides/shared/config.env")
	if err != nil {
		return err
	}
	host, err := selectScalar("config.env")
	if err != nil {
		return err
	}
	yardSettings := map[string]string{}
	yardNames := map[string]struct{}{}
	for _, name := range source.yardNames {
		yardNames[name] = struct{}{}
	}
	liveYards := filepath.Join(options.ConfigHome, "yards")
	entries, err := os.ReadDir(liveYards)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			if strings.HasSuffix(name, ".env") {
				return fmt.Errorf("legacy yard input %s must be migrated before versioned sync", name)
			}
			continue
		}
		if !domain.SafeName(name) || name == "default" {
			return fmt.Errorf("invalid live yard directory %q", name)
		}
		target := filepath.ToSlash(filepath.Join("yards", name, "config.env"))
		if _, managed := previousFiles[target]; managed {
			if _, desired := source.files[target]; !desired {
				continue
			}
		}
		path := filepath.Join(liveYards, name, "config.env")
		if _, exists, err := inspectLiveFile(options.ConfigHome, path); err != nil {
			return err
		} else if exists {
			yardNames[name] = struct{}{}
		}
	}
	for name := range yardNames {
		target := filepath.ToSlash(filepath.Join("yards", name, "config.env"))
		path, err := selectScalar(target)
		if err != nil {
			return err
		}
		if path == "" {
			return fmt.Errorf("yard %s has no candidate definition", name)
		}
		yardSettings[name] = path
	}
	environment := candidateEnvironment(options.Environment, options.ConfigHome)
	sharedAssetSource := filepath.Join(source.root, "shared", "overrides", "agents")
	hostAssetSource := filepath.Join(
		source.root, "hosts", source.hostID, "overrides", "agents",
	)
	layerPaths := &config.LayerPaths{
		SharedSettings: shared,
		SharedAssets: candidateAssetRoot(
			source.files, previous, "overrides/shared/agents/",
			sharedAssetSource, filepath.Join(options.ConfigHome, "overrides", "shared", "agents"),
		),
		HostSettings: host,
		HostAssets: candidateAssetRoot(
			source.files, previous, "overrides/host/agents/",
			hostAssetSource, filepath.Join(options.ConfigHome, "overrides", "host", "agents"),
		),
		YardSettings: yardSettings, YardAssets: map[string]string{},
	}
	for name := range yardNames {
		sourceRoot := filepath.Join(
			source.root, "hosts", source.hostID, "yards", name, "overrides", "agents",
		)
		prefix := filepath.ToSlash(filepath.Join("yards", name, "overrides", "agents")) + "/"
		layerPaths.YardAssets[name] = candidateAssetRoot(
			source.files, previous, prefix, sourceRoot,
			filepath.Join(options.ConfigHome, "yards", name, "overrides", "agents"),
		)
	}
	contexts := make([]config.Loaded, 0, len(yardNames)+1)
	load := func(name string) error {
		loaded, err := config.Load(config.LoadOptions{
			RepositoryRoot: options.RepositoryRoot, OperatorHome: options.OperatorHome,
			YardName: name, Environment: environment, DisablePrivate: true,
			ConfigLocked: options.ConfigLocked, LayerPaths: layerPaths,
		})
		if err != nil {
			return fmt.Errorf("yard %s: %w", name, err)
		}
		contexts = append(contexts, loaded)
		return nil
	}
	if err := load("default"); err != nil {
		return err
	}
	names := make([]string, 0, len(yardNames))
	for name := range yardNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := load(name); err != nil {
			return err
		}
	}
	return validateInventory(contexts)
}

func candidateAssetRoot(
	desired map[string]candidateFile,
	previous Manifest,
	targetPrefix, sourceRoot, liveRoot string,
) string {
	for path := range desired {
		if strings.HasPrefix(filepath.ToSlash(path), targetPrefix) {
			return sourceRoot
		}
	}
	for _, file := range previous.Files {
		if strings.HasPrefix(filepath.ToSlash(file.Path), targetPrefix) {
			return sourceRoot
		}
	}
	return liveRoot
}

func candidateEnvironment(source map[string]string, configHome string) map[string]string {
	result := make(map[string]string, len(source)+1)
	for name, value := range source {
		if _, setting := config.LookupSetting(name); setting {
			continue
		}
		result[name] = value
	}
	result["SUBYARD_CONFIG_HOME"] = configHome
	return result
}

func validateInventory(contexts []config.Loaded) error {
	type owner struct {
		yard string
		kind string
	}
	resources := map[string]owner{}
	for _, loaded := range contexts {
		ctx := loaded.Context
		if ctx.AccessKind == domain.AccessRemote {
			continue
		}
		values := []struct {
			kind, value string
		}{
			{"SSH port", strconv.Itoa(ctx.SSHPort)},
			{"Incus project", ctx.IncusProject},
			{"instance", ctx.YardInstanceName},
			{"host data root", ctx.Paths.HostBase},
		}
		for _, value := range values {
			if value.value == "" || value.value == "0" {
				continue
			}
			key := value.kind + "\x00" + value.value
			if existing, duplicate := resources[key]; duplicate {
				return fmt.Errorf(
					"%s %q is used by both yard %s and yard %s",
					value.kind, value.value, existing.yard, ctx.YardName,
				)
			}
			resources[key] = owner{yard: ctx.YardName, kind: value.kind}
		}
	}
	return nil
}

type liveFile struct {
	Digest string
	Mode   uint32
}

func inspectLiveFile(configHome, path string) (liveFile, bool, error) {
	if !pathWithin(path, configHome) {
		return liveFile{}, false, errors.New("live target escaped configuration root")
	}
	if err := validateConfigurationAncestors(configHome, path); err != nil {
		return liveFile{}, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return liveFile{}, false, nil
	}
	if err != nil {
		return liveFile{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return liveFile{}, false, fmt.Errorf("live target is not a regular non-symlink file: %s", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return liveFile{}, false, fmt.Errorf("live target is group/world writable: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return liveFile{}, false, fmt.Errorf("live target has unsafe ownership or hard links: %s", path)
	}
	maximum := int64(maxAssetBytes)
	if filepath.Base(path) == "config.env" {
		maximum = maxConfigBytes
		if info.Mode().Perm() != 0o600 {
			return liveFile{}, false, fmt.Errorf("live config.env mode must be 0600: %s", path)
		}
	}
	if info.Size() > int64(maximum) {
		return liveFile{}, false, fmt.Errorf("live target exceeds size limit: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return liveFile{}, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return liveFile{}, false, err
	}
	if int64(len(content)) > maximum {
		return liveFile{}, false, fmt.Errorf("live target exceeds size limit: %s", path)
	}
	return liveFile{Digest: digestBytes(content), Mode: uint32(info.Mode().Perm())}, true, nil
}

func readManifest(configHome string) (Manifest, error) {
	path := ManifestPath(configHome)
	if err := validateConfigurationAncestors(configHome, path); err != nil {
		return Manifest{}, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, nil
	}
	if err != nil {
		return Manifest{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 {
		return Manifest{}, errors.New("config sync manifest must be a regular 0600 file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Manifest{}, err
	}
	if manifest.SchemaVersion != manifestSchema {
		return Manifest{}, fmt.Errorf(
			"unsupported config sync manifest schema %d", manifest.SchemaVersion,
		)
	}
	if manifest.Generation == 0 || !safeHostID(manifest.HostID) ||
		!validGitOID(manifest.SourceCommit) ||
		!validHexDigest(manifest.SourceID, sha256.Size*2) ||
		!validHexDigest(manifest.SourceDigest, sha256.Size*2) {
		return Manifest{}, errors.New("config sync manifest is invalid")
	}
	return manifest, nil
}

func validGitOID(value string) bool {
	return validHexDigest(value, 40) || validHexDigest(value, 64)
}

func validHexDigest(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func deletedYardDefinition(path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) == 3 && parts[0] == "yards" && parts[2] == "config.env" &&
		domain.SafeName(parts[1]) {
		return parts[1], true
	}
	return "", false
}

func applicationsForPath(path string) []config.SettingApplication {
	if strings.Contains(filepath.ToSlash(path), "/overrides/") ||
		strings.HasPrefix(filepath.ToSlash(path), "overrides/") {
		return []config.SettingApplication{config.SettingConfigApply}
	}
	return []config.SettingApplication{config.SettingYardInit}
}

func planDigest(plan Plan) string {
	type digestPlan struct {
		SourceID         string
		SourceCommit     string
		SourceDigest     string
		HostID           string
		InitializeHostID bool
		Previous         uint64
		Generation       uint64
		Changes          []Change
		Adopt            bool
	}
	content, _ := json.Marshal(digestPlan{
		SourceID: plan.SourceID, SourceCommit: plan.SourceCommit,
		SourceDigest: plan.SourceDigest, HostID: plan.HostID,
		InitializeHostID: plan.InitializeHostID, Previous: plan.PreviousGeneration,
		Generation: plan.Generation, Changes: plan.Changes, Adopt: plan.Adopt,
	})
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
