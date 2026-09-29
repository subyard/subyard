package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// Setting is a profile-owned scalar configuration declaration. It deliberately
// mirrors the public catalog fields without importing the config package.
type Setting struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Scopes       []string `json:"scopes"`
	Application  string   `json:"application"`
	Optional     bool     `json:"optional,omitempty"`
	Minimum      int      `json:"minimum,omitempty"`
	Maximum      int      `json:"maximum,omitempty"`
	Default      *string  `json:"default,omitempty"`
	Syncable     bool     `json:"syncable,omitempty"`
	HostListener bool     `json:"host_listener,omitempty"`
	Enum         []string `json:"enum,omitempty"`
}

type RuntimeHook struct {
	ActivationID string `json:"activation_id"`
	Handler      string `json:"handler"`
}

type GuestEnvironmentHook struct {
	Handler string `json:"handler"`
}

var settingNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

var allowedScopes = map[string]bool{
	"shipped": true, "shared": true, "host": true, "yard": true, "command": true,
}

func (definition Definition) validateExtensions() error {
	seen := map[string]bool{}
	for _, setting := range definition.Settings {
		if !settingNamePattern.MatchString(setting.Name) || seen[setting.Name] {
			return errors.New("invalid or duplicate profile setting name")
		}
		seen[setting.Name] = true
		if setting.Application != "next-command" && setting.Application != "yard-init" && setting.Application != "config-apply" {
			return errors.New("invalid profile setting application")
		}
		if len(setting.Scopes) == 0 {
			return errors.New("profile setting requires scopes")
		}
		scopes := map[string]bool{}
		for _, scope := range setting.Scopes {
			if !allowedScopes[scope] || scopes[scope] {
				return errors.New("invalid or duplicate profile setting scope")
			}
			scopes[scope] = true
		}
		if setting.Type != "string" && setting.Type != "boolean" && setting.Type != "integer" && setting.Type != "port" && setting.Type != "size" && setting.Type != "name" {
			return errors.New("unsupported profile setting type")
		}
		if len(setting.Enum) != 0 {
			enums := map[string]bool{}
			for _, value := range setting.Enum {
				if value == "" || !textSafe(value) || enums[value] {
					return errors.New("invalid or duplicate profile setting enum value")
				}
				enums[value] = true
			}
		}
		if setting.Minimum < 0 || setting.Maximum < 0 || setting.Minimum > 0 && setting.Maximum > 0 && setting.Minimum > setting.Maximum {
			return errors.New("invalid profile setting bounds")
		}
		if (setting.Minimum != 0 || setting.Maximum != 0) && setting.Type != "integer" && setting.Type != "port" {
			return errors.New("bounds require numeric profile setting type")
		}
		if setting.Type == "port" && (setting.Minimum < 1 || setting.Maximum < 1 || setting.Maximum > 65535 || setting.Minimum > setting.Maximum) {
			return errors.New("port profile setting requires bounds within 1..65535")
		}
		if setting.HostListener && setting.Type != "port" {
			return errors.New("host listener profile setting must be a port")
		}
		if setting.Default != nil {
			if err := setting.validateDefault(*setting.Default); err != nil {
				return err
			}
		}
	}
	if definition.Runtime != nil {
		if !safeIdentifier(definition.Runtime.ActivationID) {
			return errors.New("invalid runtime activation ID")
		}
		if err := definition.validateExecutable(definition.Runtime.Handler); err != nil {
			return fmt.Errorf("runtime handler: %w", err)
		}
	}
	if definition.GuestEnvironment != nil {
		if err := definition.validateExecutable(definition.GuestEnvironment.Handler); err != nil {
			return fmt.Errorf("guest environment handler: %w", err)
		}
	}
	return nil
}

func safeIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return false
		}
	}
	return value[0] >= 'a' && value[0] <= 'z'
}

