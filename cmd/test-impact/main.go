package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/testimpact"
)

const (
	formatHuman = "human"
	formatJSON  = "json"
)

type commandOptions struct {
	format      string
	currentBase string
	base        string
	head        string
	changesFrom string
}

type commitSourceOperations struct {
	resolveCommit      func(context.Context, string, string) (string, error)
	selectorPathsDirty func(context.Context, string) (bool, error)
	readCommitChanges  func(context.Context, string, string, string) (testimpact.ChangeSet, error)
}

func main() {
	root, _ := os.Getwd()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, root))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, root string) int {
	return runWithRegistryLoader(args, stdin, stdout, stderr, root, testimpact.BuiltInRegistry)
}

func runWithRegistryLoader(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	root string,
	loadRegistry func() (testimpact.Registry, error),
) int {
	options, err := parseCommandOptions(args)
	if err != nil {
		resultErrors := []testimpact.ResultError{{Code: "CLI_MISUSE", Message: "invalid command line"}}
		if options.format == formatJSON {
			_ = testimpact.WriteErrorJSON(stdout, resultErrors)
		} else {
			_ = testimpact.WriteErrorHuman(stdout, resultErrors)
		}
		writeDiagnostics(stderr, resultErrors)
		return 2
	}

	registry, err := loadRegistry()
	if err != nil {
		return writeFallback(stdout, stderr, options.format, nil, []testimpact.ResultError{{
			Code: "REGISTRY_INVALID", Message: "embedded check registry could not be validated",
		}})
	}
	canonicalRoot, err := canonicalRepositoryRoot(root)
	if err != nil {
		return writeFallback(stdout, stderr, options.format, nil, []testimpact.ResultError{{
			Code: "POLICY_INVALID", Message: "impact policy could not be validated",
		}})
	}
	policy, err := loadRepositoryPolicy(canonicalRoot, registry)
	if err != nil {
		return writeFallback(stdout, stderr, options.format, nil, []testimpact.ResultError{{
			Code: "POLICY_INVALID", Message: "impact policy could not be validated",
		}})
	}

	changeSet, resultError := readChangeSource(context.Background(), canonicalRoot, options, stdin)
	if resultError != nil {
		return writeFallback(stdout, stderr, options.format, nil, []testimpact.ResultError{*resultError})
	}
	result := testimpact.Select(policy, registry, changeSet)
	if len(result.Errors) != 0 {
		return writeFallback(stdout, stderr, options.format, result.Changes, sanitizeSelectionErrors(result.Errors))
	}
	if err := writeResult(stdout, options.format, result); err != nil {
		return 1
	}
	return 0
}

func parseCommandOptions(args []string) (commandOptions, error) {
	options := commandOptions{format: discoverOutputFormat(args)}
	seen := make(map[string]struct{})
	for index := 0; index < len(args); index++ {
		name := args[index]
		switch name {
		case "--current-base", "--base", "--head", "--changes-from", "--format":
		default:
			return options, fmt.Errorf("unknown argument")
		}
		if _, exists := seen[name]; exists {
			return options, fmt.Errorf("duplicate flag")
		}
		seen[name] = struct{}{}
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || args[index+1] == "" {
			return options, fmt.Errorf("missing flag value")
		}
		index++
		value := args[index]
		switch name {
		case "--current-base":
			options.currentBase = value
		case "--base":
			options.base = value
		case "--head":
			options.head = value
		case "--changes-from":
			options.changesFrom = value
		case "--format":
			if value != formatHuman && value != formatJSON {
				return options, fmt.Errorf("invalid format")
			}
			options.format = value
		}
	}

	currentMode := options.currentBase != ""
	commitMode := options.base != "" || options.head != ""
	fileMode := options.changesFrom != ""
	modeCount := 0
	for _, enabled := range []bool{currentMode, commitMode, fileMode} {
		if enabled {
			modeCount++
		}
	}
	if modeCount != 1 || (commitMode && (options.base == "" || options.head == "")) {
		return options, fmt.Errorf("exactly one complete source is required")
	}
	return options, nil
}

