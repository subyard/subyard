package migration

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const (
	SourceBaseCheckout   = "source-root"
	SourceBaseDataHome   = "data-home"
	SourceBaseConfigHome = "config-home"

	MaxLegacyYardConfigBytes = 1 << 20

	DestinationConfigHome = "config-home"

	ContentTransformRetiredE2EVMTemplate = "yard-template-e2e-vms-to-test-vms"
)

type SourceInstallManifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	SourceRoot    string               `json:"sourceRoot"`
	DataHome      string               `json:"dataHome,omitempty"`
	ConfigHome    string               `json:"configHome,omitempty"`
	Entries       []SourceInstallEntry `json:"entries"`
}

type SourceInstallEntry struct {
	SourceBase               string `json:"sourceBase"`
	Source                   string `json:"source"`
	DestinationRoot          string `json:"destinationRoot"`
	Destination              string `json:"destination"`
	SemanticOwner            string `json:"semanticOwner"`
	Consumer                 string `json:"consumer"`
	Kind                     string `json:"kind"`
	Scope                    string `json:"scope"`
	Mode                     string `json:"mode"`
	SyncPolicy               string `json:"syncPolicy"`
	ConflictPolicy           string `json:"conflictPolicy"`
	ContentTransform         string `json:"contentTransform,omitempty"`
	AuthoritativeDestination string `json:"authoritativeDestination,omitempty"`
	Secret                   bool   `json:"secret"`
}

// DiscoverSourceInstall is the host-free source-checkout form used by unit
// tests. The installer uses DiscoverSourceInstallWithRoots to include layouts
// written by earlier installed versions.
func DiscoverSourceInstall(sourceRoot string) (SourceInstallManifest, error) {
	return DiscoverSourceInstallWithRoots(sourceRoot, "", "")
}

// DiscoverSourceInstallWithRoots returns the complete, explicit compatibility
// set. It validates every selected input before the installer changes an
// entrypoint and never serializes file contents.
func DiscoverSourceInstallWithRoots(
	sourceRoot string,
	dataHome string,
	configHome string,
) (SourceInstallManifest, error) {
	root, err := realOwnedDirectory(sourceRoot, true)
	if err != nil {
		return SourceInstallManifest{}, fmt.Errorf("source root: %w", err)
	}
	dataRoot, err := optionalOwnedRoot(dataHome)
	if err != nil {
		return SourceInstallManifest{}, fmt.Errorf("data home: %w", err)
	}
	configRoot, err := optionalOwnedRoot(configHome)
	if err != nil {
		return SourceInstallManifest{}, fmt.Errorf("config home: %w", err)
	}
	manifest := SourceInstallManifest{
		SchemaVersion: 2, SourceRoot: root, DataHome: dataRoot, ConfigHome: configRoot,
	}
	selected := make(map[string]struct{})
	add := func(
		base, baseRoot, source, destination, owner, consumer, kind, scope, syncPolicy string,
		secret bool,
	) error {
		source = filepath.Clean(source)
		destination = filepath.Clean(destination)
		if !safeRelativePath(source) || !safeRelativePath(destination) {
			return fmt.Errorf("unsafe source-install manifest path: %s", source)
		}
		if baseRoot == "" {
			return fmt.Errorf("source base %s is unavailable", base)
		}
		absolute := filepath.Join(baseRoot, source)
		if err := validateSourceFile(absolute); err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		contentTransform := ""
		if kind == "yard-config" || kind == "flat-yard-config" {
			values, err := config.ReadAssignments(absolute)
			if err != nil {
				return fmt.Errorf("%s: %w", source, err)
			}
			if values["YARD_TEMPLATE"] == "e2e-vms" {
				contentTransform = ContentTransformRetiredE2EVMTemplate
			}
		}
		authoritative := ""
		switch kind {
		case "legacy-staging-secret":
			authoritative = filepath.Join("generated", "staging", filepath.Base(destination))
		case "legacy-qa-secrets":
			authoritative = filepath.Join("generated", "qa-pool", "secrets.env")
		case "legacy-qa-pool":
			authoritative = filepath.Join("generated", "qa-pool", "pool.jsonl")
		}
		manifest.Entries = append(manifest.Entries, SourceInstallEntry{
			SourceBase: base, Source: filepath.ToSlash(source),
			DestinationRoot: DestinationConfigHome, Destination: filepath.ToSlash(destination),
			SemanticOwner: owner, Consumer: consumer, Kind: kind, Scope: scope,
			Mode: "0600", SyncPolicy: syncPolicy, ConflictPolicy: "identical-or-fail",
			ContentTransform:         contentTransform,
			AuthoritativeDestination: filepath.ToSlash(authoritative), Secret: secret,
		})
		selected[base+"\x00"+filepath.Clean(source)] = struct{}{}
		return nil
	}

	if err := discoverCheckoutInputs(root, add); err != nil {
		return SourceInstallManifest{}, err
	}
	if dataRoot != "" {
		if err := discoverPreviousInstalledInputs(dataRoot, add, selected); err != nil {
			return SourceInstallManifest{}, err
		}
	}
	if configRoot != "" {
		if err := discoverFlatInstalledYards(configRoot, add); err != nil {
			return SourceInstallManifest{}, err
		}
	}
	sort.Slice(manifest.Entries, func(left, right int) bool {
		if manifest.Entries[left].Destination != manifest.Entries[right].Destination {
			return manifest.Entries[left].Destination < manifest.Entries[right].Destination
		}
		if manifest.Entries[left].SourceBase != manifest.Entries[right].SourceBase {
			return manifest.Entries[left].SourceBase < manifest.Entries[right].SourceBase
		}
		return manifest.Entries[left].Source < manifest.Entries[right].Source
	})
	if err := validatePersistedAgentReferences(root, dataRoot, manifest.Entries); err != nil {
		return SourceInstallManifest{}, err
	}
	return manifest, nil
}

