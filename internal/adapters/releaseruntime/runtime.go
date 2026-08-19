package releaseruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
)

type Config struct {
	Environment map[string]string
	Installer   string
	Stdout      io.Writer
	Stderr      io.Writer
	HTTPClient  *http.Client
}

type Prepared struct {
	Action         domain.ActionID
	Changed        bool
	Consequences   []string
	RefreshConfigs bool
	ActiveLauncher string
	run            func(context.Context) error
}

func (prepared Prepared) Execute(ctx context.Context) error {
	if prepared.run == nil {
		return errors.New("release operation was not prepared")
	}
	return prepared.run(ctx)
}

type Runtime struct{ config Config }

func New(config Config) *Runtime {
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	return &Runtime{config: config}
}

type options struct {
	channel, version, root, cache, repository, baseURL, tag string
	offline, check, rollback, force                         bool
}

func (runtime *Runtime) Prepare(ctx context.Context, arguments []string) (Prepared, error) {
	options, help, err := runtime.parse(arguments)
	if err != nil {
		return Prepared{}, err
	}
	if help {
		return Prepared{Action: "update.help", run: func(context.Context) error {
			fmt.Fprintln(runtime.config.Stdout, "Usage: yard update [--check] [--version VERSION] [--offline] [--rollback] [--force]")
			return nil
		}}, nil
	}
	if options.rollback {
		expected, err := runtime.inspectRuntimeLinks(ctx, options.root, true)
		if err != nil {
			return Prepared{}, err
		}
		return Prepared{Action: "update.rollback", Changed: true, Consequences: []string{
			"verify and reactivate the previous immutable runtime",
			"refresh materialized agent configuration",
		}, run: func(ctx context.Context) error {
			if err := requirePreparedReleaseRoots(options, false); err != nil {
				return err
			}
			if err := runtime.requireRuntimeLinks(ctx, options.root, expected, true); err != nil {
				return err
			}
			return runtime.install(ctx, "--runtime-root", options.root, "--rollback")
		}, RefreshConfigs: true, ActiveLauncher: filepath.Join(options.root, "current", "bin", "yard")}, nil
	}
	if options.version == "" {
		if options.offline {
			return Prepared{}, errors.New("offline mode requires --version")
		}
		options.tag, err = runtime.latestTag(ctx, options.repository)
		if err != nil {
			return Prepared{}, err
		}
		options.version = strings.TrimPrefix(options.tag, "v")
	} else if options.tag == "" {
		options.tag = "v" + options.version
	}
	if !safeVersion(options.version) {
		return Prepared{}, fmt.Errorf("unsafe version %q", options.version)
	}
	consequences := []string{
		fmt.Sprintf("download and verify runtime %s for %s/%s", options.version, goruntime.GOOS, goruntime.GOARCH),
		map[bool]string{true: "verify compatibility without activation", false: "atomically activate it and retain the previous runtime"}[options.check],
	}
	if !options.check {
		consequences = append(
			consequences,
			"apply every required ordered config and lifecycle migration",
			"refresh materialized agent configuration",
		)
	}
	action := domain.ActionID("update.activate")
	if options.check {
		action = "update.check"
	}
	var expected runtimeLinkSnapshot
	if !options.check {
		expected, err = runtime.inspectRuntimeLinks(ctx, options.root, false)
		if err != nil {
			return Prepared{}, err
		}
	}
	return Prepared{
		Action:         action,
		Changed:        true,
		Consequences:   consequences,
		RefreshConfigs: !options.check,
		ActiveLauncher: filepath.Join(options.root, "current", "bin", "yard"),
		run: func(ctx context.Context) error {
			if err := requirePreparedReleaseRoots(options, true); err != nil {
				return err
			}
			if !options.check {
				if err := runtime.requireRuntimeLinks(ctx, options.root, expected, false); err != nil {
					return err
				}
			}
			return runtime.execute(ctx, options)
		},
	}, nil
}

type runtimeLinkState struct {
	present bool
	target  string
}

type runtimeLinkSnapshot struct {
	current  runtimeLinkState
	previous runtimeLinkState
}

func (runtime *Runtime) inspectRuntimeLinks(
	ctx context.Context,
	root string,
	requireRollback bool,
) (runtimeLinkSnapshot, error) {
	current, err := inspectRuntimeLink(root, "current")
	if err != nil {
		return runtimeLinkSnapshot{}, err
	}
	previous, err := inspectRuntimeLink(root, "previous")
	if err != nil {
		return runtimeLinkSnapshot{}, err
	}
	snapshot := runtimeLinkSnapshot{current: current, previous: previous}
	if !requireRollback {
		return snapshot, nil
	}
	if !current.present || !previous.present {
		return runtimeLinkSnapshot{}, errors.New("valid current and previous runtimes are required for rollback")
	}
	for _, state := range []runtimeLinkState{current, previous} {
		engine := filepath.Join(root, state.target, "bin", "yard-engine")
		info, err := os.Lstat(engine)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
			return runtimeLinkSnapshot{}, fmt.Errorf("runtime %q has no executable yard-engine", state.target)
		}
	}
	previousEngine := filepath.Join(root, previous.target, "bin", "yard-engine")
	command := exec.CommandContext(ctx, previousEngine, "--version")
	command.Env = []string{"HOME=" + filepath.Dir(filepath.Dir(previousEngine))}
	if err := command.Run(); err != nil {
		return runtimeLinkSnapshot{}, fmt.Errorf("previous runtime self-check failed: %w", err)
	}
	return snapshot, nil
}