func discoverOutputFormat(args []string) string {
	format := formatHuman
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "--format" {
			continue
		}
		switch args[index+1] {
		case formatJSON:
			format = formatJSON
		case formatHuman:
			format = formatHuman
		}
	}
	return format
}

func writeDiagnostics(writer io.Writer, resultErrors []testimpact.ResultError) {
	for _, resultError := range resultErrors {
		fmt.Fprintf(writer, "test-impact: %s: %s\n", resultError.Code, resultError.Message)
	}
}

func canonicalRepositoryRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("repository root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("repository root is invalid")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("repository root is unavailable")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("repository root is not a directory")
	}
	return canonical, nil
}

func loadRepositoryPolicy(root string, registry testimpact.Registry) (testimpact.Policy, error) {
	mapFile, err := os.Open(filepath.Join(root, "tests/impact-map.json"))
	if err != nil {
		return testimpact.Policy{}, errors.New("open impact policy")
	}
	defer mapFile.Close()
	policy, err := testimpact.LoadPolicy(mapFile, registry)
	if err != nil {
		return testimpact.Policy{}, errors.New("validate impact policy")
	}
	return policy, nil
}

func readChangeSource(ctx context.Context, root string, options commandOptions, stdin io.Reader) (testimpact.ChangeSet, *testimpact.ResultError) {
	return readChangeSourceWithCommitOperations(ctx, root, options, stdin, commitSourceOperations{
		resolveCommit:      resolveCommit,
		selectorPathsDirty: selectorPathsDirty,
		readCommitChanges:  testimpact.ReadCommitChanges,
	})
}

func readChangeSourceWithCommitOperations(
	ctx context.Context,
	root string,
	options commandOptions,
	stdin io.Reader,
	operations commitSourceOperations,
) (testimpact.ChangeSet, *testimpact.ResultError) {
	if options.currentBase != "" {
		changeSet, err := testimpact.ReadCurrentChanges(ctx, root, options.currentBase)
		if err != nil {
			return testimpact.ChangeSet{}, &testimpact.ResultError{
				Code: "CHANGE_SOURCE_INVALID", Message: "change source could not be analyzed",
			}
		}
		return changeSet, nil
	}
	if options.changesFrom != "" {
		changeSet, err := decodeChangeSource(options.changesFrom, stdin)
		if err != nil {
			return testimpact.ChangeSet{}, &testimpact.ResultError{
				Code: "CHANGE_SOURCE_INVALID", Message: "change source could not be analyzed",
			}
		}
		return changeSet, nil
	}
	baseCommit, headCommit, err := canonicalCommitRange(ctx, root, options.base, options.head, operations)
	if err != nil {
		return testimpact.ChangeSet{}, &testimpact.ResultError{
			Code: "COMMIT_GUARD_FAILED", Message: "commit source does not match the trusted checkout",
		}
	}
	changeSet, err := operations.readCommitChanges(ctx, root, baseCommit, headCommit)
	if err != nil {
		return testimpact.ChangeSet{}, &testimpact.ResultError{
			Code: "CHANGE_SOURCE_INVALID", Message: "change source could not be analyzed",
		}
	}
	return changeSet, nil
}

func decodeChangeSource(name string, stdin io.Reader) (testimpact.ChangeSet, error) {
	if name == "-" {
		return testimpact.DecodeChangeSet(stdin)
	}
	file, err := os.Open(name)
	if err != nil {
		return testimpact.ChangeSet{}, errors.New("open change source")
	}
	defer file.Close()
	return testimpact.DecodeChangeSet(file)
}

func canonicalCommitRange(
	ctx context.Context,
	root, base, head string,
	operations commitSourceOperations,
) (string, string, error) {
	resolvedBase, err := operations.resolveCommit(ctx, root, base)
	if err != nil {
		return "", "", err
	}
	resolvedHead, err := operations.resolveCommit(ctx, root, head)
	if err != nil {
		return "", "", err
	}
	currentHead, err := operations.resolveCommit(ctx, root, "HEAD")
	if err != nil {
		return "", "", err
	}
	if resolvedHead != currentHead {
		return "", "", errors.New("head is not current")
	}
	dirty, err := operations.selectorPathsDirty(ctx, root)
	if err != nil {
		return "", "", errors.New("inspect selector-owned paths")
	}
	if dirty {
		return "", "", errors.New("selector-owned path is dirty")
	}
	return resolvedBase, resolvedHead, nil
}