var retiredE2EVMTemplateAssignment = regexp.MustCompile(
	`^([ \t]*)(?:export[ \t]+)?YARD_TEMPLATE[ \t]*=[ \t]*(?:"e2e-vms"|'e2e-vms'|e2e-vms)[ \t]*$`,
)

// NormalizeLegacyYardConfigContent replaces exactly one direct, static retired
// template assignment while preserving every other byte of the config.
func NormalizeLegacyYardConfigContent(payload []byte) ([]byte, error) {
	if len(payload) > MaxLegacyYardConfigBytes {
		return nil, errors.New("legacy yard config exceeds its size bound")
	}
	assignments, err := config.ParsePersistentAssignments("legacy yard config", payload)
	if err != nil {
		return nil, err
	}
	found := false
	for _, assignment := range assignments {
		if assignment.Name != "YARD_TEMPLATE" {
			continue
		}
		if !assignment.Direct || assignment.Dynamic {
			return nil, errors.New("retired YARD_TEMPLATE assignment must be direct and static")
		}
		if assignment.Value != "e2e-vms" {
			return nil, errors.New("yard config does not select the retired e2e-vms template")
		}
		if found {
			return nil, errors.New("yard config has duplicate retired YARD_TEMPLATE assignments")
		}
		found = true
	}
	if !found {
		return nil, errors.New("yard config does not select the retired e2e-vms template")
	}
	lines := bytes.SplitAfter(payload, []byte("\n"))
	replaced := 0
	var normalized bytes.Buffer
	for _, line := range lines {
		body := bytes.TrimSuffix(line, []byte("\n"))
		ending := line[len(body):]
		if match := retiredE2EVMTemplateAssignment.FindSubmatch(body); match != nil {
			normalized.Write(match[1])
			normalized.WriteString("YARD_TEMPLATE=test-vms")
			normalized.Write(ending)
			replaced++
			continue
		}
		normalized.Write(line)
	}
	if replaced != 1 {
		return nil, errors.New("retired YARD_TEMPLATE assignment uses an unsupported form")
	}
	return normalized.Bytes(), nil
}

type manifestAdd func(
	base, baseRoot, source, destination, owner, consumer, kind, scope, syncPolicy string,
	secret bool,
) error

func discoverCheckoutInputs(root string, add manifestAdd) error {
	if exists, err := optionalRegular(filepath.Join(root, "private", "config.env")); err != nil {
		return err
	} else if exists {
		if err := add(SourceBaseCheckout, root, "private/config.env", "config.env",
			"config-resolver", "config-loader", "host-config", "host", "host-only", true); err != nil {
			return err
		}
	}
	if err := discoverYards(SourceBaseCheckout, root, "private/yards", add); err != nil {
		return err
	}
	if err := discoverAgentAssets(
		SourceBaseCheckout, root, "private/agents", "overrides/host/agents", add,
	); err != nil {
		return err
	}
	if err := discoverProfileInputs(SourceBaseCheckout, root, "config", add); err != nil {
		return err
	}
	if err := discoverStagingInputs(SourceBaseCheckout, root, "config", add); err != nil {
		return err
	}
	return discoverQAAndFingerprintInputs(SourceBaseCheckout, root, "config", add)
}

