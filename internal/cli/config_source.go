package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
)

const sourceStagePrefix = ".subyard-config-source-"
const sourceInitCandidatePrefix = ".subyard-config-source-init-"

type configSourceConnectOptions struct {
	origin      string
	checkout    string
	hostID      string
	initialize  bool
	materialize bool
}

type preparedConfigSource struct {
	origin           string
	checkout         string
	stage            string
	existingAncestor string
	preview          configsync.Plan
	options          configsync.Options
	needsClone       bool
	needsRegister    bool
	initialPush      bool
	initialBranch    string
	initialCommit    string
	initialCandidate string
	candidateParent  string
	targetBranch     string
}

func (cli *CLI) runConfigSyncConnect(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
	assumeYes bool,
) int {
	if loaded.Context.YardType == domain.YardRemote {
		forwarded := append([]string{"sync", "connect"}, arguments...)
		if assumeYes {
			forwarded = append(forwarded, "--yes")
		}
		return cli.forwardRemote(ctx, loaded.Context, "config", forwarded)
	}
	connect, parsedYes, err := cli.parseConfigSourceConnect(arguments)
	if err != nil {
		cli.errorf("config sync connect: %v", err)
		return 2
	}
	assumeYes = assumeYes || parsedYes
	prepared, err := cli.prepareConfigSource(ctx, loaded, connect)
	if err != nil {
		cli.errorf("config sync connect: %v", err)
		return 1
	}
	defer prepared.cleanup()

	fmt.Fprintln(cli.options.Stdout, "Configuration source onboarding")
	fmt.Fprintf(cli.options.Stdout, "  checkout: %s\n", prepared.checkout)
	writeConfigSyncPlan(cli.options.Stdout, prepared.preview)
	changed := prepared.needsClone || prepared.needsRegister || prepared.initialPush ||
		prepared.preview.NeedsApply()
	consequences := make([]string, 0, len(prepared.preview.Changes)+3)
	if changed && prepared.needsClone {
		consequences = append(consequences,
			"install the prepared private Git checkout at "+prepared.checkout)
	}
	if changed && prepared.needsRegister {
		consequences = append(consequences,
			"register the owner-host configuration source checkout")
	}
	if changed && prepared.initialPush {
		consequences = append(consequences,
			"create and push the initial configuration commit without force")
	}
	if changed && prepared.initialCandidate != "" {
		consequences = append(consequences,
			"initialize the existing configuration source checkout from the prepared commit")
	}
	if changed && prepared.preview.InitializeHostID {
		consequences = append(consequences,
			"record owner host ID "+prepared.preview.HostID)
	}
	if changed {
		for _, change := range prepared.preview.Changes {
			consequences = append(consequences, change.Action+" "+change.Path)
		}
		if prepared.preview.ManifestChanged {
			consequences = append(consequences,
				"update versioned configuration manifest metadata")
		}
	}
	if changed && connect.materialize && configSyncPlanNeedsMaterialization(prepared.preview) {
		consequences = append(consequences,
			"refresh affected file settings in running local yards")
	}
	orchestrator, operation, err := cli.planConfigSyncOperation(
		ctx, loaded, "config sync connect", "config.sync.connect", changed,
		consequences, assumeYes,
	)
	if errors.Is(err, application.ErrDeclined) {
		cli.errorf("config sync connect: operation declined")
		return 1
	}
	if err != nil {
		cli.errorf("config sync connect: %v", err)
		return 1
	}
	if !changed {
		fmt.Fprintln(cli.options.Stdout, "config sync: already connected and converged")
		return 0
	}
	adapter := &configSourceConnectAdapter{cli: cli, prepared: prepared}
	orchestrator.Runner = adapter
	if _, _, err := orchestrator.RunAdapter(ctx, operation, domain.AdapterRequest{
		OperationID: operation.OperationID,
		Adapter:     "config-source",
		Action:      "connect",
	}, nil); err != nil {
		cli.errorf("config sync connect: %v", err)
		return 1
	}
	fmt.Fprintf(cli.options.Stdout, "config sync: connected %s\n", prepared.checkout)
	if adapter.plan.NeedsApply() {
		fmt.Fprintf(cli.options.Stdout, "config sync: applied generation %d\n",
			adapter.plan.Generation)
		if connect.materialize {
			if err := cli.materializeConfigSyncPlan(ctx, loaded, adapter.plan, true); err != nil {
				cli.errorf("config sync connect --apply: %v", err)
				return 1
			}
		}
		cli.writeConfigSyncFollowups(loaded, adapter.plan, connect.materialize)
	} else {
		fmt.Fprintln(cli.options.Stdout, "config sync: already converged")
	}
	return 0
}