func (runtime *Runtime) requireRuntimeLinks(
	ctx context.Context,
	root string,
	expected runtimeLinkSnapshot,
	requireRollback bool,
) error {
	actual, err := runtime.inspectRuntimeLinks(ctx, root, requireRollback)
	if err != nil || actual != expected {
		if err != nil {
			return fmt.Errorf("%w: runtime links changed: %v", domain.ErrPlanStale, err)
		}
		return fmt.Errorf("%w: runtime links changed", domain.ErrPlanStale)
	}
	return nil
}

func inspectRuntimeLink(root, name string) (runtimeLinkState, error) {
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return runtimeLinkState{}, nil
	}
	if err != nil {
		return runtimeLinkState{}, fmt.Errorf("inspect runtime %s link: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return runtimeLinkState{}, fmt.Errorf("runtime %s must be a symbolic link", name)
	}
	target, err := os.Readlink(path)
	if err != nil {
		return runtimeLinkState{}, fmt.Errorf("read runtime %s link: %w", name, err)
	}
	clean := filepath.Clean(target)
	relative := strings.TrimPrefix(target, "releases/")
	if clean != target || filepath.IsAbs(target) || !strings.HasPrefix(target, "releases/") ||
		relative == "" || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return runtimeLinkState{}, fmt.Errorf("runtime %s link has an unsafe target", name)
	}
	destinationInfo, err := os.Stat(filepath.Join(root, target))
	if err != nil || !destinationInfo.IsDir() {
		return runtimeLinkState{}, fmt.Errorf("runtime %s link target is unavailable", name)
	}
	return runtimeLinkState{present: true, target: target}, nil
}

func (runtime *Runtime) parse(arguments []string) (options, bool, error) {
	home := runtime.config.Environment["SUBYARD_HOME"]
	if home == "" {
		home = filepath.Join(runtime.config.Environment["HOME"], ".subyard")
	}
	result := options{channel: "stable", root: filepath.Join(home, "runtime"), cache: filepath.Join(home, "releases"),
		repository: first(runtime.config.Environment["YARD_RELEASE_REPOSITORY"], "Subyard/Subyard"),
		version:    runtime.config.Environment["YARD_RELEASE_VERSION"], baseURL: runtime.config.Environment["YARD_RELEASE_BASE_URL"],
		tag: runtime.config.Environment["YARD_RELEASE_TAG"]}
	if value := runtime.config.Environment["YARD_RUNTIME_ROOT"]; value != "" {
		result.root = value
	}
	if value := runtime.config.Environment["YARD_RELEASE_CACHE"]; value != "" {
		result.cache = value
	}
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--channel", "--version", "--runtime-root":
			index++
			if index >= len(arguments) {
				return result, false, fmt.Errorf("%s needs a value", arguments[index-1])
			}
			switch arguments[index-1] {
			case "--channel":
				result.channel = arguments[index]
			case "--version":
				result.version = arguments[index]
			case "--runtime-root":
				result.root = arguments[index]
			}
		case "--offline":
			result.offline = true
		case "--check":
			result.check = true
		case "--rollback":
			result.rollback = true
		case "--force":
			result.force = true
		case "-y", "--yes":
		case "-h", "--help":
			return result, true, nil
		default:
			return result, false, fmt.Errorf("unknown option %q", arguments[index])
		}
	}
	if result.channel != "stable" {
		return result, false, fmt.Errorf("unsupported channel %q", result.channel)
	}
	if result.rollback && (result.offline || result.check || result.force || result.version != "") {
		return result, false, errors.New("--rollback cannot be combined with update options")
	}
	validatedRoot, err := validateReleaseRoot(result.root, "runtime root")
	if err != nil {
		return result, false, err
	}
	validatedCache, err := validateReleaseRoot(result.cache, "release cache")
	if err != nil {
		return result, false, err
	}
	result.root = validatedRoot
	result.cache = validatedCache
	return result, false, nil
}

func validateReleaseRoot(path, name string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || !filepath.IsAbs(path) || clean == string(filepath.Separator) {
		return "", fmt.Errorf("%s must be an absolute non-root path", name)
	}
	current := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	operatorUID := uint32(os.Getuid())
	privateAncestor := false
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return clean, nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect %s path %s: %w", name, current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s path has a symlink or non-directory ancestor: %s", name, current)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 && stat.Uid != operatorUID {
			return "", fmt.Errorf("%s path has unsafe ownership: %s", name, current)
		}
		final := index == len(components)-1
		stickySystemAncestor := stat.Uid == 0 && info.Mode()&os.ModeSticky != 0 && !final
		containedOperatorAncestor := stat.Uid == operatorUID && privateAncestor && !final
		if info.Mode().Perm()&0o022 != 0 && !stickySystemAncestor && !containedOperatorAncestor {
			return "", fmt.Errorf("%s path has unsafe permissions: %s", name, current)
		}
		if stat.Uid == operatorUID && info.Mode().Perm()&0o077 == 0 {
			privateAncestor = true
		}
		if final && stat.Uid != operatorUID {
			return "", fmt.Errorf("%s is not operator-owned: %s", name, current)
		}
	}
	return clean, nil
}

