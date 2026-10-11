package githubbroker

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func repositoryName(value string) (string, error) {
	value = strings.TrimPrefix(value, "github.com/")
	if strings.HasPrefix(value, "git@github.com:") {
		value = strings.TrimPrefix(value, "git@github.com:")
	} else if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "ssh") {
			return "", errors.New("repository must belong to github.com")
		}
		value = strings.TrimPrefix(parsed.Path, "/")
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git")
	if !validRepository(value) {
		return "", errors.New("invalid GitHub repository; use owner/repo")
	}
	return value, nil
}

// Recognize only unambiguous native gh selectors. Arbitrary child programs use
// the wrapper's --repo option or the configured default installation.
func commandRepository(ctx context.Context, command []string, selected string) (string, error) {
	if filepath.Base(command[0]) != "gh" {
		return selected, nil
	}
	if host := os.Getenv("GH_HOST"); host != "" && host != "github.com" {
		return "", errors.New("GitHub App wrapper only supports github.com")
	}
	add := func(value string) error {
		repository, err := repositoryName(value)
		if err != nil {
			return err
		}
		if selected != "" && !strings.EqualFold(selected, repository) {
			return errors.New("conflicting GitHub repositories; run separate commands for each repository")
		}
		selected = repository
		return nil
	}
	for index := 1; index < len(command); index++ {
		arg := command[index]
		if arg == "--hostname" || strings.HasPrefix(arg, "--hostname=") {
			host := strings.TrimPrefix(arg, "--hostname=")
			if arg == "--hostname" {
				index++
				if index >= len(command) {
					return "", errors.New("missing GitHub hostname")
				}
				host = command[index]
			}
			if host != "github.com" {
				return "", errors.New("GitHub App wrapper only supports github.com")
			}
		} else if arg == "-R" || arg == "--repo" {
			index++
			if index >= len(command) {
				return "", errors.New("missing GitHub repository")
			}
			if err := add(command[index]); err != nil {
				return "", err
			}
		} else if strings.HasPrefix(arg, "--repo=") || strings.HasPrefix(arg, "-R") {
			value := strings.TrimPrefix(arg, "--repo=")
			if strings.HasPrefix(arg, "-R") {
				value = strings.TrimPrefix(arg, "-R")
			}
			if err := add(value); err != nil {
				return "", err
			}
		}
	}
	if len(command) > 3 && command[1] == "repo" && (command[2] == "view" || command[2] == "clone") {
		for index := 3; index < len(command); index++ {
			arg := command[index]
			if arg == "--" {
				if index+1 < len(command) {
					if err := add(command[index+1]); err != nil {
						return "", err
					}
				}
				break
			}
			if !strings.HasPrefix(arg, "-") {
				if err := add(arg); err != nil {
					return "", err
				}
				break
			}
			flag, _, hasValue := strings.Cut(arg, "=")
			if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
				short := arg[:2]
				if (command[2] == "view" && strings.Contains("bqt", arg[1:2])) || (command[2] == "clone" && short == "-u") {
					flag, hasValue = short, true
				}
			}
			valueFlag := false
			booleanFlag := false
			if command[2] == "view" {
				switch flag {
				case "--branch", "-b", "--json", "--jq", "-q", "--template", "-t":
					valueFlag = true
				case "--web", "-w", "--help":
					booleanFlag = true
				}
			} else {
				switch flag {
				case "--upstream-remote-name", "-u":
					valueFlag = true
				case "--no-upstream", "--help":
					booleanFlag = true
				}
			}
			if valueFlag && !hasValue {
				index++
				if index >= len(command) {
					return "", errors.New("missing GitHub repository command flag value")
				}
			} else if !valueFlag && !booleanFlag {
				if selected == "" {
					return "", errors.New("ambiguous GitHub repository command flags; use run --repo owner/repo")
				}
				break
			}
		}
	}
	if len(command) > 2 && command[1] == "api" {
		for _, arg := range command[2:] {
			path := strings.TrimPrefix(strings.TrimPrefix(arg, "https://api.github.com/"), "/")
			if strings.HasPrefix(path, "repos/") {
				parts := strings.Split(path, "/")
				if len(parts) < 3 {
					return "", errors.New("invalid GitHub repository API endpoint")
				}
				if parts[1] == "{owner}" && parts[2] == "{repo}" {
					continue // Native gh expands these from the selected GH_REPO.
				}
				if err := add(parts[1] + "/" + strings.SplitN(parts[2], "?", 2)[0]); err != nil {
					return "", err
				}
			}
		}
	}
	if selected != "" {
		return selected, nil
	}
	if repository := os.Getenv("GH_REPO"); repository != "" {
		return repositoryName(repository)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// Git config is a local read; never fetch, rewrite the origin or use SSH.
	probe := exec.CommandContext(probeCtx, "git", "config", "--get", "remote.origin.url")
	pipe, err := probe.StdoutPipe()
	if err != nil || probe.Start() != nil {
		return "", nil
	}
	output, readErr := io.ReadAll(io.LimitReader(pipe, maxProtocolInput+1))
	_ = pipe.Close()
	err = probe.Wait()
	if len(output) > maxProtocolInput || readErr != nil || probeCtx.Err() != nil {
		return "", errors.New("cannot read GitHub repository context; use --repo owner/repo")
	}
	if err == nil && strings.TrimSpace(string(output)) != "" {
		return repositoryName(strings.TrimSpace(string(output)))
	}
	return "", nil
}