func (cli *CLI) runConfigSyncPath(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
) int {
	if loaded.Context.YardType == domain.YardRemote {
		return cli.forwardRemote(ctx, loaded.Context, "config", []string{"sync", "path"})
	}
	if len(arguments) != 0 {
		cli.errorf("config sync path accepts no arguments")
		return 2
	}
	record, exists, err := configsync.ReadSourceRecord(loaded.Context.Paths.ConfigHome)
	if err != nil {
		cli.errorf("config sync path: %v", err)
		return 1
	}
	if !exists {
		cli.errorf(
			"config sync path: no source is registered; run %s config sync connect <git-url>",
			cli.options.Program,
		)
		return 1
	}
	fmt.Fprintln(cli.options.Stdout, record.Checkout)
	return 0
}

func (cli *CLI) parseConfigSourceConnect(
	arguments []string,
) (configSourceConnectOptions, bool, error) {
	var result configSourceConnectOptions
	assumeYes := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "--host-id", "--checkout":
			if index+1 >= len(arguments) {
				return result, false, fmt.Errorf("%s needs a value", argument)
			}
			index++
			if argument == "--host-id" {
				if result.hostID != "" {
					return result, false, errors.New("--host-id may be specified only once")
				}
				result.hostID = arguments[index]
			} else {
				if result.checkout != "" {
					return result, false, errors.New("--checkout may be specified only once")
				}
				result.checkout = arguments[index]
			}
		case "-y", "--yes":
			assumeYes = true
		case "--init":
			result.initialize = true
		case "--apply":
			result.materialize = true
		default:
			if strings.HasPrefix(argument, "-") {
				return result, false, fmt.Errorf("unknown option %q", argument)
			}
			if result.origin != "" {
				return result, false, errors.New("connect accepts exactly one Git URL")
			}
			result.origin = argument
		}
	}
	if result.origin == "" {
		return result, false, errors.New("a Git URL is required")
	}
	return result, assumeYes, nil
}

