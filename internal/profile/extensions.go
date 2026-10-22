package profile

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Setting is a profile-owned scalar configuration declaration. It deliberately
// mirrors the public catalog fields without importing the config package.
type Setting struct {
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	Scopes       []string     `json:"scopes"`
	Application  string       `json:"application"`
	Optional     bool         `json:"optional,omitempty"`
	Minimum      int          `json:"minimum,omitempty"`
	Maximum      int          `json:"maximum,omitempty"`
	Default      *string      `json:"default,omitempty"`
	DefaultFrom  *PortDefault `json:"default_from,omitempty"`
	Syncable     bool         `json:"syncable,omitempty"`
	HostListener bool         `json:"host_listener,omitempty"`
	Enum         []string     `json:"enum,omitempty"`
}

type RuntimeHook struct {
	ActivationID         string   `json:"activation_id"`
	Handler              string   `json:"handler"`
	Custody              bool     `json:"custody,omitempty"`
	ProjectsChangedHooks []string `json:"projects_changed_hooks,omitempty"`
}

type GuestEnvironmentHook struct {
	Handler string `json:"handler"`
}

func (definition *Definition) validateExecutable(path string) error {
	resolved, err := definition.ExecutablePath(path)
	if err == nil {
		definition.validationInputs = append(definition.validationInputs, resolved)
	}
	return err
}

// ExecutablePath revalidates a profile hook and keeps updater-pinned directory
// descriptors intact. Ordinary roots and all descendants must remain real paths.
func (definition Definition) ExecutablePath(path string) (string, error) {
	return definition.sourcePath(path, true)
}

func (definition Definition) sourcePath(path string, executable bool) (string, error) {
	if definition.Root == "" {
		return "", errors.New("profile root is unavailable")
	}
	if !relative(path) {
		return "", errors.New("handler path escapes profile")
	}
	root, err := packageDirectory(definition.Root)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	current := root
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("handler path must not traverse symlinks")
		}
		if current != filepath.Join(root, path) && !info.IsDir() {
			return "", errors.New("handler parent must be a directory")
		}
	}
	info, err := os.Lstat(current)
	resolved, resolveErr := filepath.EvalSymlinks(current)
	if err != nil || resolveErr != nil || !info.Mode().IsRegular() || (executable && info.Mode().Perm()&0o111 == 0) || resolved != filepath.Join(resolvedRoot, path) {
		return "", errors.New("handler must be an executable regular file within profile root")
	}
	return current, nil
}

var profileDescriptorRoot = regexp.MustCompile(`^/proc/[1-9][0-9]*/fd/(0|[1-9][0-9]*)/`)

var pinnedRepositoryRoot = regexp.MustCompile(`^/proc/(self|[1-9][0-9]*)/fd/(0|[1-9][0-9]*)$`)

// ReadExecutable shares the runtime hook's validation before reading guest hooks.
func (definition Definition) ReadExecutable(path string) ([]byte, error) {
	current, err := definition.ExecutablePath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(current)
}
