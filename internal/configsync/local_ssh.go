package configsync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// Keep the Git snapshot digest intact while projecting local-only assignments out of its cache.
func normalizeGitSSHCache(source *sourceSnapshot) ([]string, error) {
	var changed []string
	for path, candidate := range source.files {
		if source.scalarPath[path] == "" {
			continue
		}
		content, err := config.EditPersistentAssignmentContent(candidate.SourcePath, candidate.Content, "SSH_PORT", nil)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(content, candidate.Content) {
			continue
		}
		candidate.Content, candidate.Digest = content, digestBytes(content)
		source.files[path] = candidate
		changed = append(changed, path)
	}
	return changed, nil
}

// Rename preserves local settings exactly; unfinished SSH migration belongs to sync.
// A normalized managed digest proves that a raw Git port was already discarded by
// sync, including when an explicit recovery has lost the normalized cache itself.
func prepareRenameSSHCache(options Options, source *sourceSnapshot, previous Manifest) error {
	inspection := options
	inspection.inspectionOnly = true
	var raw []config.Loaded
	if err := validateCandidate(inspection, *source, previous, source.catalog, &raw); err != nil {
		return err
	}
	overrides, _, cleanup, err := projectGitSSHCache(source)
	if err != nil {
		return err
	}
	defer cleanup()
	validation := options
	validation.validationOverrides = overrides
	validation.inspectionOnly = true
	var projected []config.Loaded
	if err := validateCandidate(validation, *source, previous, source.catalog, &projected); err != nil {
		return err
	}
	ports := map[string]int{}
	for _, loaded := range projected {
		ports[loaded.Context.YardName] = loaded.Context.SSHPort
	}
	for _, loaded := range raw {
		selection, _, origin := config.ComponentSelectionSource(loaded.Settings["SSH_PORT"])
		if loaded.Context.AccessKind != domain.AccessLocal || origin != "Git settings" || ports[loaded.Context.YardName] == loaded.Context.SSHPort {
			continue
		}
		normalized := false
		for path, candidate := range source.files {
			if candidate.SourcePath != selection.Path {
				continue
			}
			for _, managed := range previous.Files {
				if managed.Path == path && managed.Digest == candidate.Digest && managed.Mode == candidate.Mode {
					normalized = true
				}
			}
		}
		if !normalized {
			return fmt.Errorf("yard %s: complete local SSH port migration before owner rename; preserved Git SSH_PORT has no normalized managed baseline", loaded.Context.YardName)
		}
	}
	validation.inspectionOnly = false
	return validateCandidate(validation, *source, previous, source.catalog)
}

// Project normalized cache bytes through temporary paths because validation reads SourcePath.
// The returned writer also stages prospective local pins for ordinary sync; rename never uses it.
func projectGitSSHCache(source *sourceSnapshot) (map[string]string, func(string, []byte) error, func(), error) {
	overrides := map[string]string{}
	temporary, err := os.MkdirTemp("/tmp", "subyard-local-ssh-*")
	if err != nil {
		return nil, nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	index := 0
	write := func(path string, content []byte) error {
		draft := filepath.Join(temporary, strconv.Itoa(index))
		index++
		if err := writeFileDurable(temporary, draft, content, 0o600); err != nil {
			return err
		}
		overrides[path] = draft
		return nil
	}
	changed, err := normalizeGitSSHCache(source)
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	for _, path := range changed {
		candidate := source.files[path]
		if err := write(candidate.SourcePath, candidate.Content); err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
	}
	return overrides, write, cleanup, nil
}

func prepareLocalSSHPorts(options Options, source *sourceSnapshot, previous Manifest, contexts []config.Loaded) (map[string]candidateFile, map[string]string, func(), error) {
	locals := map[string]candidateFile{}
	overrides, write, cleanup, err := projectGitSSHCache(source)
	if err != nil {
		return nil, nil, func() {}, err
	}
	projectedOptions := options
	projectedOptions.validationOverrides, projectedOptions.inspectionOnly = overrides, true
	var projected []config.Loaded
	if err := validateCandidate(projectedOptions, *source, previous, source.catalog, &projected); err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	projectedPorts := map[string]int{}
	for _, loaded := range projected {
		projectedPorts[loaded.Context.YardName] = loaded.Context.SSHPort
	}
	for _, loaded := range contexts {
		if loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		port := loaded.Context.SSHPort
		_, _, candidateOrigin := config.ComponentSelectionSource(loaded.Settings["SSH_PORT"])
		var live config.Loaded
		var loadErr error
		known := loaded.Context.YardName == "default"
		if !known {
			if _, rootErr := os.Stat(options.ConfigHome); errors.Is(rootErr, os.ErrNotExist) {
				loadErr = config.ErrUnknownYard
			} else {
				_, registrationErr := config.FindYardRegistrationFile(filepath.Join(options.RepositoryRoot, "config"), options.ConfigHome, loaded.Context.YardName)
				known = registrationErr == nil
				if registrationErr != nil && !errors.Is(registrationErr, config.ErrUnknownYard) {
					cleanup()
					return nil, nil, func() {}, registrationErr
				}
			}
		}
		if known {
			live, loadErr = config.Load(config.LoadOptions{Catalog: &loaded.Catalog, RepositoryRoot: options.RepositoryRoot, OperatorHome: options.OperatorHome,
				YardName: loaded.Context.YardName, ConfigLocked: options.ConfigLocked, DisablePrivate: true,
				Environment: candidateEnvironment(options.Environment, options.ConfigHome, loaded.Catalog)})
		} else {
			loadErr = config.ErrUnknownYard
		}
		liveOrigin := ""
		if loadErr != nil && !errors.Is(loadErr, config.ErrUnknownYard) {
			cleanup()
			return nil, nil, func() {}, loadErr
		}
		if loadErr == nil {
			_, _, liveOrigin = config.ComponentSelectionSource(live.Settings["SSH_PORT"])
			port = live.Context.SSHPort
		}
		if candidateOrigin != "Git settings" && liveOrigin != "Git settings" {
			continue
		}
		yard := loaded.Context.YardName
		if projectedPorts[yard] == port {
			continue
		}
		path := filepath.Join(options.ConfigHome, "yards", yard, "config.env")
		before, err := readLocalSSHFile(options.ConfigHome, path)
		if err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
		flatPath := filepath.Join(options.ConfigHome, "yards", yard+".env")
		flat, err := readLocalSSHFile(options.ConfigHome, flatPath)
		if err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
		if flat.Exists && before.Exists {
			cleanup()
			return nil, nil, func() {}, errors.New("SSH port migration requires unambiguous local yard settings")
		}
		if flat.Exists {
			path, before = flatPath, flat
		}
		value := strconv.Itoa(port)
		content, err := config.EditPersistentAssignmentContent(path, before.Content, "SSH_PORT", &value)
		if err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
		mode := uint32(0o600)
		if before.Exists {
			mode = before.Identity.Mode & 0o777
		}
		relative, err := filepath.Rel(options.ConfigHome, path)
		if err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
		locals[filepath.ToSlash(relative)] = candidateFile{Content: content, Digest: digestBytes(content), Mode: mode}
		if err := write(path, content); err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
	}

	return locals, overrides, cleanup, nil
}

func readLocalSSHFile(root, path string) (config.PersistentFileSnapshot, error) {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return config.PersistentFileSnapshot{}, nil
	}
	return config.ReadPersistentFileSnapshot(root, path)
}