func (cli *CLI) prepareConfigSource(
	ctx context.Context,
	loaded config.Loaded,
	request configSourceConnectOptions,
) (*preparedConfigSource, error) {
	origin, err := normalizeConfigGitSource(request.origin, cli.options.WorkingDir)
	if err != nil {
		return nil, err
	}
	record, registered, err := configsync.ReadSourceRecord(
		loaded.Context.Paths.ConfigHome,
	)
	if err != nil {
		return nil, err
	}
	checkout := request.checkout
	if checkout == "" && registered {
		checkout = record.Checkout
	}
	if checkout == "" {
		checkout = filepath.Join(
			loaded.Context.Paths.OperatorHome, ".local", "share", "subyard-config",
		)
	}
	if !filepath.IsAbs(checkout) {
		checkout = filepath.Join(cli.options.WorkingDir, checkout)
	}
	checkout, err = filepath.Abs(checkout)
	if err != nil {
		return nil, err
	}
	checkout = filepath.Clean(checkout)
	if err := validateConfigSourceCheckoutBoundary(
		checkout, loaded.Context.Paths, cli.options.RepositoryRoot,
	); err != nil {
		return nil, err
	}
	if registered && checkout != record.Checkout {
		return nil, fmt.Errorf(
			"configuration source is already registered at %s", record.Checkout,
		)
	}
	prepared := &preparedConfigSource{
		origin:        origin,
		checkout:      checkout,
		needsRegister: !registered,
	}
	info, statErr := os.Lstat(checkout)
	switch {
	case statErr == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("configuration source checkout must be a real directory")
		}
		if err := cli.verifyConfigGitOrigin(ctx, checkout, origin); err != nil {
			return nil, err
		}
	case errors.Is(statErr, os.ErrNotExist):
		ancestor, err := privateCheckoutAncestor(filepath.Dir(checkout))
		if err != nil {
			return nil, err
		}
		stage, err := os.MkdirTemp(ancestor, sourceStagePrefix)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(stage, 0o700); err != nil {
			_ = os.Remove(stage)
			return nil, err
		}
		prepared.stage = stage
		prepared.existingAncestor = ancestor
		prepared.needsClone = true
		if err := cli.cloneConfigSource(ctx, origin, stage); err != nil {
			prepared.cleanup()
			return nil, err
		}
	default:
		return nil, statErr
	}
	sourceRoot := checkout
	if prepared.needsClone {
		sourceRoot = prepared.stage
	}
	environment := make(map[string]string, len(cli.baseEnv)+1)
	for name, value := range cli.baseEnv {
		environment[name] = value
	}
	if request.hostID != "" {
		environment["SUBYARD_HOST_ID"] = request.hostID
	}
	sourceHead, headErr := cli.configGitInspectOutput(
		ctx, sourceRoot, "rev-parse", "--verify", "HEAD",
	)
	if headErr != nil {
		if !request.initialize {
			prepared.cleanup()
			return nil, errors.New(
				"configuration repository has no commit; repeat connect with --init to create the initial manifest",
			)
		}
		if !prepared.needsClone {
			prepared.candidateParent, prepared.initialCandidate, err =
				cli.cloneInitialConfigSourceCandidate(ctx, origin)
			if err != nil {
				prepared.cleanup()
				return nil, err
			}
			sourceRoot = prepared.initialCandidate
			prepared.targetBranch, err = cli.configGitInspectOutput(
				ctx, checkout, "symbolic-ref", "--quiet", "--short", "HEAD",
			)
			prepared.targetBranch = strings.TrimSpace(prepared.targetBranch)
			if err != nil || prepared.targetBranch == "" {
				prepared.cleanup()
				return nil, errors.New("empty configuration checkout has no unborn branch")
			}
		}
		branch, branchErr := cli.initializeConfigSource(ctx, sourceRoot)
		if branchErr != nil {
			prepared.cleanup()
			return nil, branchErr
		}
		if prepared.targetBranch != "" && prepared.targetBranch != branch {
			prepared.cleanup()
			return nil, errors.New("prepared initial branch does not match the empty checkout")
		}
		prepared.initialCommit, err = cli.configGitOutput(
			ctx, sourceRoot, "rev-parse", "--verify", "HEAD",
		)
		if err != nil {
			prepared.cleanup()
			return nil, err
		}
		prepared.initialCommit = strings.TrimSpace(prepared.initialCommit)
		prepared.initialPush = true
		prepared.initialBranch = branch
	} else if request.initialize {
		_ = sourceHead
		prepared.cleanup()
		return nil, errors.New(
			"--init is only valid for an empty configuration repository",
		)
	}
	prepared.options = configsync.Options{
		SourceRoot: sourceRoot, SourceIdentityRoot: checkout,
		ConfigHome:     loaded.Context.Paths.ConfigHome,
		RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome:   loaded.Context.Paths.OperatorHome,
		Environment:    environment,
		FileSettings:   config.SyncableFileMappings(loaded),
		Adopt:          true,
		YardInUse:      cli.configSyncYardInUse(loaded),
	}
	prepared.preview, err = configsync.BuildPlan(prepared.options)
	if err != nil {
		prepared.cleanup()
		return nil, err
	}
	if request.hostID != "" && prepared.preview.HostID != request.hostID {
		prepared.cleanup()
		return nil, fmt.Errorf(
			"saved owner host ID is %q, not requested %q",
			prepared.preview.HostID, request.hostID,
		)
	}
	return prepared, nil
}

func (cli *CLI) cloneInitialConfigSourceCandidate(
	ctx context.Context,
	origin string,
) (string, string, error) {
	parent, err := os.MkdirTemp("", sourceInitCandidatePrefix)
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		_ = os.Remove(parent)
		return "", "", err
	}
	candidate := filepath.Join(parent, "candidate")
	if err := cli.cloneConfigSource(ctx, origin, candidate); err != nil {
		_ = os.RemoveAll(parent)
		return "", "", err
	}
	return parent, candidate, nil
}

