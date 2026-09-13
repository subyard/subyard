package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/audit"
)

type hostLogOptions struct {
	mode  string
	lines int
	help  bool
}

func hostLogInvocation(arguments []string) bool {
	return slices.Contains(arguments, "--updates") || slices.Contains(arguments, "--audit")
}

func (cli *CLI) runHostLogs(arguments []string) int {
	options, err := parseHostLogOptions(arguments)
	if err != nil {
		cli.errorf("logs: %v", err)
		return 2
	}
	if options.help {
		fmt.Fprintf(cli.options.Stdout, "Usage: %s logs <%s> [-n LINES]\n", cli.options.Program, options.mode)
		return 0
	}
	home := cli.env["SUBYARD_HOME"]
	if home == "" {
		operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
		if operatorHome == "" {
			operatorHome = cli.env["HOME"]
		}
		if operatorHome != "" {
			home = filepath.Join(operatorHome, ".subyard")
		}
	}
	if home == "" || !filepath.IsAbs(home) {
		cli.errorf("logs: Subyard data home is unavailable")
		return 1
	}
	if options.mode == "--audit" {
		lines, err := audit.ReadAuditLines(home, options.lines)
		if err != nil {
			cli.errorf("logs: read audit history: %v", err)
			return 1
		}
		for _, line := range lines {
			fmt.Fprintln(cli.options.Stdout, line)
		}
		return 0
	}
	records, err := (audit.UpdateHistory{Home: home}).Read(options.lines)
	if err != nil {
		cli.errorf("logs: read update history: %v", err)
		return 1
	}
	for _, record := range records {
		fmt.Fprintf(cli.options.Stdout,
			"%s attempt=%s operation=%s direction=%s status=%s phase=%s source=%s target=%s",
			record.StartedAt.Format(time.RFC3339), record.AttemptID, record.OperationID,
			record.Direction, record.Status, record.Phase,
			firstNonempty(record.SourceVersion, record.SourceRelease, "unknown"),
			firstNonempty(record.TargetVersion, record.TargetRelease, "unknown"),
		)
		if record.ErrorCode != "" {
			fmt.Fprintf(cli.options.Stdout, " code=%s", record.ErrorCode)
		}
		fmt.Fprintln(cli.options.Stdout)
		for _, event := range record.Events {
			fmt.Fprintf(cli.options.Stdout, "  %s phase=%s status=%s",
				event.At.Format(time.RFC3339), event.Phase, event.Status)
			if event.ErrorCode != "" {
				fmt.Fprintf(cli.options.Stdout, " code=%s", event.ErrorCode)
			}
			fmt.Fprintln(cli.options.Stdout)
		}
	}
	return 0
}

func parseHostLogOptions(arguments []string) (hostLogOptions, error) {
	result := hostLogOptions{lines: 20}
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; argument {
		case "--updates", "--audit":
			if result.mode != "" && result.mode != argument {
				return hostLogOptions{}, errors.New("choose exactly one of --updates or --audit")
			}
			result.mode = argument
		case "-n":
			index++
			if index >= len(arguments) {
				return hostLogOptions{}, errors.New("-n needs a positive number")
			}
			lines, err := strconv.Atoi(arguments[index])
			if err != nil || lines < 1 || lines > 1000 {
				return hostLogOptions{}, errors.New("-n needs a number from 1 to 1000")
			}
			result.lines = lines
		case "-y", "--yes":
		case "-h", "--help":
			result.help = true
		case "-f":
			return hostLogOptions{}, errors.New("-f is supported only for yard runtime logs")
		default:
			if strings.HasPrefix(argument, "-") {
				return hostLogOptions{}, fmt.Errorf("unknown option %q", argument)
			}
			return hostLogOptions{}, errors.New("host logs do not accept a systemd unit")
		}
	}
	if result.mode == "" {
		return hostLogOptions{}, errors.New("host log selector is required")
	}
	return result, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
