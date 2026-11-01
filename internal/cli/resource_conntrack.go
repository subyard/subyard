package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func runResourceConntrack(ctx context.Context, sourceRoot, binary string, sudo bool, arguments ...string) ([]byte, error) {
	return runResourceConntrackPostcondition(ctx, sourceRoot, binary, sudo, nil, arguments...)
}

func runResourceConntrackPostcondition(ctx context.Context, sourceRoot, binary string, sudo bool, settle func(error) error, arguments ...string) ([]byte, error) {
	return runResourceConntrackWithSudo(ctx, sourceRoot, binary, sudo, "/usr/bin/sudo", settle, arguments...)
}

func runResourceConntrackWithSudo(ctx context.Context, sourceRoot, binary string, sudo bool, nativeSudo string, settle func(error) error, arguments ...string) ([]byte, error) {
	if ports.NativeOperationObserverFrom(ctx) == nil {
		command := exec.CommandContext(ctx, binary, arguments...)
		if sudo {
			command = exec.CommandContext(ctx, "sudo", append([]string{"-n", "--", binary}, arguments...)...)
		}
		return command.Output()
	}
	home := ports.NativeOwnerHome(ctx)
	if home == "" {
		return nil, errors.New("native conntrack requires original owner scope")
	}
	owner, err := uninstall.BorrowOwnerLock(home)
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, errors.New("native conntrack requires original owner barrier")
	}
	defer owner.Close()
	script := filepath.Join(sourceRoot, "scripts/lib/native-owner-barrier.py")
	file, err := os.Open(script)
	if err != nil {
		return nil, err
	}
	source, readErr := io.ReadAll(io.LimitReader(file, (32<<20)+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if len(source) > 32<<20 {
		return nil, errors.New("native conntrack source bound")
	}
	argv := append([]string{"-B", script, "conntrack", strconv.Itoa(os.Getuid()), home, binary}, arguments...)
	command := exec.CommandContext(ctx, "/usr/bin/python3", argv...)
	if sudo {
		command = exec.CommandContext(ctx, nativeSudo, append([]string{"-n", "--", "/usr/bin/python3"}, argv...)...)
	}
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	command.Stdin = owner
	configureResourceProcess(command)
	stdout := &boundedResourceBuffer{limit: resource.MaxPrepareOutputBytes}
	stderr := &boundedResourceBuffer{limit: resource.MaxPrepareOutputBytes}
	command.Stdout, command.Stderr = stdout, stderr
	err = shelladapter.RunNativeCommand(ctx, command, source, nil, func(waitErr error) error {
		if stdout.overflow || stderr.overflow {
			return errors.Join(waitErr, errors.New("conntrack output exceeded limit"))
		}
		if settle != nil {
			// A typed deletion exit may be reconciled. Lost streams or other
			// transport failures cannot be converted into a completion fact.
			var exit *exec.ExitError
			if waitErr != nil && !errors.As(waitErr, &exit) {
				return waitErr
			}
			return settle(waitErr)
		}
		return waitErr
	})
	return stdout.Bytes(), err
}