func (cli *CLI) initializeConfigSource(
	ctx context.Context,
	checkout string,
) (string, error) {
	if _, err := cli.configGitOutput(
		ctx, checkout, "var", "GIT_AUTHOR_IDENT",
	); err != nil {
		return "", errors.New(
			"Git author identity is not configured; set user.name and user.email for the operator account",
		)
	}
	branch, err := cli.configGitOutput(
		ctx, checkout, "symbolic-ref", "--quiet", "--short", "HEAD",
	)
	branch = strings.TrimSpace(branch)
	if err != nil || branch == "" {
		branch = "main"
		if err := cli.configGitRun(
			ctx, checkout, "symbolic-ref", "HEAD", "refs/heads/"+branch,
		); err != nil {
			return "", fmt.Errorf("initialize Git branch: %w", err)
		}
	}
	manifest := filepath.Join(checkout, "subyard-config.json")
	if err := os.WriteFile(
		manifest, []byte("{\n  \"schemaVersion\": 1\n}\n"), 0o600,
	); err != nil {
		return "", err
	}
	if err := cli.configGitRun(
		ctx, checkout, "add", "--", "subyard-config.json",
	); err != nil {
		return "", fmt.Errorf("stage initial configuration manifest: %w", err)
	}
	if err := cli.configGitRun(
		ctx, checkout, "commit", "--quiet", "-m",
		"Initialize Subyard configuration",
	); err != nil {
		return "", fmt.Errorf("create initial configuration commit: %w", err)
	}
	return branch, nil
}

func (cli *CLI) cloneConfigSource(
	ctx context.Context,
	origin string,
	destination string,
) error {
	return cli.cloneConfigSourceWithEnvironment(ctx, origin, destination, nil)
}

func (cli *CLI) cloneRegisteredConfigSource(
	ctx context.Context,
	origin string,
	destination string,
) error {
	return cli.cloneConfigSourceWithEnvironment(
		ctx, origin, destination, configGitInspectionOverrides(),
	)
}

func (cli *CLI) cloneConfigSourceWithEnvironment(
	ctx context.Context,
	origin string,
	destination string,
	environment map[string]string,
) error {
	command := exec.CommandContext(ctx,
		"git", "clone", "--quiet", "--", origin, destination,
	)
	command.Dir = filepath.Dir(destination)
	command.Env = cli.configGitEnvironment(environment)
	var diagnostics bytes.Buffer
	command.Stdout = io.Discard
	command.Stderr = &diagnostics
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(diagnostics.String())
		if len(message) > 4096 {
			message = message[:4096] + "..."
		}
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf(
			"Git clone failed; configure Git authentication on this owner host: %s",
			message,
		)
	}
	if err := hardenClonedConfigSource(destination); err != nil {
		return fmt.Errorf("protect cloned configuration source: %w", err)
	}
	return cli.verifyConfigGitOrigin(ctx, destination, origin)
}

func hardenClonedConfigSource(root string) error {
	gitDirectory := filepath.Join(root, ".git")
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == gitDirectory {
			return filepath.SkipDir
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.Mode().IsRegular() {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != uint32(os.Getuid()) {
				return fmt.Errorf("cloned path is not operator-owned: %s", path)
			}
			if stat.Nlink != 1 {
				return nil
			}
		}
		protected := info.Mode().Perm() &^ 0o022
		if protected == info.Mode().Perm() {
			return nil
		}
		return os.Chmod(path, protected)
	})
}

func hardenConfigCandidate(root string) error {
	gitPath := filepath.Join(root, ".git")
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == gitPath && info.IsDir() {
			return filepath.SkipDir
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.Mode().IsRegular() {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != uint32(os.Getuid()) {
				return fmt.Errorf("candidate path is not operator-owned: %s", path)
			}
			if stat.Nlink != 1 {
				content, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if err := os.Remove(path); err != nil {
					return err
				}
				if err := os.WriteFile(path, content, info.Mode().Perm()&^0o022); err != nil {
					return err
				}
				return nil
			}
		}
		protected := info.Mode().Perm() &^ 0o022
		if protected == info.Mode().Perm() {
			return nil
		}
		return os.Chmod(path, protected)
	})
}

func (cli *CLI) verifyConfigGitOrigin(
	ctx context.Context,
	checkout string,
	expected string,
) error {
	output, err := cli.configGitInspectOutput(
		ctx, checkout, "remote", "get-url", "origin",
	)
	if err != nil {
		return errors.New("configuration source checkout has no readable Git origin")
	}
	actual := strings.TrimSpace(output)
	if actual != expected {
		return fmt.Errorf(
			"configuration source checkout origin does not match the requested Git URL",
		)
	}
	return nil
}

