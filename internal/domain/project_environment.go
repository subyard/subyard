package domain

import (
	"errors"
	"path/filepath"
	"strings"
)

// ValidateProjectCachePath limits writable sharing to dependency-cache directories.
// Project installs and runtime data remain in the project's private filesystem.
func ValidateProjectCachePath(path string) error {
	if projectControlSocket(path) {
		return errors.New("CACHES exposes a control socket")
	}
	if !projectCleanPath(path) || !strings.HasPrefix(path, "/srv/cache/") || projectPrivatePath(path) {
		return errors.New("CACHES requires clean dependency-cache paths below /srv/cache")
	}
	return nil
}

// ValidateProjectEnvironmentMount accepts read-only toolchain assets. Writable
// dependency caches use CACHES; project and secret mounts are created by the runner.
func ValidateProjectEnvironmentMount(mount string) error {
	if projectControlSocket(mount) {
		return errors.New("ENV_MOUNTS exposes a control socket")
	}
	if projectPrivatePath(mount) {
		return errors.New("ENV_MOUNTS exposes private project, runtime or credential state")
	}
	parts := strings.Split(mount, ":")
	if len(parts) != 3 || parts[2] != "ro" || !projectCleanPath(parts[0]) || !projectCleanPath(parts[1]) {
		return errors.New("ENV_MOUNTS requires clean absolute source:destination:ro declarations")
	}
	source, destination := parts[0], parts[1]
	if !ProjectEnvironmentDependencyPath(source) {
		return errors.New("ENV_MOUNTS source must stay below a toolchain or dependency-cache root")
	}
	for _, reserved := range []string{"/workspace", "/run", "/proc", "/sys", "/dev", "/etc", "/home", "/root", "/srv/env-secrets", "/srv/env-meta"} {
		if destination == reserved || strings.HasPrefix(destination, reserved+"/") || strings.HasPrefix(reserved, destination+"/") {
			return errors.New("ENV_MOUNTS destination overlaps a reserved project-environment path")
		}
	}
	return nil
}

// ProjectEnvironmentDependencyPath also validates a source resolved in the yard:
// a symlink may name an installed JDK, but cannot widen access to private state.
func ProjectEnvironmentDependencyPath(path string) bool {
	if !projectCleanPath(path) || projectControlSocket(path) || projectPrivatePath(path) {
		return false
	}
	for _, root := range []string{"/srv/cache/", "/opt/", "/usr/local/", "/usr/lib/"} {
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	return false
}

func projectCleanPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, ":,\x00\r\n\t ")
}

func projectControlSocket(path string) bool {
	path = strings.ToLower(path)
	return strings.Contains(path, "docker.sock") || strings.Contains(path, "incus.sock") || strings.Contains(path, "lxd.sock")
}

func projectPrivatePath(path string) bool {
	for _, part := range strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == ':' }) {
		switch part {
		case "node_modules", ".venv", "venv", ".docker", "docker", "docker-data", "docker-data-root", "containers", ".claude", ".codex", ".pi", "credentials", "auth.json", "env-secrets", "agent-sessions", "workspaces":
			return true
		}
	}
	return false
}
