package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/ports"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Sources stay with their package. This inventory only binds static delivery bytes;
// provider registration in the guest proves successful capability provisioning.
func (runtime Runtime) agentSkillSources() ([]string, error) {
	if _, err := os.Stat(filepath.Join(runtime.RepositoryRoot, "scripts/refresh-agent-skills.sh")); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	paths := []string{"scripts/refresh-agent-skills.sh"}
	addTree := func(relative string) error {
		return filepath.WalkDir(filepath.Join(runtime.RepositoryRoot, relative), func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() && (entry.Name() == "tests" || entry.Name() == "__pycache__") {
				return filepath.SkipDir
			}
			if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".pyc") {
				relative, err := filepath.Rel(runtime.RepositoryRoot, path)
				if err != nil {
					return err
				}
				paths = append(paths, filepath.ToSlash(relative))
			}
			return nil
		})
	}
	if err := addTree("config/skills"); err != nil {
		return nil, err
	}
	packages, err := filepath.Glob(filepath.Join(runtime.RepositoryRoot, "config/profiles/*"))
	if err != nil {
		return nil, err
	}
	for _, directory := range packages {
		relative, _ := filepath.Rel(runtime.RepositoryRoot, directory)
		if err := addTree(filepath.Join(relative, "skills")); err != nil {
			return nil, err
		}
		manifest := filepath.Join(directory, "skills.json")
		payload, err := os.ReadFile(manifest)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var declarations map[string]map[string]string
		if err := json.Unmarshal(payload, &declarations); err != nil {
			return nil, err
		}
		paths = append(paths, filepath.ToSlash(filepath.Join(relative, "skills.json")))
		for _, files := range declarations {
			for _, source := range files {
				if filepath.IsAbs(source) || source == ".." || strings.HasPrefix(filepath.Clean(source), "../") {
					return nil, fs.ErrInvalid
				}
				paths = append(paths, filepath.ToSlash(filepath.Join(relative, source)))
			}
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func (runtime Runtime) agentSkillsConverged(ctx context.Context, paths []string) (bool, error) {
	files := map[string][]byte{}
	for _, path := range paths {
		payload, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, path))
		if err != nil {
			return false, err
		}
		files[path] = payload
	}
	request, err := json.Marshal(map[string]any{"files": files, "user": runtime.devUser(), "profiles": runtime.environmentValue("ENVIRONMENT_PROFILES"), "coding": runtime.environmentValue("ALLOWS_CODING_TOOLS") != "false"})
	if err != nil {
		return false, err
	}
	program := files["config/skills/probe.py"]
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", string(program)}, Stdin: request})
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 || string(result.Stdout) != "skills-current\n" && string(result.Stdout) != "skills-stale\n" {
		return false, errors.New("agent skill readiness check failed")
	}
	return string(result.Stdout) == "skills-current\n", nil
}