func normalizeConfigGitSource(value, workingDirectory string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) ||
		strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("Git URL is empty or contains unsafe characters")
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme == "" {
			return "", errors.New("Git URL is invalid")
		}
		switch parsed.Scheme {
		case "https", "ssh", "git", "file":
		default:
			return "", fmt.Errorf("Git URL scheme %q is not supported", parsed.Scheme)
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", errors.New("Git URL must not contain a query or fragment")
		}
		if parsed.User != nil {
			_, password := parsed.User.Password()
			if password || parsed.Scheme == "https" {
				return "", errors.New(
					"Git credentials must not be embedded in the URL",
				)
			}
		}
		return value, nil
	}
	if configGitSCPStyle(value) {
		return value, nil
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(workingDirectory, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func configGitSCPStyle(value string) bool {
	colon := strings.IndexByte(value, ':')
	slash := strings.IndexByte(value, '/')
	if colon <= 0 || slash >= 0 && slash < colon {
		return false
	}
	host := value[:colon]
	if strings.ContainsAny(host, `\ `) {
		return false
	}
	if at := strings.IndexByte(host, '@'); at >= 0 {
		user := host[:at]
		host = host[at+1:]
		if user == "" || strings.Contains(user, ":") {
			return false
		}
	}
	return host != ""
}

func validateConfigSourceCheckoutBoundary(
	checkout string,
	paths domain.RuntimePaths,
	repositoryRoot string,
) error {
	if checkout == string(filepath.Separator) || checkout == paths.OperatorHome {
		return errors.New("configuration source checkout target is too broad")
	}
	for label, root := range map[string]string{
		"live configuration": paths.ConfigHome,
		"Subyard data":       paths.DataHome,
		"runtime repository": repositoryRoot,
	} {
		if pathContains(checkout, root) || pathContains(root, checkout) {
			return fmt.Errorf(
				"configuration source checkout must not overlap %s root %s",
				label, root,
			)
		}
	}
	return nil
}

func pathContains(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func privateCheckoutAncestor(path string) (string, error) {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if err := validatePrivateCheckoutDirectory(current, info); err != nil {
				return "", err
			}
			return current, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(current)
		if next == current {
			return "", errors.New(
				"configuration source checkout has no safe existing parent",
			)
		}
		current = next
	}
}

func validatePrivateCheckoutDirectory(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf(
			"configuration source parent must be a private real directory: %s", path,
		)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf(
			"configuration source parent is not operator-owned: %s", path,
		)
	}
	return nil
}

func installConfigSourceCheckout(
	stage string,
	checkout string,
	existingAncestor string,
) error {
	if stage == "" {
		return nil
	}
	parent := filepath.Dir(checkout)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	current := parent
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if err := validatePrivateCheckoutDirectory(current, info); err != nil {
			return err
		}
		if current == existingAncestor {
			break
		}
		next := filepath.Dir(current)
		if next == current || !pathContains(existingAncestor, next) {
			return errors.New("configuration source parent escaped its prepared root")
		}
		current = next
	}
	if _, err := os.Lstat(checkout); err == nil {
		return errors.New("configuration source checkout appeared after preview")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, checkout); err != nil {
		return fmt.Errorf("install prepared configuration source checkout: %w", err)
	}
	return syncConfigSourceDirectoryChain(parent, existingAncestor)
}

