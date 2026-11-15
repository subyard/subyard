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
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/shellquote"
)

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
