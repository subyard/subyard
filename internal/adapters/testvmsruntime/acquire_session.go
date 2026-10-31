package testvmsruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// AcquireOwner exists only until the client acknowledges the ready grant.
// Persisting the exact worker identity lets the reaper recover a killed worker.
type AcquireOwner struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Boot  string `json:"boot"`
}

func currentAcquireOwner() (AcquireOwner, error) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return AcquireOwner{}, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return AcquireOwner{}, err
	}
	owner := AcquireOwner{PID: os.Getpid(), Start: brokerProcessStart(stat), Boot: strings.TrimSpace(string(boot))}
	if owner.Start == "" || owner.Boot == "" {
		return owner, errors.New("acquire process identity unavailable")
	}
	return owner, nil
}

func (owner AcquireOwner) alive() bool {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return err == nil && owner.PID > 1 && owner.Start != "" && owner.Boot == strings.TrimSpace(string(boot)) && brokerBridgeAlive(strconv.Itoa(owner.PID), owner.Start)
}

type acquireSession struct {
	facade    Facade
	grant     LeaseGrant
	context   context.Context
	cancel    context.CancelCauseFunc
	ready     chan struct{}
	ack       chan struct{}
	handedOff bool
}

func (facade Facade) newAcquireSession(grant LeaseGrant) *acquireSession {
	ctx, cancel := context.WithCancelCause(facade.Context)
	session := &acquireSession{facade: facade, grant: grant, context: ctx, cancel: cancel, ready: make(chan struct{}), ack: make(chan struct{})}
	input := facade.Input
	if input == nil {
		input = strings.NewReader("")
	}
	frames := make(chan string)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 32), 32)
		for scanner.Scan() {
			select {
			case frames <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				cancel(errors.New("acquire requester heartbeat lost"))
				return
			case frame, ok := <-frames:
				if !ok {
					cancel(io.EOF)
					return
				}
				if frame == "ack" {
					select {
					case <-session.ready:
						close(session.ack)
						return
					default:
						cancel(errors.New("premature acquire acknowledgement"))
						return
					}
				}
				if frame != "keepalive" {
					cancel(errors.New("invalid acquire control frame"))
					return
				}
				if !deadline.Stop() {
					select {
					case <-deadline.C:
					default:
					}
				}
				deadline.Reset(15 * time.Second)
			case <-ticker.C:
				slot, err := storeSlot(facade.Store, grant.SlotID)
				if err != nil || slot.LeaseID != grant.LeaseID || slot.LeaseEpoch != grant.LeaseEpoch || (slot.State != SlotProvisioning && slot.State != SlotHeld) {
					cancel(errors.New("acquire lease cancelled"))
					return
				}
			}
		}
	}()
	return session
}

func (session *acquireSession) handoff() error {
	select {
	case <-session.context.Done():
		return context.Cause(session.context)
	case <-session.ack:
	}
	if err := session.facade.Store.mutateOwned(session.grant, func(slot *LeaseSlot, _ time.Time) error {
		if slot.State != SlotHeld {
			return ErrLeaseLost
		}
		slot.AcquireOwner = nil
		return nil
	}); err != nil {
		return err
	}
	session.handedOff = true
	return nil
}

func (session *acquireSession) close() error {
	session.cancel(context.Canceled)
	if session.handedOff {
		return nil
	}
	slot, err := storeSlot(session.facade.Store, session.grant.SlotID)
	if err != nil {
		return err
	}
	if slot.LeaseEpoch != session.grant.LeaseEpoch || slot.State == SlotAvailable || slot.State == SlotQuarantined || slot.State == SlotRecovering {
		return nil
	}
	if session.facade.OnRelease == nil {
		return errors.New("acquire cleanup unavailable")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.context), 180*time.Second)
	defer cancel()
	if err := session.facade.OnRelease(ctx, session.grant); err != nil {
		return fmt.Errorf("acquire cleanup incomplete: %w", err)
	}
	return nil
}
