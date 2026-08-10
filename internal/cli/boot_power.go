package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/ports"
)

const bootPowerTemporaryFailure = 75

func RunBootPower(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	reconciler application.BootPowerReconciler,
) int {
	if len(arguments) == 1 && arguments[0] == "has-managed" {
		managed, err := reconciler.HasManaged(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "subyard-power: %v\n", err)
			return 2
		}
		if !managed {
			return 1
		}
		return 0
	}
	if len(arguments) != 0 {
		fmt.Fprintln(stderr, "subyard-power: _power-reconcile accepts only has-managed")
		return 2
	}
	result, err := reconciler.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "subyard-power: FAIL: %v\n", err)
		if errors.Is(err, ports.ErrIncusUnavailable) {
			return bootPowerTemporaryFailure
		}
		return 1
	}
	if len(result.Started)+len(result.Stopped)+len(result.AlreadyRunning) == 0 {
		fmt.Fprintln(stdout, "subyard-power: no managed yards")
		return 0
	}
	for _, reference := range result.Stopped {
		fmt.Fprintf(stdout, "subyard-power: stopped %s\n", reference)
	}
	for _, reference := range result.Started {
		fmt.Fprintf(stdout, "subyard-power: started %s\n", reference)
	}
	for _, reference := range result.AlreadyRunning {
		fmt.Fprintf(stdout, "subyard-power: %s already running\n", reference)
	}
	fmt.Fprintln(stdout, "subyard-power: desired power restored")
	return 0
}
