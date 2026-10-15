package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// Legacy installation discovery uses the executing public runtime plus exact
// entrypoint and migration records. It never treats an unrelated PATH command
// or a similarly named directory as ownership.
func (cli *CLI) discoverLegacyInstallation(current config.Loaded) (uninstall.Installation, error) {
	uid := uint32(os.Getuid())
	installation := uninstall.Installation{Schema: 1, OperatorUID: uid, DataHome: current.Context.Paths.DataHome, ConfigHome: current.Context.Paths.ConfigHome, RuntimeRoot: runtimeRootForRepository(cli.options.RepositoryRoot, filepath.Join(current.Context.Paths.DataHome, "runtime")), CacheRoot: firstNonempty(cli.env["YARD_RELEASE_CACHE"], filepath.Join(current.Context.Paths.DataHome, "releases"))}
	record := filepath.Join(installation.DataHome, "recovery", "pre-go-source")
	read := func(name string) (string, error) {
		path := filepath.Join(record, name)
		tree, err := uninstall.Capture(path)
		if err != nil {
			return "", err
		}
		if len(tree.Entries) == 0 {
			return "", os.ErrNotExist
		}
		if len(tree.Entries) != 1 || tree.Entries[0].Mode&syscall.S_IFMT != syscall.S_IFREG || tree.Entries[0].Mode&0777 != 0600 || tree.Entries[0].UID != uid {
			return "", errors.New("legacy source ownership record is unsafe")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if len(body) > 4096 {
			return "", errors.New("legacy source ownership record is oversized")
		}
		if err = tree.Check(); err != nil {
			return "", err
		}
		value := strings.TrimSuffix(string(body), "\n")
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return "", errors.New("legacy source ownership record has an invalid path")
		}
		return value, nil
	}
	if bin, err := read("bin-dir"); err == nil {
		installation.BinDir = bin
		for name, expected := range map[string]string{"data-home": installation.DataHome, "config-home": installation.ConfigHome} {
			actual, err := read(name)
			if err != nil {
				return installation, err
			}
			if actual != expected {
				return installation, fmt.Errorf("legacy source record %s belongs to another installation", name)
			}
		}
		target, err := read("runtime-launcher")
		if err != nil {
			return installation, err
		}
		suffix := "/current/bin/yard"
		if !strings.HasSuffix(target, suffix) {
			return installation, errors.New("legacy runtime entrypoint has an unknown layout")
		}
		installation.RuntimeRoot = strings.TrimSuffix(target, suffix)
		for _, name := range []string{"rc.path", "login-rc.path"} {
			path, err := read(name)
			if err == nil {
				if !slices.Contains(installation.ShellFiles, path) {
					installation.ShellFiles = append(installation.ShellFiles, path)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return installation, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return installation, err
	} else {
		launcher, err := exec.LookPath(cli.options.Program)
		if err != nil || !filepath.IsAbs(launcher) {
			launcher, err = exec.LookPath("yard")
		}
		if err == nil {
			target, readErr := os.Readlink(launcher)
			if readErr == nil {
				resolved := target
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(filepath.Dir(launcher), resolved)
				}
				if filepath.Clean(resolved) == filepath.Join(cli.options.RepositoryRoot, "bin", "yard") || filepath.Clean(resolved) == filepath.Join(installation.RuntimeRoot, "current", "bin", "yard") {
					installation.BinDir = filepath.Dir(launcher)
				}
			}
		}
		// These operator files are only candidates for exact marked-block edits;
		// their other contents and the source checkout remain preserved.
		for _, name := range []string{".bashrc", ".zshrc", ".profile", ".bash_profile"} {
			installation.ShellFiles = append(installation.ShellFiles, filepath.Join(current.Context.Paths.OperatorHome, name))
		}
	}
	return installation, installation.Validate(installation.DataHome, uid)
}
