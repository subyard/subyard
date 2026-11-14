package reconcileruntime

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// hostInstallerSourcePaths retains the existing declaration reader's complete
// public source scope. Its sealed engine still runs RequireReader against the
// actual protected ledger; no captured verdict replaces that check.
func (runtime Runtime) hostInstallerSourcePaths(owner string) (string, []string, error) {
	var handler string
	switch owner {
	case "host-power":
		handler = "install-power-reconciler.sh"
	case "test-vms-host-sink":
		handler = "install-test-vms-host-sink.sh"
	case "materialized-integrations":
		handler = "reconcile-integrations.sh"
	default:
		return "", nil, errors.New("unknown host installer source recipe")
	}
	paths := []string{}
	for _, prefix := range []string{"config", "scripts"} {
		root := filepath.Join(runtime.RepositoryRoot, prefix)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "tests" || entry.Name() == ".git" || entry.Name() == "__pycache__" || entry.Name() == "private" || entry.Name() == ".build" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".pyc") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("host installer source must be regular")
			}
			relative, err := filepath.Rel(runtime.RepositoryRoot, path)
			if err != nil {
				return err
			}
			if len(relative) > 512 {
				return errors.New("host installer path bound")
			}
			paths = append(paths, relative)
			if len(paths) > 4096 {
				return errors.New("host installer file bound")
			}
			return nil
		})
		if err != nil {
			return "", nil, err
		}
	}
	slices.Sort(paths)
	if !slices.Contains(paths, filepath.Join("scripts", handler)) {
		return "", nil, errors.New("host installer source unavailable")
	}
	return handler, paths, nil
}