func discoverPreviousInstalledInputs(
	dataRoot string,
	add manifestAdd,
	selected map[string]struct{},
) error {
	if exists, err := optionalRegular(filepath.Join(dataRoot, "config.env")); err != nil {
		return err
	} else if exists {
		if err := add(SourceBaseDataHome, dataRoot, "config.env", "config.env",
			"config-resolver", "config-loader", "previous-host-config", "host", "host-only", true); err != nil {
			return err
		}
	}
	if err := discoverAgentAssets(
		SourceBaseDataHome, dataRoot, "operator-overlay/private/agents",
		"overrides/host/agents", add,
	); err != nil {
		return err
	}
	if err := discoverProfileInputs(
		SourceBaseDataHome, dataRoot, "operator-overlay/config", add,
	); err != nil {
		return err
	}
	if err := discoverStagingInputs(
		SourceBaseDataHome, dataRoot, "operator-overlay/config", add,
	); err != nil {
		return err
	}
	if err := discoverQAAndFingerprintInputs(
		SourceBaseDataHome, dataRoot, "operator-overlay/config", add,
	); err != nil {
		return err
	}
	overlay := filepath.Join(dataRoot, "operator-overlay")
	exists, err := optionalDirectory(overlay)
	if err != nil || !exists {
		return err
	}
	return filepath.WalkDir(overlay, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(dataRoot, path)
		if err != nil || !safeRelativePath(relative) {
			return errors.New("legacy operator overlay escapes the data home")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if err := validateOwnedSafeMode(path, info); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("legacy operator overlay contains a symlink: %s", relative)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("legacy operator overlay contains a special file: %s", relative)
		}
		if _, ok := selected[SourceBaseDataHome+"\x00"+filepath.Clean(relative)]; ok {
			return nil
		}
		destination := filepath.Join("secrets", "legacy", "operator-overlay",
			strings.TrimPrefix(relative, "operator-overlay"+string(filepath.Separator)))
		return add(SourceBaseDataHome, dataRoot, relative, destination,
			"operator-review", "unsupported-legacy", "unclassified-legacy",
			"host", "never-sync", true)
	})
}

func discoverFlatInstalledYards(configRoot string, add manifestAdd) error {
	directory := filepath.Join(configRoot, "yards")
	entries, err := optionalDirectoryEntries(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".env" {
			return fmt.Errorf("config-home yards contains unsupported entry %q", entry.Name())
		}
		name := strings.TrimSuffix(entry.Name(), ".env")
		if !domain.SafeName(name) {
			return fmt.Errorf("unsafe installed yard name %q", name)
		}
		source := filepath.Join("yards", entry.Name())
		destination := filepath.Join("yards", name, "config.env")
		if err := add(SourceBaseConfigHome, configRoot, source, destination,
			"config-resolver", "yard-config-loader", "flat-yard-config",
			"yard", "selected-yard-only", true); err != nil {
			return err
		}
	}
	return nil
}

func discoverYards(base, root, relativeDirectory string, add manifestAdd) error {
	directory := filepath.Join(root, relativeDirectory)
	entries, err := optionalDirectoryEntries(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			filepath.Ext(entry.Name()) != ".env" {
			return fmt.Errorf("%s contains unsupported entry %q", relativeDirectory, entry.Name())
		}
		name := strings.TrimSuffix(entry.Name(), ".env")
		if !domain.SafeName(name) {
			return fmt.Errorf("unsafe legacy yard name %q", name)
		}
		source := filepath.Join(relativeDirectory, entry.Name())
		destination := filepath.Join("yards", name, "config.env")
		if err := add(base, root, source, destination,
			"config-resolver", "yard-config-loader", "yard-config",
			"yard", "selected-yard-only", true); err != nil {
			return err
		}
	}
	return nil
}

func discoverAgentAssets(
	base, root, relativeDirectory, destinationDirectory string,
	add manifestAdd,
) error {
	directory := filepath.Join(root, relativeDirectory)
	exists, err := optionalDirectory(directory)
	if err != nil || !exists {
		return err
	}
	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !safeRelativePath(relative) {
			return errors.New("private agent path escapes its source root")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("private agent input contains a symlink: %s", relative)
		}
		if err := validateOwnedSafeMode(path, info); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("private agent input contains a special file: %s", relative)
		}
		underAgents, err := filepath.Rel(filepath.Join(root, relativeDirectory), path)
		if err != nil || !safeRelativePath(underAgents) {
			return errors.New("private agent asset escapes its logical root")
		}
		destination := filepath.Join(destinationDirectory, underAgents)
		return add(base, root, relative, destination,
			"agent-config-reconciler", "agent-config-reconciler", "agent-asset",
			"host", "host-only", false)
	})
}