func resolveCommit(ctx context.Context, root, ref string) (string, error) {
	output, err := runTrustedGit(ctx, root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", errors.New("resolve commit")
	}
	resolved := strings.TrimSpace(string(output))
	if resolved == "" || strings.ContainsAny(resolved, "\x00\r\n\t ") {
		return "", errors.New("invalid commit identity")
	}
	return resolved, nil
}

func selectorPathsDirty(ctx context.Context, root string) (bool, error) {
	output, err := runTrustedGit(ctx, root,
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--",
		"internal/testimpact", "cmd/test-impact", "dev/test-impact.sh", "tests/impact-map.json",
	)
	if err != nil {
		return false, err
	}
	return len(output) != 0, nil
}

func runTrustedGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	commandArgs := []string{"-c", "safe.directory=" + root, "-C", root}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
	}
	output, err := command.Output()
	if err != nil {
		return nil, errors.New("trusted Git command failed")
	}
	return output, nil
}

func sanitizeSelectionErrors(resultErrors []testimpact.ResultError) []testimpact.ResultError {
	sanitized := make([]testimpact.ResultError, 0, len(resultErrors))
	for _, resultError := range resultErrors {
		message := "impact analysis failed"
		switch resultError.Code {
		case "UNMATCHED_PATH":
			message = "a changed repository path is not covered by impact policy"
		case "INVALID_CHANGE_PATH":
			message = "a changed repository path is invalid"
		case "REGISTRY_EXPANSION_FAILED":
			message = "selected checks could not be expanded"
		}
		sanitized = append(sanitized, testimpact.ResultError{Code: resultError.Code, Message: message})
	}
	return sanitized
}

func writeFallback(stdout, stderr io.Writer, format string, changes []testimpact.Change, resultErrors []testimpact.ResultError) int {
	result := universalFallback(changes, resultErrors)
	if err := writeResult(stdout, format, result); err != nil {
		return 1
	}
	writeDiagnostics(stderr, resultErrors)
	return 0
}

func universalFallback(changes []testimpact.Change, resultErrors []testimpact.ResultError) testimpact.Result {
	if changes == nil {
		changes = []testimpact.Change{}
	}
	return testimpact.Result{
		SchemaVersion:  1,
		Status:         "fallback",
		Changes:        changes,
		CheckSets:      []string{"host-free:all"},
		RiskDomains:    []string{},
		HostFreeChecks: emergencyHostFreeChecks(),
		E2EChecks:      []testimpact.CheckRecommendation{},
		FullP0: testimpact.FullP0Selection{Required: true, Reasons: []testimpact.FullP0Reason{{
			Code: "universal_fallback", RiskDomains: []string{},
		}}},
		Reasons: []testimpact.SelectionReason{},
		Errors:  resultErrors,
	}
}

func emergencyHostFreeChecks() []testimpact.CheckRecommendation {
	return []testimpact.CheckRecommendation{
		{ID: "host-free:core", Tier: "T2", BudgetSeconds: 1800, Rationale: "required core host-free merge gate"},
		{ID: "veranda:build", Tier: "T1", BudgetSeconds: 180, Rationale: "Veranda production build"},
		{ID: "veranda:check", Tier: "T1", BudgetSeconds: 180, Rationale: "Veranda static checks"},
		{ID: "veranda:rust-test", Tier: "T1", BudgetSeconds: 300, Rationale: "Veranda Rust tests without desktop dependencies"},
		{ID: "veranda:test", Tier: "T1", BudgetSeconds: 180, Rationale: "Veranda unit tests"},
	}
}

func writeResult(writer io.Writer, format string, result testimpact.Result) error {
	if format == formatJSON {
		return testimpact.WriteJSON(writer, result)
	}
	return testimpact.WriteHuman(writer, result)
}
