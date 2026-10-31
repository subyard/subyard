package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAcquireSessionCancellationDrainsNativeWork(t *testing.T) {
	for _, mode := range []string{"disconnect", "release"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(t)
			store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1}
			ready := filepath.Join(testkit.TempDir(t), "native-ready")
			input, control := io.Pipe()
			defer input.Close()
			defer control.Close()
			output, replies := io.Pipe()
			defer output.Close()
			defer replies.Close()
			runtime := &Runtime{Config: cfg, finishDrain: func(ctx context.Context, store LeaseStore, slot LeaseSlot) error {
				if ctx.Err() != nil {
					return errors.New("cleanup inherited cancelled context")
				}
				return store.FinishDrain(slot.SlotID, nil)
			}}
			facade := Facade{Store: store, Input: input, Output: replies,
				OnAcquire: func(ctx context.Context, grant LeaseGrant, _ string) (LeaseGrant, error) {
					err := withFileLock(runtime.slotLifecycleLock(grant.SlotID), func() error {
						_, _, err := (ProcessRunner{}).Run(ctx, "/bin/sh", []string{"-c", "touch \"$1\"; exec sleep 60", "fixture", ready}, nil, nil)
						return err
					})
					return grant, err
				},
				OnRelease: func(ctx context.Context, grant LeaseGrant) error { return runtime.ReleaseSlot(ctx, store, grant) },
			}
			key := strings.Fields(fixturePublicKey(t))
			done := make(chan error, 1)
			go func() {
				done <- facade.Run("acquire-v4 subyard-pair client SHA256:key yard Project run tests " + key[0] + " " + key[1] + " slot-001")
				replies.Close()
			}()
			var reservation facadeResponse
			decoder := json.NewDecoder(output)
			if err := decoder.Decode(&reservation); err != nil || reservation.Grant == nil {
				t.Fatalf("reservation: %v", err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("native child did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			released := make(chan error, 1)
			if mode == "disconnect" {
				control.Close()
			} else {
				wrong := *reservation.Grant
				wrong.Capability += "wrong"
				if err := runtime.ReleaseSlot(context.Background(), store, wrong); !errors.Is(err, ErrLeaseLost) {
					t.Fatalf("foreign release: %v", err)
				}
				go func() { released <- runtime.ReleaseSlot(context.Background(), store, *reservation.Grant) }()
			}
			// Drain any terminal diagnostic so the pipe cannot keep cancellation blocked.
			go io.Copy(io.Discard, output)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("cancel waited for provisioning")
			}
			if mode == "release" {
				select {
				case err := <-released:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("release did not finish")
				}
			}
			pool, err := store.Status()
			if err != nil || pool.Slots[0].State != SlotAvailable || pool.Slots[0].Reserved || pool.Slots[0].AcquireOwner != nil {
				t.Fatalf("cancel retained allocation: %#v %v", pool, err)
			}
		})
	}
}

func TestAcquireOwnerDeathFencesBeforeDeadlineAndBootReuse(t *testing.T) {
	for _, mode := range []string{"process", "boot"} {
		t.Run(mode, func(t *testing.T) {
			owner, err := currentAcquireOwner()
			if err != nil {
				t.Fatal(err)
			}
			store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1, AcquireOwner: &owner}
			spec, _ := fixtureConfig(t).EnvironmentSpec(EnvironmentPair)
			grant, err := store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "run", "tests", "slot-001")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error {
				if mode == "process" {
					slot.AcquireOwner.Start += "1"
				} else {
					slot.AcquireOwner.Boot += "other"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			pool, err := store.Status()
			if err != nil || pool.Slots[0].State != SlotDraining || !time.Now().Before(pool.Slots[0].ExpiresAt) {
				t.Fatalf("orphan remained until TTL: %#v %v", pool, err)
			}
		})
	}
}

type acquireResponseWriter func([]byte) (int, error)

func (write acquireResponseWriter) Write(body []byte) (int, error) { return write(body) }

func syntheticRelease(store LeaseStore) func(context.Context, LeaseGrant) error {
	return func(ctx context.Context, grant LeaseGrant) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := store.BeginDrain(grant); err != nil {
			return err
		}
		return store.FinishDrain(grant.SlotID, nil)
	}
}

func TestAcquireSessionReplyLossAndAcknowledgedHandoff(t *testing.T) {
	for _, mode := range []string{"reservation-lost", "ready-lost", "disconnect-before-ack", "ack"} {
		t.Run(mode, func(t *testing.T) {
			store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1}
			input, control := io.Pipe()
			defer input.Close()
			defer control.Close()
			replies := make(chan facadeResponse, 2)
			writes := 0
			var reserved *LeaseGrant
			facade := Facade{Store: store, Input: input,
				Output: acquireResponseWriter(func(body []byte) (int, error) {
					writes++
					if (mode == "reservation-lost" && writes == 1) || (mode == "ready-lost" && writes == 2) {
						return 0, io.ErrClosedPipe
					}
					var response facadeResponse
					if err := json.Unmarshal(body, &response); err != nil {
						return 0, err
					}
					if response.Status == "reserved" {
						reserved = response.Grant
					}
					replies <- response
					return len(body), nil
				}),
				OnAcquire: func(ctx context.Context, grant LeaseGrant, _ string) (LeaseGrant, error) {
					if writes != 1 || reserved == nil || reserved.LeaseID != grant.LeaseID || reserved.Capability != grant.Capability || reserved.LeaseEpoch != grant.LeaseEpoch {
						t.Fatal("provisioning preceded the usable reservation grant")
					}
					if err := ctx.Err(); err != nil {
						return grant, err
					}
					expires, err := store.MarkHeld(grant)
					grant.ExpiresAt = expires
					return grant, err
				},
				OnRelease: syntheticRelease(store),
			}
			done := make(chan error, 1)
			key := strings.Fields(fixturePublicKey(t))
			go func() {
				done <- facade.Run("acquire-v4 subyard-pair client SHA256:key yard Project run tests " + key[0] + " " + key[1] + " slot-001")
			}()
			if mode == "ack" || mode == "disconnect-before-ack" {
				for _, status := range []string{"reserved", "ok"} {
					select {
					case response := <-replies:
						if response.Status != status {
							t.Fatalf("wrong protocol order: %s", response.Status)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("missing reply")
					}
				}
				if mode == "ack" {
					if _, err := io.WriteString(control, "ack\n"); err != nil {
						t.Fatal(err)
					}
				} else {
					control.Close()
				}
			}
			select {
			case err := <-done:
				if mode == "ack" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("session did not finish")
			}
			pool, err := store.Status()
			if err != nil {
				t.Fatal(err)
			}
			want := SlotAvailable
			if mode == "ack" {
				want = SlotHeld
			}
			if pool.Slots[0].State != want || pool.Slots[0].AcquireOwner != nil {
				t.Fatalf("reply outcome retained wrong ownership: %#v", pool.Slots[0])
			}
		})
	}
}