func discoverProfileInputs(base, root, configPrefix string, add manifestAdd) error {
	profilesRelative := filepath.Join(configPrefix, "profiles")
	profiles := filepath.Join(root, profilesRelative)
	entries, err := optionalDirectoryEntries(profiles)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			if !domain.SafeName(entry.Name()) {
				return fmt.Errorf("unsafe profile name %q", entry.Name())
			}
			source := filepath.Join(profilesRelative, entry.Name(), "profile.env")
			exists, err := optionalRegular(filepath.Join(root, source))
			if err != nil {
				return err
			}
			if exists {
				destination := filepath.Join("secrets", "profiles", entry.Name(), "profile.env")
				if err := add(base, root, source, destination,
					"project-environment", "project-environment", "profile-secret",
					"host", "never-sync", true); err != nil {
					return err
				}
			}
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".env" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".env")
		if !domain.SafeName(name) {
			return fmt.Errorf("unsafe legacy profile input %q", entry.Name())
		}
		source := filepath.Join(profilesRelative, entry.Name())
		destination := filepath.Join("secrets", "legacy", "profiles", entry.Name())
		if err := add(base, root, source, destination,
			"operator-review", "unsupported-legacy", "legacy-profile-secret",
			"host", "never-sync", true); err != nil {
			return err
		}
	}
	return nil
}

func discoverStagingInputs(base, root, configPrefix string, add manifestAdd) error {
	relativeDirectory := filepath.Join(configPrefix, "staging")
	entries, err := optionalDirectoryEntries(filepath.Join(root, relativeDirectory))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		extension := filepath.Ext(entry.Name())
		if extension != ".conf" && extension != ".env" || strings.HasSuffix(entry.Name(), ".example") {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s contains an invalid runtime input %q", relativeDirectory, entry.Name())
		}
		name := strings.TrimSuffix(entry.Name(), extension)
		if !domain.SafeName(name) {
			return fmt.Errorf("unsafe staging zone name %q", name)
		}
		source := filepath.Join(relativeDirectory, entry.Name())
		destination := filepath.Join("overrides", "host", "staging", entry.Name())
		owner, consumer, kind, syncPolicy := "staging", "staging", "staging-config", "host-only"
		secret := false
		if extension == ".env" {
			destination = filepath.Join("secrets", "legacy", "staging", entry.Name())
			owner, consumer, kind, syncPolicy = "credential-ledger", "operator-import", "legacy-staging-secret", "never-sync"
			secret = true
		}
		if err := add(base, root, source, destination,
			owner, consumer, kind, "host", syncPolicy, secret); err != nil {
			return err
		}
	}
	return nil
}

func discoverQAAndFingerprintInputs(base, root, configPrefix string, add manifestAdd) error {
	type input struct {
		source, destination, owner, consumer, kind, sync string
		secret                                           bool
	}
	inputs := []input{
		{filepath.Join(configPrefix, "prod-fingerprints"),
			"overrides/host/prod-fingerprints", "production-guard",
			"credential-and-staging-production-guard", "production-fingerprints", "host-only", false},
		{filepath.Join(configPrefix, "qa-pool", "broker.conf"),
			"overrides/host/qa-pool/broker.conf", "qa-pool",
			"qa-pool", "qa-broker-config", "host-only", false},
		{filepath.Join(configPrefix, "qa-pool", "secrets.env"),
			"secrets/legacy/qa-pool/secrets.env", "credential-ledger",
			"operator-import", "legacy-qa-secrets", "never-sync", true},
		{filepath.Join(configPrefix, "qa-pool", "pool.jsonl"),
			"secrets/legacy/qa-pool/pool.jsonl", "credential-ledger",
			"operator-import", "legacy-qa-pool", "never-sync", true},
	}
	recognized := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		recognized[filepath.Clean(input.source)] = struct{}{}
		exists, err := optionalRegular(filepath.Join(root, input.source))
		if err != nil {
			return err
		}
		if exists {
			if err := add(base, root, input.source, input.destination,
				input.owner, input.consumer, input.kind, "host", input.sync, input.secret); err != nil {
				return err
			}
		}
	}
	qaRelative := filepath.Join(configPrefix, "qa-pool")
	entries, err := optionalDirectoryEntries(filepath.Join(root, qaRelative))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".example") {
			continue
		}
		source := filepath.Join(qaRelative, entry.Name())
		if _, ok := recognized[filepath.Clean(source)]; ok {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s contains an invalid ignored input %q", qaRelative, entry.Name())
		}
		destination := filepath.Join("secrets", "legacy", "unclassified", "qa-pool", entry.Name())
		if err := add(base, root, source, destination,
			"operator-review", "unsupported-legacy", "unclassified-legacy",
			"host", "never-sync", true); err != nil {
			return err
		}
	}
	return nil
}

