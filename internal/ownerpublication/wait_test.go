package ownerpublication

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// Preserve the original wait contract at its current boundaries: original
// identity/receipts fence delivery, while the worker owns physical restoration.
func TestWaitOriginalPublicationStateAndReadiness(t *testing.T) {
	for _, mode := range []string{"delay", "stopped", "replacement", "intent", "unmanaged", "uninitialized", "unexpected-stop", "invalid-receipt", "unknown-receipt", "deadline", "cancel", "apply-error", "host-error"} {
		t.Run(mode, func(t *testing.T) {
			h, instance := handoffFixture(t)
			switch mode {
			case "stopped":
				instance.Status = "Stopped"
				instance.Config["user.subyard.desired_power"] = "stopped"
				instance.LocalConfig["user.subyard.desired_power"] = "stopped"
			case "replacement":
				instance.Config["volatile.uuid"] = "foreign"
			case "intent":
				instance.LocalConfig["user.subyard.desired_power"] = "stopped"
			case "unmanaged":
				instance.LocalConfig["user.subyard.managed"] = "false"
			case "uninitialized":
				instance.LocalConfig["user.subyard.initialized"] = "false"
			case "unexpected-stop":
				instance.Status = "Stopped"
			case "invalid-receipt":
				instance.LocalConfig[h.ReceiptKey] = `{"version":1}`
			case "unknown-receipt":
				instance.LocalConfig[Prefix+"unknown"] = h.Receipt
			}
			beforeConfig, beforeLocal := maps.Clone(instance.Config), maps.Clone(instance.LocalConfig)
			observer := &publicationWaitObserver{}
			timeout := 3 * time.Second
			if mode == "deadline" {
				timeout = 40 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(ports.WithNativeOperationObserver(context.Background(), observer), timeout)
			defer cancel()
			calls, guards, exchanges, restores := 0, 0, 0, 0
			lastStartGuard, cleanupGuard := 0, 0
			failure := errors.New("original publication failure")
			var err error
			if mode == "stopped" || mode == "apply-error" {
				if mode == "stopped" {
					target := OriginalTarget{Project: h.Project, Name: h.Instance, UUID: h.UUID, Kind: h.Kind, Desired: domain.PowerStopped}
					if err := target.Check(instance); err != nil {
						t.Fatal("approved original stopped target rejected", err)
					}
				}
				worker := PublicationWorker{
					Contracts: CoreContracts(),
					Inventory: func(context.Context) ([]ports.InstanceInfo, error) { return []ports.InstanceInfo{instance}, nil },
					Consumer: func(context.Context, Consumer) error {
						t.Fatal("worker invented a delivery consumer")
						return nil
					},
					Ack: func(context.Context, Handoff, string, string) error {
						t.Fatal("worker invented a delivery acknowledgement")
						return nil
					},
					Restore: func(context.Context, ports.InstanceInfo) ([]string, error) {
						restores++
						return nil, failure
					},
				}
				var waiting bool
				waiting, err = worker.Run(ctx)
				if waiting || mode == "stopped" && restores != 0 || mode == "apply-error" && (restores != 1 || !errors.Is(err, failure)) {
					t.Fatal("stopped/failed restoration lost original state", waiting, restores, err)
				}
			} else {
				err = Deliver(ctx, []Handoff{h}, func(context.Context) (ports.InstanceInfo, error) {
					return instance, nil
				}, func(context.Context) error {
					guards++
					if mode == "host-error" {
						return failure
					}
					return h.Check(instance, CoreContracts(), true)
				}, func(_ context.Context, _ Handoff, before, after string) error {
					exchanges++
					if guards == 0 || instance.LocalConfig[h.Key()] != before || instance.Config[h.Key()] != before {
						t.Fatal("delivery lost its original guarded CAS")
					}
					if after == "" {
						ack, decodeErr := DecodeHandoff(before)
						if decodeErr != nil || ack.Ack != h.ID || guards <= lastStartGuard {
							t.Fatal("cleanup preceded exact ACK and fresh guard", decodeErr)
						}
						cleanupGuard = guards
					}
					instance.LocalConfig[h.Key()], instance.Config[h.Key()] = after, after
					return nil
				}, func(context.Context) error {
					calls++
					if guards <= lastStartGuard {
						t.Fatal("start was not freshly guarded")
					}
					lastStartGuard = guards
					if mode == "cancel" {
						cancel()
					}
					if mode == "delay" && calls == 2 {
						ack := h
						ack.Ack = ack.ID
						instance.LocalConfig[h.Key()], instance.Config[h.Key()] = ack.Encode(), ack.Encode()
					}
					return nil
				})
				if mode == "delay" {
					if calls != 2 || exchanges != 2 || cleanupGuard == 0 || guards <= cleanupGuard || instance.LocalConfig[h.Key()] != "" || instance.Config[h.Key()] != "" {
						t.Fatal("delayed delivery lost guard ordering or exact cleanup", calls, guards, exchanges)
					}
				} else if mode == "deadline" || mode == "cancel" {
					if calls != 1 || exchanges != 1 || instance.LocalConfig[h.Key()] != h.Encode() || instance.Config[h.Key()] != h.Encode() {
						t.Fatal("uncertain delivery lost original durable request", calls, exchanges)
					}
				} else if guards != 1 || calls != 0 || exchanges != 0 {
					t.Fatal("original refusal reached effects", guards, calls, exchanges)
				}
				if mode == "host-error" && !errors.Is(err, failure) {
					t.Fatal("original host failure was replaced", err)
				}
			}
			success := mode == "delay" || mode == "stopped"
			if (err == nil) != success {
				t.Fatal("original publication outcome changed", mode, err)
			}
			if observer.unknown != (mode == "deadline" || mode == "cancel") || observer.facts != 0 {
				t.Fatalf("invented native outcome: %+v", observer)
			}
			// Delivery may create/remove only its independent acknowledgement key;
			// neither a timeout nor a completed ACK changes listener authority.
			afterConfig, afterLocal := maps.Clone(instance.Config), maps.Clone(instance.LocalConfig)
			delete(afterConfig, h.Key())
			delete(afterLocal, h.Key())
			if !maps.Equal(beforeConfig, afterConfig) || !maps.Equal(beforeLocal, afterLocal) {
				t.Fatal("original publication ownership/configuration changed")
			}
		})
	}
}
