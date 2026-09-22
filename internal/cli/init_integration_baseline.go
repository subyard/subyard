package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// Keep the integration inputs of every init plan, including already-canonical
// selections. Adoption is only one possible settings mutation during init.
type initIntegrationBaseline struct {
	Selection config.IntegrationSelection
	Settings  map[string]config.SettingTrace
	Sources   map[string]initIntegrationSource
}

type initIntegrationSource struct {
	Exists        bool
	Digest        [32]byte
	Device, Inode uint64
	Mode          os.FileMode
}

func captureInitIntegrationBaseline(loaded config.Loaded) (*initIntegrationBaseline, error) {
	baseline := &initIntegrationBaseline{Selection: loaded.Integrations, Settings: map[string]config.SettingTrace{}, Sources: map[string]initIntegrationSource{}}
	for name, trace := range loaded.Settings {
		if !initIntegrationSetting(name) {
			continue
		}
		baseline.Settings[name] = trace
		paths := []string{}
		for _, resolution := range trace.Resolutions {
			if resolution.Status != "unset" && filepath.IsAbs(resolution.Path) {
				paths = append(paths, resolution.Path)
			}
		}
		if (trace.Kind == config.SettingFile || trace.Type == config.SettingRegularFilePath) && filepath.IsAbs(trace.EffectiveValue) {
			paths = append(paths, trace.EffectiveValue)
		}
		for _, path := range paths {
			if _, seen := baseline.Sources[path]; seen {
				continue
			}
			source, err := readInitIntegrationSource(path)
			if err != nil {
				return nil, err
			}
			baseline.Sources[path] = source
		}
	}
	return baseline, nil
}

func initIntegrationSetting(name string) bool {
	switch name {
	case "CODING_TOOL_INTEGRATIONS", "ALLOWS_CODING_TOOLS", "YARD_TEMPLATE", "HOST_LINKS", "HOST_CLAUDE_MD", "HOST_CODEX_AGENTS_MD", "HOST_OPENCODE_AGENTS_MD":
		return true
	}
	return strings.HasPrefix(name, "AGENT_") || strings.HasPrefix(name, "CCUSAGE_") || strings.HasPrefix(name, "AI_OBSERVER_")
}

func readInitIntegrationSource(path string) (initIntegrationSource, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return initIntegrationSource{}, nil
	}
	if err != nil {
		return initIntegrationSource{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return initIntegrationSource{}, fmt.Errorf("init integration source must be a bounded regular file: %s", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return initIntegrationSource{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return initIntegrationSource{}, fmt.Errorf("cannot identify init integration source: %s", path)
	}
	return initIntegrationSource{Exists: true, Digest: sha256.Sum256(content), Device: uint64(stat.Dev), Inode: stat.Ino, Mode: info.Mode()}, nil
}

func (execution *initExecution) checkIntegrationBaseline(cli *CLI) error {
	if execution.integrationBaseline == nil {
		return nil
	}
	options := config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: execution.loaded.Context.Paths.OperatorHome, YardName: execution.loaded.Context.YardName, Environment: cli.baseEnv}
	if execution.bootstrap != nil {
		options.YardSettingsFile = execution.bootstrap.sourcePath
	}
	loaded, err := config.Load(options)
	if err != nil {
		return err
	}
	current, err := captureInitIntegrationBaseline(loaded)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, execution.integrationBaseline) {
		return fmt.Errorf("%w: init integration configuration changed after planning", domain.ErrPlanStale)
	}
	return nil
}

// A composed resource bootstrap may publish ENVIRONMENT_PROFILES before init.
// Accept only the exact bytes from that already-authorized CAS, keeping all
// other source and integration-setting observations unchanged.
func (baseline *initIntegrationBaseline) acceptPublishedSource(path string, content []byte) error {
	if baseline == nil {
		return nil
	}
	if _, tracked := baseline.Sources[path]; !tracked {
		return nil
	}
	observed, err := readInitIntegrationSource(path)
	if err != nil {
		return err
	}
	if !observed.Exists || observed.Digest != sha256.Sum256(content) {
		return fmt.Errorf("%w: composed bootstrap settings changed", domain.ErrPlanStale)
	}
	baseline.Sources[path] = observed
	return nil
}