func validatePersistedAgentReferences(
	sourceRoot string,
	dataRoot string,
	entries []SourceInstallEntry,
) error {
	configs := []struct {
		base, root, path string
	}{
		{SourceBaseCheckout, sourceRoot, filepath.Join(sourceRoot, "private", "config.env")},
	}
	if dataRoot != "" {
		configs = append(configs, struct{ base, root, path string }{
			SourceBaseDataHome, dataRoot, filepath.Join(dataRoot, "config.env"),
		})
	}
	allowlisted := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var root string
		switch entry.SourceBase {
		case SourceBaseCheckout:
			root = sourceRoot
		case SourceBaseDataHome:
			root = dataRoot
		default:
			continue
		}
		if root != "" {
			allowlisted[filepath.Join(root, filepath.FromSlash(entry.Source))] = struct{}{}
		}
	}
	for _, persisted := range configs {
		exists, err := optionalRegular(persisted.path)
		if err != nil || !exists {
			if err != nil {
				return err
			}
			continue
		}
		configDirectory := filepath.Join(sourceRoot, "config")
		if persisted.base == SourceBaseDataHome {
			configDirectory = filepath.Join(dataRoot, "operator-overlay", "config")
		}
		values, err := config.ReadAssignmentsOver(persisted.path, map[string]string{
			"SUBYARD_CONFIG_DIR": configDirectory,
			"SUBYARD_HOME":       dataRoot,
		})
		if err != nil {
			return fmt.Errorf("parse %s: %w", persisted.path, err)
		}
		for name, value := range values {
			if !sourceValuedAgentKey(name) || value == "" {
				continue
			}
			if !filepath.IsAbs(value) {
				return fmt.Errorf("%s must use an absolute regular file", name)
			}
			path := filepath.Clean(value)
			if pathWithin(path, sourceRoot) || dataRoot != "" && pathWithin(path, dataRoot) {
				if _, ok := allowlisted[path]; !ok {
					relative, _ := filepath.Rel(persisted.root, path)
					return fmt.Errorf("%s references unsupported source-install input %s", name, relative)
				}
			}
			if err := validateSourceFile(path); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

func sourceValuedAgentKey(name string) bool {
	if !strings.HasPrefix(name, "AGENT_") {
		return false
	}
	for _, suffix := range []string{"_CONFIG", "_RULES", "_PROVISION"} {
		agent, found := strings.CutSuffix(strings.TrimPrefix(name, "AGENT_"), suffix)
		if found && domain.SafeName(agent) {
			return true
		}
	}
	return false
}

func optionalOwnedRoot(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Clean(absolute), nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("root must be a real directory")
	}
	if err := validateOwnedSafeMode(absolute, info); err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func realOwnedDirectory(path string, required bool) (string, error) {
	if path == "" {
		if required {
			return "", errors.New("path is required")
		}
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if filepath.Clean(absolute) == string(filepath.Separator) {
		return "", errors.New("filesystem root is not allowed")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("path must be a real directory")
	}
	if err := validateOwnedSafeMode(absolute, info); err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func validateSourceFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("source file is unavailable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("source must be a regular non-symlink file")
	}
	if err := validateOwnedSafeMode(path, info); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("source file is not readable: %w", err)
	}
	return file.Close()
}

func validateOwnedSafeMode(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("source-install input is not operator-owned: %s", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("source-install input is group/world writable: %s", path)
	}
	return nil
}

func optionalRegular(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("source-install input must be a regular non-symlink file: %s", path)
	}
	return true, nil
}

func optionalDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("source-install input must be a real directory: %s", path)
	}
	if err := validateOwnedSafeMode(path, info); err != nil {
		return false, err
	}
	return true, nil
}

func optionalDirectoryEntries(path string) ([]os.DirEntry, error) {
	exists, err := optionalDirectory(path)
	if err != nil || !exists {
		return nil, err
	}
	return os.ReadDir(path)
}

func safeRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r\t") {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func pathWithin(path, root string) bool {
	if root == "" {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