func syncConfigSourceDirectoryChain(path, stop string) error {
	current := path
	for {
		directory, err := os.Open(current)
		if err != nil {
			return err
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		if current == stop {
			return nil
		}
		next := filepath.Dir(current)
		if next == current || !pathContains(stop, next) {
			return errors.New("configuration source durability path escaped its prepared root")
		}
		current = next
	}
}

func (prepared *preparedConfigSource) cleanup() {
	if prepared == nil {
		return
	}
	if prepared.stage != "" {
		parent := filepath.Dir(prepared.stage)
		if parent == prepared.existingAncestor &&
			strings.HasPrefix(filepath.Base(prepared.stage), sourceStagePrefix) {
			_ = os.RemoveAll(prepared.stage)
		}
		prepared.stage = ""
	}
	if prepared.candidateParent != "" &&
		filepath.Dir(prepared.initialCandidate) == prepared.candidateParent &&
		strings.HasPrefix(filepath.Base(prepared.candidateParent), sourceInitCandidatePrefix) {
		_ = os.RemoveAll(prepared.candidateParent)
	}
	prepared.initialCandidate = ""
	prepared.candidateParent = ""
}

type configSourceConnectAdapter struct {
	cli      *CLI
	prepared *preparedConfigSource
	plan     configsync.Plan
}

func (adapter *configSourceConnectAdapter) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	_ io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "config-source" || request.Action != "connect" {
		return domain.AdapterResult{}, "", errors.New(
			"invalid configuration source adapter request",
		)
	}
	prepared := adapter.prepared
	if prepared.needsClone {
		if err := installConfigSourceCheckout(
			prepared.stage, prepared.checkout, prepared.existingAncestor,
		); err != nil {
			return domain.AdapterResult{}, "", err
		}
		prepared.stage = ""
	}
	if prepared.initialCandidate != "" {
		if err := adapter.cli.installInitialConfigSourceCandidate(ctx, prepared); err != nil {
			return domain.AdapterResult{}, "", err
		}
	}
	finalOptions := prepared.options
	finalOptions.SourceRoot = prepared.checkout
	finalOptions.SourceIdentityRoot = prepared.checkout
	finalPlan, err := configsync.BuildPlan(finalOptions)
	if err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf(
			"revalidate installed configuration source: %w", err,
		)
	}
	if finalPlan.Digest != prepared.preview.Digest {
		return domain.AdapterResult{}, "", errors.New(
			"configuration source or live settings changed after preview; rerun connect",
		)
	}
	if err := configsync.RegisterSourceOrigin(
		finalOptions.ConfigHome, prepared.checkout, prepared.origin,
	); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if err := configsync.Apply(finalPlan); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if prepared.initialPush {
		refspec := "HEAD:refs/heads/" + prepared.initialBranch
		if err := adapter.cli.configGitRun(
			ctx, prepared.checkout, "push", "--porcelain", "--set-upstream",
			"origin", refspec,
		); err != nil {
			return domain.AdapterResult{}, "", fmt.Errorf(
				"push initial configuration commit: %w; checkout remains registered and ahead for a safe retry",
				err,
			)
		}
	}
	adapter.plan = finalPlan
	return domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
		Output: map[string]any{
			"checkout":   prepared.checkout,
			"generation": finalPlan.Generation,
		},
	}, "", nil
}

func (cli *CLI) installInitialConfigSourceCandidate(
	ctx context.Context,
	prepared *preparedConfigSource,
) error {
	if err := cli.verifyConfigGitOrigin(ctx, prepared.checkout, prepared.origin); err != nil {
		return err
	}
	if head, err := cli.configGitOutput(
		ctx, prepared.checkout, "rev-parse", "--verify", "HEAD",
	); err == nil || strings.TrimSpace(head) != "" {
		return errors.New("configuration source checkout gained a commit after preview")
	}
	branch, err := cli.configGitOutput(
		ctx, prepared.checkout, "symbolic-ref", "--quiet", "--short", "HEAD",
	)
	if err != nil || strings.TrimSpace(branch) != prepared.targetBranch {
		return errors.New("configuration source checkout branch changed after preview")
	}
	status, err := cli.configGitOutput(
		ctx, prepared.checkout, "status", "--porcelain=v1", "--untracked-files=all",
	)
	if err != nil || strings.TrimSpace(status) != "" {
		return errors.New("configuration source checkout changed after preview")
	}
	refs, err := cli.configGitOutput(
		ctx, prepared.checkout, "for-each-ref", "--format=%(refname):%(objectname)",
	)
	if err != nil || strings.TrimSpace(refs) != "" {
		return errors.New("configuration source checkout refs changed after preview")
	}
	candidate, err := cli.configGitOutput(
		ctx, prepared.initialCandidate, "rev-parse", "--verify", "HEAD",
	)
	if err != nil || strings.TrimSpace(candidate) != prepared.initialCommit {
		return errors.New("prepared initial configuration commit changed")
	}
	if err := cli.configGitRun(
		ctx, prepared.checkout, "fetch", "--quiet", "--no-tags", "--",
		prepared.initialCandidate, prepared.initialCommit,
	); err != nil {
		return fmt.Errorf("import prepared initial configuration commit: %w", err)
	}
	if err := cli.configGitRun(
		ctx, prepared.checkout, "reset", "--hard", prepared.initialCommit,
	); err != nil {
		return fmt.Errorf("initialize configuration checkout: %w", err)
	}
	return hardenClonedConfigSource(prepared.checkout)
}
