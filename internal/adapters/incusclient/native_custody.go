package incusclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"net/url"
	"strings"
	"time"
)

// waitNative preserves the ordinary SDK wait path when no native custodian is
// present. Custodied calls durably report the exact SDK ID before waiting and
// require an explicit final remote status. A nil Wait is insufficient.
func waitNative(ctx context.Context, server incus.InstanceServer, operation incus.Operation, kind, project, resource string) (result error) {
	observer := ports.NativeOperationObserverFrom(ctx)
	if observer == nil {
		return operation.Wait()
	}
	defer func() {
		if result != nil {
			if _, closed := result.(*ports.NativeOperationFailure); !closed {
				observer.Uncertain()
			}
		}
	}()
	value, err := nativeOperation(server, kind, project, resource)
	if err != nil {
		_ = operation.Cancel()
		_ = operation.WaitContext(ctx)
		return err
	}
	value.ID = operation.Get().ID
	var recordErr error
	if err := nativeOperationIdentity(operation.Get(), value); err != nil {
		recordErr = err
	} else {
		recordErr = observer.Accepted(value)
	}
	// Reserve time inside the invocation deadline for cancellation, exact-ID
	// refresh and terminal publication. WaitContext can return nil before final
	// status on the SDK's no-listener path, so it remains only a wait hint.
	waitDeadline := time.Now().Add(75 * time.Minute)
	if end, ok := ctx.Deadline(); ok {
		reserve := min(30*time.Second, time.Until(end)/6)
		waitDeadline = end.Add(-reserve)
	}
	waitContext, stopWait := context.WithDeadline(ctx, waitDeadline)
	waited := operation.WaitContext(waitContext)
	if waitContext.Err() != nil {
		_ = operation.Cancel()
		waited = errors.Join(waited, context.Cause(waitContext))
	}
	stopWait()
	remote := operation.Get()
	// Some official-client paths have no listener and can return from Wait while
	// the operation is still running. Retain custody and refresh that exact ID.
	end := time.Now().Add(30 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(end) {
		end = deadline
	}
	for !remote.StatusCode.IsFinal() {
		if time.Now().After(end) {
			return errors.Join(recordErr, waited, errors.New("native operation drain deadline"))
		}
		if ctx.Err() != nil {
			_ = operation.Cancel()
		}
		if err := operation.Refresh(); err != nil {
			return errors.Join(recordErr, waited, err)
		}
		remote = operation.Get()
		if err := nativeOperationIdentity(remote, value); err != nil {
			return errors.Join(recordErr, waited, err)
		}
		if !remote.StatusCode.IsFinal() {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := nativeOperationIdentity(remote, value); err != nil {
		return errors.Join(recordErr, waited, err)
	}
	value.Status = int(remote.StatusCode)
	if !nativeFinalStatus(remote.StatusCode) {
		return errors.Join(recordErr, waited, errors.New("native operation terminal status unproved"))
	}
	terminalErr := observer.Terminal(value)
	if remote.StatusCode != api.Success && recordErr == nil && terminalErr == nil {
		return &ports.NativeOperationFailure{Operation: value, Cause: errors.Join(waited, fmt.Errorf("remote operation status %d", remote.StatusCode))}
	}
	return errors.Join(recordErr, waited, terminalErr)
}

// NativeOperationState observes only an already accepted exact SDK operation.
// A missing operation, changed server/resource or nonfinal result remains
// unresolved. It never sends a new mutation or treats resource drift as drain.
func (client *Client) NativeOperationState(ctx context.Context, expected ports.NativeOperation) (ports.NativeOperation, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return ports.NativeOperation{}, err
	}
	identity, err := nativeOperation(server, expected.Kind, expected.Project, expected.Resource)
	if err != nil || identity.Server != expected.Server || expected.ID == "" {
		return ports.NativeOperation{}, errors.New("native operation server binding")
	}
	remote, _, err := server.UseProject(expected.Project).GetOperation(expected.ID)
	if err != nil {
		return ports.NativeOperation{}, err
	}
	if remote == nil {
		return ports.NativeOperation{}, errors.New("native operation absent")
	}
	if err := nativeOperationIdentity(*remote, expected); err != nil {
		return ports.NativeOperation{}, err
	}
	identity.ID, identity.Status = remote.ID, int(remote.StatusCode)
	return identity, nil
}

func nativeOperation(server incus.InstanceServer, kind, project, resource string) (ports.NativeOperation, error) {
	info, err := server.GetConnectionInfo()
	if err != nil {
		return ports.NativeOperation{}, err
	}
	raw, _ := json.Marshal(struct {
		Protocol, Socket, Certificate string
		Addresses                     []string
	}{info.Protocol, info.SocketPath, info.Certificate, info.Addresses})
	sum := sha256.Sum256(raw)
	return ports.NativeOperation{Kind: kind, Server: hex.EncodeToString(sum[:]), Project: project, Resource: resource}, nil
}

func beforeNative(ctx context.Context, server incus.InstanceServer, kind, project, resource string) error {
	observer := ports.NativeOperationObserverFrom(ctx)
	if observer == nil {
		return nil
	}
	value, err := nativeOperation(server, kind, project, resource)
	if err != nil {
		return err
	}
	return observer.Before(value)
}

func nativeFinalStatus(status api.StatusCode) bool {
	return status == api.Success || status == api.Failure || status == api.Cancelled
}

func nativeOperationIdentity(remote api.Operation, expected ports.NativeOperation) error {
	if expected.ID == "" || remote.ID != expected.ID {
		return errors.New("native operation ID unavailable or changed")
	}
	resources := remote.Resources["instances"]
	if len(resources) == 0 {
		resources = remote.Resources["containers"]
	}
	if len(resources) != 1 {
		return errors.New("native operation resource identity unavailable")
	}
	resource, err := url.Parse(resources[0])
	if err != nil || resource.IsAbs() || resource.Host != "" || resource.Fragment != "" || (resource.Path != "/1.0/instances/"+expected.Resource && resource.Path != "/1.0/containers/"+expected.Resource) {
		return errors.New("native operation resource changed")
	}
	project := resource.Query().Get("project")
	if project != "" && project != expected.Project || strings.ContainsAny(expected.Project+expected.Resource, "/\x00\r\n") || expected.Project == "" || expected.Resource == "" {
		return errors.New("native operation project changed")
	}
	return nil
}
