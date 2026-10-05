package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// Owner inputs remain private. Protected file contents are never read here;
// their path, ownership and filesystem identity bind the later native transfer.
type ownerInputBaseline struct {
	initial     config.Loaded
	preset      string
	files       map[string]ownerInputFile
	peers       []domain.Context
	runtimeRoot string
}

type ownerInputFile struct {
	Exists            bool
	Mode              os.FileMode
	Size              int64
	Device, Inode     uint64
	UID, GID          uint32
	Modified, Changed syscall.Timespec
	Link              string
}

func ownerInputFiles(loaded config.Loaded) (map[string]ownerInputFile, error) {
	paths := map[string]bool{}
	for _, layer := range loaded.ConfigurationLayers {
		if filepath.IsAbs(layer.Path) {
			paths[layer.Path] = true
		}
	}
	for _, trace := range loaded.Settings {
		for _, resolution := range trace.Resolutions {
			if filepath.IsAbs(resolution.Path) {
				paths[resolution.Path] = true
			}
		}
		if (trace.Kind == config.SettingFile || trace.Type == config.SettingRegularFilePath) && filepath.IsAbs(trace.EffectiveValue) {
			paths[trace.EffectiveValue] = true
		}
	}
	facts := map[string]ownerInputFile{}
	for path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			facts[path] = ownerInputFile{}
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			fact, err := ownerInputIdentity(path, info)
			if err != nil {
				return nil, err
			}
			facts[path] = fact
			continue
		}
		err = filepath.WalkDir(path, func(child string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if len(facts) >= 4096 {
				return errors.New("operation configuration input tree exceeds its bound")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			fact, err := ownerInputIdentity(child, info)
			facts[child] = fact
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return facts, nil
}

func ownerInputIdentity(path string, info os.FileInfo) (ownerInputFile, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ownerInputFile{}, errors.New("cannot identify operation input")
	}
	fact := ownerInputFile{Exists: true, Mode: info.Mode(), Size: info.Size(), Device: uint64(stat.Dev), Inode: stat.Ino, Modified: stat.Mtim, Changed: stat.Ctim}
	fact.UID, fact.GID = stat.Uid, stat.Gid
	if info.Mode()&os.ModeSymlink != 0 {
		var err error
		fact.Link, err = os.Readlink(path)
		if err != nil {
			return ownerInputFile{}, err
		}
	}
	return fact, nil
}

func (cli *CLI) captureOwnerInputs(loaded config.Loaded, preset string, peers []domain.Context) (*ownerInputBaseline, error) {
	files, err := ownerInputFiles(loaded)
	if err != nil {
		return nil, err
	}
	if err := captureRuntimeSources(cli.options.RepositoryRoot, files); err != nil {
		return nil, err
	}
	return &ownerInputBaseline{initial: loaded, preset: preset, files: files, peers: peers, runtimeRoot: cli.options.RepositoryRoot}, nil
}

func (baseline *ownerInputBaseline) binding() string {
	if baseline == nil {
		return ""
	}
	return operationStateDigest(struct {
		Context  domain.Context
		Settings map[string]config.SettingTrace
		Files    map[string]ownerInputFile
		Peers    []domain.Context
	}{baseline.initial.Context, baseline.initial.Settings, baseline.files, baseline.peers})
}

func (baseline *ownerInputBaseline) check(_ context.Context, cli *CLI) error {
	if baseline == nil {
		return nil
	}
	fresh, err := config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome: baseline.initial.Context.Paths.OperatorHome, YardName: baseline.initial.Context.YardName,
		YardSettingsFile: baseline.preset, Environment: cli.baseEnv})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh.Context, baseline.initial.Context) || !reflect.DeepEqual(fresh.Settings, baseline.initial.Settings) {
		return fmt.Errorf("%w: owner configuration inputs changed", domain.ErrPlanStale)
	}
	files, err := ownerInputFiles(fresh)
	if err != nil {
		return err
	}
	if err := captureRuntimeSources(baseline.runtimeRoot, files); err != nil {
		return err
	}
	if !reflect.DeepEqual(files, baseline.files) {
		return fmt.Errorf("%w: owner configuration source identity changed", domain.ErrPlanStale)
	}
	if baseline.peers != nil {
		peers, err := cli.powerYardContexts(fresh)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(peers, baseline.peers) {
			return fmt.Errorf("%w: registered owner yard scope changed", domain.ErrPlanStale)
		}
	}
	return nil
}

func captureRuntimeSources(root string, facts map[string]ownerInputFile) error {
	for _, directory := range []string{"config", "scripts"} {
		path := filepath.Join(root, directory)
		if !filepath.IsAbs(path) {
			return errors.New("operation runtime root must be absolute")
		}
		err := filepath.WalkDir(path, func(child string, entry os.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) && child == path {
				facts[path] = ownerInputFile{}
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() && (entry.Name() == "tests" || entry.Name() == "__pycache__") {
				return filepath.SkipDir
			}
			if len(facts) >= 4096 {
				return errors.New("operation runtime source inventory exceeds its bound")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			fact, err := ownerInputIdentity(child, info)
			facts[child] = fact
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
