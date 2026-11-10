package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/state"
)

const projectNameMigrationID = "normalize-project-names"

// CheckLegacyGitConfig refuses activation until the operator has preserved or
// removed the retired drop-in. Inspection never imports settings or changes files.
func CheckLegacyGitConfig(ctx context.Context, yard domain.Context) error {
	path := filepath.Join(yard.Paths.DataHome, "gitconfig")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy gitconfig: %w", err)
	}
	refuse := func(reason string) error {
		return domain.MigrationStateRefusal(yard.YardName, errors.New(reason), reason)
	}
	reason := fmt.Sprintf("legacy gitconfig is no longer managed by Subyard; move or delete %q after preserving its settings", path)
	if !info.Mode().IsRegular() {
		return refuse(reason + "; cannot inspect a regular drop-in file")
	}
	payload, err := (config.MaterializedAsset{Source: path}).ReadSource()
	if err != nil {
		return domain.CollectRefusals(refuse(reason), fmt.Errorf("read legacy gitconfig: %w", err))
	}
	command := exec.CommandContext(ctx, "git", "config", "--file", "-", "--no-includes", "--null", "--list")
	command.Stdin = bytes.NewReader(payload)
	output, err := command.Output()
	if err != nil {
		return domain.CollectRefusals(refuse(reason), fmt.Errorf("read legacy Git configuration keys: %w", err))
	}
	var keys []string
	values := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		keys = append(keys, key)
		values[key] = value
	}
	slices.Sort(keys)
	refusals := []error{refuse(reason)}
	for _, key := range slices.Compact(keys) {
		if key == "" {
			continue
		}
		setting := ""
		switch key {
		case "user.name":
			setting = "GIT_USER_NAME"
		case "user.email":
			setting = "GIT_USER_EMAIL"
		}
		if setting != "" {
			suggestion := fmt.Sprintf("yard -Y %s config set %s %s --scope yard",
				yard.YardName, setting, shellquote.Word(values[key]))
			refusals = append(refusals, refuse("legacy gitconfig identity: run "+suggestion))
		} else {
			refusals = append(refusals, domain.MigrationStateRefusal(yard.YardName,
				fmt.Errorf("legacy gitconfig key %q is no longer managed by Subyard; preserve it separately", key),
				"legacy gitconfig key is no longer managed by Subyard; preserve it separately", key))
		}
	}
	return domain.CollectRefusals(refusals...)
}

type Report struct {
	SchemaVersion          int      `json:"schemaVersion"`
	ProjectStateSchema     int      `json:"projectStateSchema"`
	CredentialSchema       int      `json:"credentialSchema"`
	Layout                 int      `json:"layout,omitempty"`
	TargetLayout           int      `json:"targetLayout,omitempty"`
	RequiredMigrations     []string `json:"requiredMigrations,omitempty"`
	AffectedResources      []string `json:"affectedResources,omitempty"`
	Phase                  string   `json:"phase,omitempty"`
	ProjectStoresValidated int      `json:"projectStoresValidated"`
	CredentialRevisions    int      `json:"credentialRevisions"`
	Pending                bool     `json:"pending,omitempty"`
	Changed                bool     `json:"changed"`
}

// Check validates every existing store before an engine replacement without
// changing it.
func Check(
	ctx context.Context,
	projectDirectories []string,
	credentials ports.CredentialMetadataReader,
) (Report, error) {
	return run(ctx, projectDirectories, credentials, false)
}

// Apply performs registered, backwards-compatible repairs before validating
// every store.
func Apply(
	ctx context.Context,
	projectDirectories []string,
	credentials ports.CredentialMetadataReader,
) (Report, error) {
	return run(ctx, projectDirectories, credentials, true)
}

func run(
	ctx context.Context,
	projectDirectories []string,
	credentials ports.CredentialMetadataReader,
	apply bool,
) (Report, error) {
	report := Report{SchemaVersion: 1, ProjectStateSchema: 1, CredentialSchema: 1}
	directories := uniquePaths(projectDirectories)
	for _, directory := range directories {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Report{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return Report{}, errors.New("project state path is not a real directory")
		}
		store, err := state.NewFileStore(directory)
		if err != nil {
			return Report{}, err
		}
		if apply {
			changed, err := store.RepairLegacyPermissions(ctx)
			if err != nil {
				return Report{}, err
			}
			report.Changed = report.Changed || changed
			changed, err = store.MigrateLegacyNames(ctx)
			if err != nil {
				return Report{}, err
			}
			report.Changed = report.Changed || changed
		} else {
			pending, err := store.LegacyNameMigrationPending(ctx)
			if err != nil {
				return Report{}, err
			}
			if pending {
				report.Pending = true
				if !contains(report.RequiredMigrations, projectNameMigrationID) {
					report.RequiredMigrations = append(
						report.RequiredMigrations, projectNameMigrationID,
					)
				}
				report.AffectedResources = append(report.AffectedResources, directory)
			}
		}
		if _, err := store.List(ctx); err != nil {
			return Report{}, err
		}
		report.ProjectStoresValidated++
	}
	if credentials != nil {
		metadata, err := credentials.ListMetadata(ctx)
		if err != nil {
			return Report{}, err
		}
		report.CredentialRevisions = len(metadata)
	}
	return report, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func uniquePaths(paths []string) []string {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if filepath.IsAbs(path) {
			set[filepath.Clean(path)] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}