func (setting Setting) validateDefault(value string) error {
	if value == "" && setting.Optional {
		return nil
	}
	if !textSafe(value) {
		return errors.New("invalid profile setting default")
	}
	valid := true
	switch setting.Type {
	case "boolean":
		valid = value == "0" || value == "1"
	case "integer", "port":
		n, err := strconv.Atoi(value)
		valid = err == nil && n >= 0
		if valid && setting.Minimum > 0 {
			valid = n >= setting.Minimum
		}
		if valid && setting.Maximum > 0 {
			valid = n <= setting.Maximum
		}
		if valid && setting.Type == "port" {
			valid = n >= 1 && n <= 65535
		}
	case "size":
		raw := strings.TrimSuffix(strings.TrimSuffix(value, "MiB"), "GiB")
		n, err := strconv.Atoi(raw)
		valid = raw != value && err == nil && n > 0
	case "name":
		valid = domain.SafeName(value)
	}
	if len(setting.Enum) != 0 {
		match := false
		for _, candidate := range setting.Enum {
			match = match || value == candidate
		}
		valid = valid && match
	}
	if !valid {
		return errors.New("profile setting default violates its type or bounds")
	}
	return nil
}

func (definition Definition) validateExecutable(path string) error {
	if !relative(path) {
		return errors.New("handler path escapes profile")
	}
	root, err := filepath.EvalSymlinks(definition.Root)
	if err != nil {
		return errors.New("profile root cannot be resolved")
	}
	current := definition.Root
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("handler path must not traverse symlinks")
		}
		if current != filepath.Join(definition.Root, path) && !info.IsDir() {
			return errors.New("handler parent must be a directory")
		}
	}
	info, err := os.Lstat(current)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("handler must be an executable regular file")
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil || resolved != filepath.Join(root, path) {
		return errors.New("handler must remain within profile root")
	}
	return nil
}

// ExecutablePath revalidates a profile hook and keeps updater-pinned directory
// descriptors intact. Ordinary roots and all descendants must remain real paths.
func (definition Definition) ExecutablePath(path string) (string, error) {
	if definition.Root == "" {
		return "", errors.New("profile root is unavailable")
	}
	if !relative(path) {
		return "", errors.New("handler path escapes profile")
	}
	root, err := filepath.Abs(definition.Root)
	if err != nil {
		return "", errors.New("profile root cannot be resolved")
	}
	// Child hook processes do not inherit the engine's descriptor table.
	if strings.HasPrefix(root, "/proc/self/fd/") {
		root = fmt.Sprintf("/proc/%d/fd/%s", os.Getpid(), strings.TrimPrefix(root, "/proc/self/fd/"))
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	rootInfo, rootErr := os.Lstat(root)
	if err != nil || rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("profile root must be a real directory")
	}
	if resolvedRoot != root {
		// Only the kernel directory-descriptor anchor may be a symlink. Check
		// every component below it, including the profile package's ancestors.
		anchor := strings.TrimSuffix(profileDescriptorRoot.FindString(root), "/")
		if anchor == "" {
			return "", errors.New("profile root must be a real directory")
		}
		info, err := os.Stat(anchor)
		if err != nil || !info.IsDir() {
			return "", errors.New("profile directory descriptor is unavailable")
		}
		current := anchor
		for _, part := range strings.Split(strings.TrimPrefix(root, anchor+"/"), string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", errors.New("profile root must not traverse symlinks")
			}
		}
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
	if err != nil || resolveErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || resolved != filepath.Join(resolvedRoot, path) {
		return "", errors.New("handler must be an executable regular file within profile root")
	}
	return current, nil
}

var profileDescriptorRoot = regexp.MustCompile(`^/proc/[1-9][0-9]*/fd/(0|[1-9][0-9]*)/`)

// ReadExecutable shares the runtime hook's validation before reading guest hooks.
func (definition Definition) ReadExecutable(path string) ([]byte, error) {
	current, err := definition.ExecutablePath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(current)
}
