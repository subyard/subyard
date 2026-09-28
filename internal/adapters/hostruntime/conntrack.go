package hostruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// ConntrackCleaner removes stale pre-NAT UDP state for one already verified
// public endpoint. The reply-source filter selects entries still aimed at the
// owner, leaving the live DNAT route and unrelated flows intact.
type ConntrackCleaner struct {
	Lookup func(string) (string, error)
	Run    func(context.Context, string, ...string) ([]byte, error)
}

func (cleaner ConntrackCleaner) Clear(ctx context.Context, endpoint netip.AddrPort) error {
	if !endpoint.Addr().Is4() || !endpoint.Addr().IsGlobalUnicast() || endpoint.Port() == 0 {
		return errors.New("invalid UDP conntrack endpoint")
	}
	commandContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := commandContext.Err(); err != nil {
		return err
	}
	lookup := cleaner.Lookup
	if lookup == nil {
		lookup = func(string) (string, error) {
			if binary, found := FindConntrack(os.Getenv("PATH"), "/usr/sbin", "/sbin"); found {
				return binary, nil
			}
			return "", exec.ErrNotFound
		}
	}
	binary, err := lookup("conntrack")
	if err != nil {
		return errors.New("conntrack is required for VM public UDP boot recovery; rerun yard init")
	}
	run := cleaner.Run
	if run == nil {
		run = func(ctx context.Context, name string, arguments ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, arguments...).Output()
		}
	}
	filter := []string{"-f", "ipv4", "-p", "udp", "--orig-dst", endpoint.Addr().String(),
		"--orig-port-dst", strconv.Itoa(int(endpoint.Port())), "--reply-src", endpoint.Addr().String()}
	list := func() ([]byte, error) {
		return run(commandContext, binary, append([]string{"-L"}, filter...)...)
	}
	entries, err := list()
	if commandContext.Err() != nil {
		return commandContext.Err()
	}
	if err != nil {
		return fmt.Errorf("inspect stale UDP conntrack entries: %w", sanitizedConntrackError(err))
	}
	if len(bytes.TrimSpace(entries)) == 0 {
		return nil
	}
	if _, err := run(commandContext, binary, append([]string{"-D"}, filter...)...); err != nil {
		if commandContext.Err() != nil {
			return commandContext.Err()
		}
		remaining, recheckErr := list()
		if commandContext.Err() != nil {
			return commandContext.Err()
		}
		if recheckErr == nil && len(bytes.TrimSpace(remaining)) == 0 {
			return nil // another owner removed the exact entry between list and delete
		}
		return fmt.Errorf("delete stale UDP conntrack entries: %w", sanitizedConntrackError(err))
	}
	if commandContext.Err() != nil {
		return commandContext.Err()
	}
	return nil
}

func sanitizedConntrackError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("conntrack exited %d", exit.ExitCode())
	}
	return errors.New("conntrack command failed")
}