func TestAcquireSessionRejectsInvalidAndPrematureFrames(t *testing.T) {
	for _, frame := range []string{"ack\n", "unexpected\n", strings.Repeat("x", 64) + "\n"} {
		t.Run(strings.TrimSpace(frame[:min(len(frame), 10)]), func(t *testing.T) {
			store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1}
			input, control := io.Pipe()
			defer input.Close()
			defer control.Close()
			output, response := io.Pipe()
			defer output.Close()
			defer response.Close()
			facade := Facade{Store: store, Input: input, Output: response,
				OnAcquire: func(ctx context.Context, grant LeaseGrant, _ string) (LeaseGrant, error) {
					<-ctx.Done()
					return grant, context.Cause(ctx)
				},
				OnRelease: syntheticRelease(store),
			}
			done := make(chan error, 1)
			key := strings.Fields(fixturePublicKey(t))
			go func() {
				done <- facade.Run("acquire-v4 subyard-pair client SHA256:key yard Project run tests " + key[0] + " " + key[1] + " slot-001")
				response.Close()
			}()
			var reserved facadeResponse
			if err := json.NewDecoder(output).Decode(&reserved); err != nil || reserved.Status != "reserved" {
				t.Fatalf("reservation: %v", err)
			}
			go io.Copy(io.Discard, output)
			// A rejected oversized frame need not be consumed in full by the server.
			go func() { _, _ = io.WriteString(control, frame) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("invalid control frame retained session")
			}
			pool, err := store.Status()
			if err != nil || pool.Slots[0].State != SlotAvailable {
				t.Fatalf("invalid-frame cleanup: %#v %v", pool, err)
			}
		})
	}
}

func TestReleaseWaitingForProvisioningCannotDrainSuccessor(t *testing.T) {
	cfg := fixtureConfig(t)
	store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1}
	original, err := store.AcquireSlot("client", "SHA256:key", "", "tests", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{Config: cfg, finishDrain: func(context.Context, LeaseStore, LeaseSlot) error {
		return errors.New("stale release reached physical cleanup")
	}}
	locked, unlock := make(chan struct{}), make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- withFileLock(runtime.slotLifecycleLock(original.SlotID), func() error { close(locked); <-unlock; return nil })
	}()
	<-locked
	released := make(chan error, 1)
	go func() { released <- runtime.ReleaseSlot(context.Background(), store, original) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pool, err := store.Status()
		if err != nil {
			t.Fatal(err)
		}
		if pool.Slots[0].State == SlotDraining {
			break
		}
		if time.Now().After(deadline) {
			close(unlock)
			t.Fatal("release did not fence before the lifecycle lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Model another completed drain and a successor winning the lifecycle lock.
	if err := store.FinishDrain(original.SlotID, nil); err != nil {
		t.Fatal(err)
	}
	successor, err := store.AcquireSlot("successor", "SHA256:key", "", "tests", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	close(unlock)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-released; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale release result: %v", err)
	}
	pool, err := store.Status()
	if err != nil || pool.Slots[0].LeaseID != successor.LeaseID || pool.Slots[0].State != SlotProvisioning {
		t.Fatalf("successor harmed: %#v %v", pool, err)
	}
}

func TestAcquireCancellationWinsProvisioningFailure(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1}
	var output bytes.Buffer
	// The callback notices DRAINING before the asynchronous context watcher ticks.
	facade := Facade{Store: store, Input: strings.NewReader("keepalive\n"), Output: &output,
		OnAcquire: func(ctx context.Context, grant LeaseGrant, _ string) (LeaseGrant, error) {
			if err := store.BeginDrain(grant); err != nil {
				return grant, err
			}
			if err := store.Quarantine(grant, errors.New("late provisioning error")); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("quarantine overwrote cancellation: %v", err)
			}
			return grant, errors.New("provisioning noticed cancellation")
		},
		OnRelease: syntheticRelease(store),
	}
	key := strings.Fields(fixturePublicKey(t))
	if err := facade.Run("acquire-v4 subyard-pair client SHA256:key yard Project run tests " + key[0] + " " + key[1] + " slot-001"); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Status()
	if err != nil || pool.Slots[0].State != SlotAvailable {
		t.Fatalf("cancel did not finish cleanup: %#v %v", pool, err)
	}
}
