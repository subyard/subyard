package systemdunit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// RequireFreshLoaded observes the manager's cached unit state without changing it.
func RequireFreshLoaded(
	ctx context.Context,
	systemctl string,
	environment []string,
	unit string,
) error {
	return requireManagerState(ctx, systemctl, environment, unit, false)
}

// RequireSettledPrevious observes a unit restored from a retained release. A
// previously published unit may be incompatible with the current systemd and
// therefore parse as bad-setting; it must still have been reloaded completely.
func RequireSettledPrevious(
	ctx context.Context,
	systemctl string,
	environment []string,
	unit string,
) error {
	return requireManagerState(ctx, systemctl, environment, unit, true)
}

func requireManagerState(
	ctx context.Context,
	systemctl string,
	environment []string,
	unit string,
	allowBadSetting bool,
) error {
	command := exec.CommandContext(
		ctx,
		systemctl,
		"show",
		unit,
		"--property=LoadState",
		"--property=NeedDaemonReload",
	)
	command.Env = environment
	var output bytes.Buffer
	command.Stdin, command.Stdout, command.Stderr = nil, &output, io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("query systemd manager state: %w", err)
	}
	loadState, needDaemonReload, err := parseManagerState(output.String())
	if err != nil {
		return err
	}
	loadStateAccepted := loadState == "loaded" || allowBadSetting && loadState == "bad-setting"
	if !loadStateAccepted || needDaemonReload != "no" {
		return fmt.Errorf(
			"systemd manager state is LoadState=%s NeedDaemonReload=%s",
			loadState,
			needDaemonReload,
		)
	}
	return nil
}

func parseManagerState(output string) (string, string, error) {
	values := make(map[string]string, 2)
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 2 {
		return "", "", errors.New("systemd manager state has an unexpected shape")
	}
	for _, line := range lines {
		name, value, ok := strings.Cut(line, "=")
		if !ok || value == "" || (name != "LoadState" && name != "NeedDaemonReload") {
			return "", "", errors.New("systemd manager state has an unexpected property")
		}
		if _, exists := values[name]; exists {
			return "", "", errors.New("systemd manager state repeats a property")
		}
		values[name] = value
	}
	return values["LoadState"], values["NeedDaemonReload"], nil
}
