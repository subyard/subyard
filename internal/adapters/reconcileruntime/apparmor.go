package reconcileruntime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func (runtime Runtime) incusAppArmorDisabled(ctx context.Context) (bool, error) {
	unknown := func(err error) (bool, error) {
		return false, fmt.Errorf("Incus AppArmor capability unknown: %w; check incus.service and retry init", err)
	}
	systemctl, err := runtime.executableFromPath("systemctl")
	if err != nil {
		return unknown(errors.New("systemctl is unavailable"))
	}
	command := exec.CommandContext(ctx, systemctl, "show", "incus.service", "-p", "Environment", "--value")
	command.Env = runtime.Environment
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return unknown(ctx.Err())
		}
		// Neither stdout nor stderr is safe to include: both can contain environment values.
		return unknown(errors.New("systemctl show failed"))
	}
	disabled, err := parseIncusAppArmor(strings.TrimRight(string(output), "\n"))
	if err != nil {
		return unknown(err)
	}
	return disabled, nil
}

// systemctl serializes Environment as shell-quoted assignments. Decode without
// evaluation so a flag-shaped substring in another value cannot become a flag.
// This observes the loaded Environment= property, not the daemon's full process
// environment. Compatibility provisioning completes reload/restart before init.
func parseIncusAppArmor(output string) (bool, error) {
	invalid := errors.New("invalid or ambiguous systemctl Environment output")
	if strings.ContainsAny(output, "\x00\x01\r\n") {
		return false, invalid
	}
	seen, disabled := false, false
	for i := 0; i < len(output); {
		if output[i] == ' ' || output[i] == '\t' {
			i++
			continue
		}
		var token strings.Builder
		var quote byte
		for ; i < len(output); i++ {
			c := output[i]
			if quote == 0 && (c == ' ' || c == '\t') {
				break
			}
			if c == '\\' && quote != '\'' {
				i++
				if i == len(output) {
					return false, invalid
				}
				// systemctl escapes only these bytes inside double quotes. Never
				// turn a malformed value such as "fa\lse" into the token "false".
				if quote == '"' && !strings.ContainsRune("\\\"$`", rune(output[i])) {
					return false, invalid
				}
				token.WriteByte(output[i])
			} else if quote != 0 && c == quote {
				quote = 0
			} else if quote == 0 && (c == '\'' || c == '"') {
				quote = c
			} else {
				token.WriteByte(c)
			}
		}
		name, value, assignment := strings.Cut(token.String(), "=")
		if quote != 0 || !assignment || name == "" {
			return false, invalid
		}
		for index, c := range name {
			if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || index > 0 && c >= '0' && c <= '9') {
				return false, invalid
			}
		}
		if name == "INCUS_SECURITY_APPARMOR" {
			if seen || (value != "true" && value != "false") {
				return false, invalid
			}
			seen, disabled = true, value == "false"
		}
	}
	return disabled, nil
}
