package incusclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

// ProbeInstanceAgentIf observes only the original VM's fixed true command. It
// cannot carry caller-selected commands or reserve native mutation receipts.
func (client *Client) ProbeInstanceAgentIf(ctx context.Context, project, name, uuid string) (bool, error) {
	return client.probeInstanceAgentIf(ctx, project, name, uuid, domain.YardVM)
}

// ProbeOriginalInstanceAgentIf exposes only fixed true against the captured kind.
func (client *Client) ProbeOriginalInstanceAgentIf(ctx context.Context, project, name, uuid string, kind domain.YardKind) (bool, error) {
	if kind != domain.YardVM && kind != domain.YardContainer {
		return false, domain.ErrPlanStale
	}
	return client.probeInstanceAgentIf(ctx, project, name, uuid, kind)
}
func (client *Client) probeInstanceAgentIf(ctx context.Context, project, name, uuid string, kind domain.YardKind) (bool, error) {
	if uuid == "" || !domain.SafeName(project) || !domain.SafeName(name) {
		return false, domain.ErrPlanStale
	}
	ctx, cancel := context.WithTimeout(ports.WithoutNativeOperationObserver(ctx), 5*time.Second)
	defer cancel()
	if err := client.checkOriginalKindUUID(ctx, project, name, uuid, kind); err != nil {
		return false, err
	}
	_, err := client.exec(ctx, project, name, ports.InstanceExecRequest{Command: []string{"true"}}, bytes.NewReader(nil), true)
	err = errors.Join(err, client.checkOriginalKindUUID(ctx, project, name, uuid, kind))
	return err == nil, err
}

func finishInstanceAgentProbe(ctx context.Context, operation incus.Operation, dataDone <-chan bool) error {
	wait := make(chan error, 1)
	go func() { wait <- operation.WaitContext(ctx) }()
	incomplete := func(boundary string) error {
		// Cancel is a best effort request through the same SDK connection. Neither
		// cancellation nor an escaped websocket proves remote or stream completion.
		go func() { _ = operation.Cancel() }()
		return fmt.Errorf("agent probe %s unproved: %w", boundary, context.Cause(ctx))
	}
	var waitErr error
	select {
	case <-ctx.Done():
		return incomplete("wait and stream drain")
	case waitErr = <-wait:
	}
	select {
	case <-ctx.Done():
		return errors.Join(waitErr, incomplete("stream drain"))
	case <-dataDone:
	}
	if ctx.Err() != nil {
		return incomplete("completion")
	}
	remote := operation.Get()
	if waitErr != nil {
		return normalizeOperationError("wait for agent probe", remote, waitErr)
	}
	if remote.StatusCode != api.Success {
		return errors.New("agent probe remote success unproved")
	}
	code, valid := explicitNativeExecExit(remote.Metadata)
	if !valid {
		return errors.New("agent probe return status unproved")
	}
	if code != 0 {
		return fmt.Errorf("agent probe exited with status %d", code)
	}
	return nil
}
