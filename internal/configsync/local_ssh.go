package configsync

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func prepareLocalSSHPorts(options Options, source *sourceSnapshot, previous Manifest, contexts []config.Loaded) (map[string]candidateFile, map[string]string, func(), error) {
	locals, overrides := map[string]candidateFile{}, map[string]string{}
	temporary, err := os.MkdirTemp("/tmp", "subyard-local-ssh-*")
	if err != nil {
		return nil, nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	write := func(path string, content []byte) error {
		file, err := os.CreateTemp(temporary, "candidate-*")
		if err != nil {
			return err
		}
		err = file.Chmod(0o600)
		if err == nil {
			_, err = file.Write(content)
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			overrides[path] = file.Name()
		}
		return err
	}
	for path, candidate := range source.files {
		if source.scalarPath[path] == "" {
			continue
		}
		content, err := config.EditPersistentAssignmentContent(candidate.SourcePath, candidate.Content, "SSH_PORT", nil)
		if err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
		if bytes.Equal(content, candidate.Content) {
			continue
		}
		candidate.Content, candidate.Digest = content, digestBytes(content)
		source.files[path] = candidate
		if err := write(candidate.SourcePath, content); err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
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