func requirePreparedReleaseRoots(options options, requireCache bool) error {
	for _, root := range []struct {
		path, name string
	}{
		{path: options.root, name: "runtime root"},
		{path: options.cache, name: "release cache"},
	} {
		if root.name == "release cache" && !requireCache {
			continue
		}
		validated, err := validateReleaseRoot(root.path, root.name)
		if err != nil || validated != root.path {
			if err != nil {
				return fmt.Errorf("%w: %s changed after planning: %v", domain.ErrPlanStale, root.name, err)
			}
			return fmt.Errorf("%w: %s changed after planning", domain.ErrPlanStale, root.name)
		}
	}
	return nil
}

func (runtime *Runtime) execute(ctx context.Context, options options) error {
	osName, arch := goruntime.GOOS, goruntime.GOARCH
	if osName != "linux" || arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported platform %s/%s", osName, arch)
	}
	name := fmt.Sprintf("subyard-%s-%s-%s.tar.gz", options.version, osName, arch)
	directory := filepath.Join(options.cache, options.version)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	paths := make([]string, 4)
	for index, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		paths[index] = filepath.Join(directory, name+suffix)
		if err := runtime.fetch(ctx, options, name+suffix, paths[index]); err != nil {
			return fmt.Errorf("release download failed; current runtime was not changed: %w", err)
		}
	}
	current := "none"
	engine := filepath.Join(options.root, "current", "bin", "yard-engine")
	if output, err := exec.CommandContext(ctx, engine, "--version").Output(); err == nil {
		fields := strings.Fields(string(output))
		if len(fields) > 1 {
			current = fields[1]
		}
	}
	fmt.Fprintf(runtime.config.Stdout, "channel=%s current=%s available=%s platform=%s/%s\n", options.channel, current, options.version, osName, arch)
	arguments := []string{"--runtime-root", options.root, "--bundle", paths[0], "--checksum", paths[1], "--manifest", paths[2], "--provenance", paths[3]}
	if options.check {
		arguments = append(arguments, "--check")
	} else if !options.force && current == options.version {
		fmt.Fprintln(runtime.config.Stdout, "runtime is already current; checking migrations")
	}
	return runtime.install(ctx, arguments...)
}

func (runtime *Runtime) fetch(ctx context.Context, options options, name, destination string) error {
	if options.offline {
		info, err := os.Lstat(destination)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("cached release asset is unavailable")
		}
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".download-*")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	var reader io.ReadCloser
	if strings.HasPrefix(options.baseURL, "file://") {
		reader, err = os.Open(filepath.Join(strings.TrimPrefix(options.baseURL, "file://"), name))
	} else {
		url := options.baseURL
		if url == "" {
			url = fmt.Sprintf("https://github.com/%s/releases/download/%s", options.repository, options.tag)
		}
		if !strings.HasPrefix(url, "https://") {
			temporary.Close()
			return errors.New("release base URL must use https:// or file://")
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(url, "/")+"/"+name, nil)
		if requestErr != nil {
			temporary.Close()
			return requestErr
		}
		response, requestErr := runtime.config.HTTPClient.Do(request)
		if requestErr != nil {
			temporary.Close()
			return requestErr
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			temporary.Close()
			return fmt.Errorf("download returned %s", response.Status)
		}
		reader = response.Body
	}
	if err != nil {
		temporary.Close()
		return err
	}
	_, copyErr := io.Copy(temporary, reader)
	closeReaderErr, closeFileErr := reader.Close(), temporary.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeReaderErr != nil {
		return closeReaderErr
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	return os.Rename(path, destination)
}

func (runtime *Runtime) latestTag(ctx context.Context, repository string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := runtime.config.HTTPClient.Do(request)
	if err != nil {
		return "", errors.New("could not resolve the stable release")
	}
	defer response.Body.Close()
	var payload struct {
		Tag string `json:"tag_name"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload) != nil || payload.Tag == "" {
		return "", errors.New("latest release has no valid tag")
	}
	return payload.Tag, nil
}

func (runtime *Runtime) install(ctx context.Context, arguments ...string) error {
	command := exec.CommandContext(ctx, runtime.config.Installer, arguments...)
	command.Env = commandEnvironment(runtime.config.Environment)
	command.Stdout = runtime.config.Stdout
	command.Stderr = runtime.config.Stderr
	return command.Run()
}

func commandEnvironment(values map[string]string) []string {
	if values == nil {
		return os.Environ()
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func safeVersion(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character != '.' && character != '_' && character != '+' && character != '-' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}

func first(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
