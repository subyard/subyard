package ownerpublication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
)

func handoffFixture(t *testing.T) (Handoff, ports.InstanceInfo) {
	t.Helper()
	cfg := map[string]string{"volatile.uuid": "original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "running", previewroute.Key: "v1:100.127.255.254:23456"}
	receipt, _ := json.Marshal(Receipt{Version: 1, Ownership: cfg[previewroute.Key], Device: previewroute.Device("100.127.255.254", "23456")})
	cfg[Prefix+previewroute.DeviceName] = string(receipt)
	i := ports.InstanceInfo{Project: "p", Name: "yard", Type: domain.YardVM, Status: "Running", Config: maps.Clone(cfg), LocalConfig: maps.Clone(cfg), Devices: map[string]map[string]string{}, LocalDevices: map[string]map[string]string{}}
	c := Consumer{EngineDigest: strings.Repeat("a", 64), UnitDigest: strings.Repeat("b", 64), ContractsDigest: strings.Repeat("c", 64), FilesDigest: strings.Repeat("d", 64), Generations: []ports.NativeResourceGeneration{{Role: "engine", Value: "0"}, {Role: "service", Value: "0"}, {Role: "publication", Value: "0"}}}
	h, err := NewHandoff(OriginalTarget{Project: "p", Name: "yard", UUID: "original", Desired: domain.PowerRunning, Kind: domain.YardVM}, previewroute.DeviceName, strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("a", 64), c, i, CoreContracts())
	if err != nil {
		t.Fatal(err)
	}
	return h, i
}
func TestHandoffClosedIdentityAndOriginalReceipt(t *testing.T) {
	h, i := handoffFixture(t)
	if _, e := DecodeHandoff(h.Encode()); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"unknown", "duplicate", "space", "intent", "source", "ack", "receipt", "consumer"} {
		t.Run(mode, func(t *testing.T) {
			v := h
			raw := ""
			switch mode {
			case "unknown":
				raw = strings.TrimSuffix(h.Encode(), "}") + `,"foreign":1}`
			case "duplicate":
				raw = strings.TrimSuffix(h.Encode(), "}") + `,"ack":""}`
			case "space":
				raw = " " + h.Encode()
			case "intent":
				v.Intent = strings.Repeat("b", 64)
			case "source":
				v.Recipe = strings.Repeat("c", 64)
			case "ack":
				v.Ack = strings.Repeat("d", 64)
			case "receipt":
				v.Receipt += " "
			case "consumer":
				v.Consumer.EngineDigest = strings.Repeat("f", 64)
			}
			if raw == "" {
				raw = v.Encode()
			}
			if _, e := DecodeHandoff(raw); e == nil {
				t.Fatal("altered delivery admitted")
			}
		})
	}
	for _, mode := range []string{"replacement", "stop", "retired", "receipt-gone", "restored", "effective-conflict", "marker"} {
		t.Run(mode, func(t *testing.T) {
			v := i
			v.Config = maps.Clone(i.Config)
			v.LocalConfig = maps.Clone(i.LocalConfig)
			v.LocalDevices = map[string]map[string]string{}
			v.Devices = map[string]map[string]string{}
			contracts := CoreContracts()
			switch mode {
			case "replacement":
				v.LocalConfig["volatile.uuid"] = "foreign"
			case "stop":
				v.Status = "Stopped"
			case "retired":
				contracts[0].CleanupOnly = true
			case "receipt-gone", "restored", "effective-conflict":
				delete(v.LocalConfig, h.ReceiptKey)
				delete(v.Config, h.ReceiptKey)
				if mode != "receipt-gone" {
					device := previewroute.Device("100.127.255.254", "23456")
					v.LocalDevices[previewroute.DeviceName] = device
					v.Devices[previewroute.DeviceName] = maps.Clone(device)
					if mode == "effective-conflict" {
						v.Devices[previewroute.DeviceName]["listen"] = "tcp:192.0.2.1:1"
					}
				}
			case "marker":
				v.LocalConfig[previewroute.Key] = "foreign"
			}
			e := h.Check(v, contracts, true)
			if (e == nil) != (mode == "restored") {
				t.Fatal(mode, e)
			}
		})
	}
}
func TestWorkerAcknowledgesBeforeHeldNetworkAndSkipsTemporaryRestop(t *testing.T) {
	h, i := handoffFixture(t)
	i.LocalConfig[h.Key()] = h.Encode()
	i.Config[h.Key()] = h.Encode()
	stopped := i
	stopped.Name = "legacy"
	stopped.Status = "Stopped"
	ack := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	restores := 0
	w := PublicationWorker{Contracts: CoreContracts(), Inventory: func(context.Context) ([]ports.InstanceInfo, error) { return []ports.InstanceInfo{i, stopped}, nil }, Consumer: func(context.Context, Consumer) error { return nil }, Ack: func(_ context.Context, v Handoff, before, after string) error {
		if before != h.Encode() || v.Ack != h.ID {
			t.Error("inexact ACK")
		}
		close(ack)
		return nil
	}, Restore: func(context.Context, ports.InstanceInfo) ([]string, error) {
		restores++
		<-release
		return []string{"waiting"}, nil
	}}
	go func() {
		waiting, e := w.Run(context.Background())
		if !waiting && e == nil {
			e = errors.New("retry lost")
		}
		done <- e
	}()
	select {
	case <-ack:
	case <-time.After(time.Second):
		t.Fatal("ACK blocked behind network lock")
	}
	close(release)
	if e := <-done; e != nil || restores != 1 {
		t.Fatal(e, restores)
	}
}
func TestDeliveryCoalescedStartsAndExactCleanup(t *testing.T) {
	for _, mode := range []string{"ack", "coalesced", "changed", "replacement", "consumer", "loss"} {
		t.Run(mode, func(t *testing.T) {
			h, i := handoffFixture(t)
			observer := &publicationWaitObserver{}
			timeout := 500 * time.Millisecond
			if mode == "coalesced" || mode == "consumer" {
				timeout = 3 * time.Second
			}
			ctx, cancel := context.WithTimeout(ports.WithNativeOperationObserver(context.Background(), observer), timeout)
			defer cancel()
			starts := 0
			cleaned := false
			check := func(context.Context) error {
				if mode == "consumer" && starts > 0 {
					return domain.ErrPlanStale
				}
				return nil
			}
			err := Deliver(ctx, []Handoff{h}, func(context.Context) (ports.InstanceInfo, error) {
				if mode == "replacement" && starts > 0 {
					return i, domain.ErrPlanStale
				}
				return i, nil
			}, check, func(_ context.Context, _ Handoff, before, after string) error {
				if i.LocalConfig[h.Key()] != before {
					return domain.ErrPlanStale
				}
				if after == "" {
					v, e := DecodeHandoff(before)
					if e != nil || v.Ack != h.ID {
						t.Fatal("cleanup without ACK")
					}
					cleaned = true
				}
				i.LocalConfig[h.Key()] = after
				return nil
			}, func(context.Context) error {
				starts++
				if mode == "changed" {
					i.LocalConfig[h.Key()] = "foreign"
				}
				if mode == "ack" || mode == "coalesced" && starts == 2 {
					v := h
					v.Ack = v.ID
					i.LocalConfig[h.Key()] = v.Encode()
				}
				return nil
			})
			success := mode == "ack" || mode == "coalesced"
			if (err == nil) != success || cleaned != success || observer.unknown != (mode == "loss") {
				t.Fatal(mode, err, cleaned, observer.unknown)
			}
			if mode == "coalesced" && starts != 2 {
				t.Fatal(starts)
			}
		})
	}
}

type publicationWaitObserver struct {
	unknown bool
	facts   int
}

func (o *publicationWaitObserver) Before(ports.NativeOperation) error   { o.facts++; return nil }
func (o *publicationWaitObserver) Accepted(ports.NativeOperation) error { o.facts++; return nil }
func (o *publicationWaitObserver) Terminal(ports.NativeOperation) error { o.facts++; return nil }
func (o *publicationWaitObserver) Uncertain()                           { o.unknown = true }
func (o *publicationWaitObserver) CommandStarted(string, string) (uint64, error) {
	o.facts++
	return 1, nil
}
func (o *publicationWaitObserver) CommandTerminal(uint64) error { o.facts++; return nil }

func TestWorkerPreservesStaleOrForeignDelivery(t *testing.T) {
	for _, mode := range []string{"malformed", "old-ack", "stopped", "retired", "consumer-replaced", "receipt-gone"} {
		t.Run(mode, func(t *testing.T) {
			h, i := handoffFixture(t)
			contracts := CoreContracts()
			raw := h.Encode()
			switch mode {
			case "malformed":
				raw = "foreign"
			case "old-ack":
				h.Ack = strings.Repeat("e", 64)
				raw = h.Encode()
			case "stopped":
				i.Status = "Stopped"
			case "retired":
				contracts[0].CleanupOnly = true
			case "receipt-gone":
				delete(i.LocalConfig, h.ReceiptKey)
				delete(i.Config, h.ReceiptKey)
			}
			i.LocalConfig[h.Key()] = raw
			i.Config[h.Key()] = raw
			acks := 0
			worker := PublicationWorker{Contracts: contracts, Inventory: func(context.Context) ([]ports.InstanceInfo, error) { return []ports.InstanceInfo{i}, nil }, Consumer: func(context.Context, Consumer) error {
				if mode == "consumer-replaced" {
					return domain.ErrPlanStale
				}
				return nil
			}, Ack: func(context.Context, Handoff, string, string) error { acks++; return nil }, Restore: func(context.Context, ports.InstanceInfo) ([]string, error) { return nil, nil }}
			if _, e := worker.Run(context.Background()); e != nil || acks != 0 || i.LocalConfig[h.Key()] != raw {
				t.Fatal("stale sidecar consumed", e, acks)
			}
		})
	}
}

func TestDeliveryBatchesReceiptsAcrossOlderWorkerTurnover(t *testing.T) {
	_, instance := handoffFixture(t)
	instance.Config = map[string]string{"volatile.uuid": "original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "running"}
	instance.LocalConfig = maps.Clone(instance.Config)
	contracts := []Contract{}
	requests := []Handoff{}
	for n := 0; n < 8; n++ {
		contract := CoreContracts()[0]
		contract.Device = fmt.Sprintf("optional-%d", n)
		contract.Key = fmt.Sprintf("user.fixture.%d", n)
		contracts = append(contracts, contract)
		marker := fmt.Sprintf("v1:100.127.255.254:%d", 23456+n)
		device := previewroute.Device("100.127.255.254", fmt.Sprint(23456+n))
		raw, _ := json.Marshal(Receipt{Version: 1, Ownership: marker, Device: device})
		instance.Config[contract.Key] = marker
		instance.LocalConfig[contract.Key] = marker
		instance.Config[Prefix+contract.Device] = string(raw)
		instance.LocalConfig[Prefix+contract.Device] = string(raw)
	}
	first, _ := handoffFixture(t)
	for _, c := range contracts {
		h, e := NewHandoff(OriginalTarget{Project: "p", Name: "yard", UUID: "original", Desired: domain.PowerRunning, Kind: domain.YardVM}, c.Device, first.Invocation, first.Intent, first.Recipe, first.Consumer, instance, contracts)
		if e != nil {
			t.Fatal(e)
		}
		requests = append(requests, h)
	}
	var mu sync.Mutex
	snapshot := func(context.Context) ([]ports.InstanceInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		i := instance
		i.LocalConfig = maps.Clone(instance.LocalConfig)
		i.Config = maps.Clone(instance.Config)
		return []ports.InstanceInfo{i}, nil
	}
	inspect := func(ctx context.Context) (ports.InstanceInfo, error) {
		values, _ := snapshot(ctx)
		for _, h := range requests {
			if e := h.Check(values[0], contracts, true); e != nil {
				return values[0], e
			}
		}
		return values[0], nil
	}
	oldBlocked := make(chan struct{})
	turnover := make(chan struct{})
	oldDone := make(chan error, 1)
	old := PublicationWorker{Contracts: contracts, Inventory: snapshot, Consumer: func(context.Context, Consumer) error { return nil }, Ack: func(context.Context, Handoff, string, string) error {
		return errors.New("old inventory gained a new request")
	}, Restore: func(context.Context, ports.InstanceInfo) ([]string, error) {
		close(oldBlocked)
		<-turnover
		return []string{"waiting"}, nil
	}}
	go func() { _, e := old.Run(context.Background()); oldDone <- e }()
	<-oldBlocked
	freshDone := make(chan error, 1)
	freshRestore := make(chan struct{})
	allAcked := make(chan struct{})
	acks := 0
	fresh := PublicationWorker{Contracts: contracts, Inventory: snapshot, Consumer: func(context.Context, Consumer) error { return nil }, Ack: func(_ context.Context, h Handoff, before, after string) error {
		mu.Lock()
		defer mu.Unlock()
		if instance.LocalConfig[h.Key()] != before {
			return domain.ErrPlanStale
		}
		instance.LocalConfig[h.Key()] = after
		instance.Config[h.Key()] = after
		acks++
		if acks == len(requests) {
			close(allAcked)
		}
		return nil
	}, Restore: func(context.Context, ports.InstanceInfo) ([]string, error) {
		<-freshRestore
		return []string{"waiting"}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	starts, published, removed := 0, 0, 0
	err := Deliver(ctx, requests, inspect, func(context.Context) error { return nil }, func(_ context.Context, h Handoff, before, after string) error {
		mu.Lock()
		defer mu.Unlock()
		if instance.LocalConfig[h.Key()] != before {
			return domain.ErrPlanStale
		}
		instance.LocalConfig[h.Key()] = after
		instance.Config[h.Key()] = after
		if after == "" {
			removed++
		} else {
			published++
		}
		return nil
	}, func(context.Context) error {
		starts++
		if published != len(requests) {
			t.Error("started before complete request delivery")
		}
		if starts == 2 {
			close(turnover)
			if e := <-oldDone; e != nil {
				return e
			}
			go func() { _, e := fresh.Run(ctx); freshDone <- e }()
			select {
			case <-allAcked:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	close(freshRestore)
	if err != nil {
		t.Fatal(err)
	}
	if e := <-freshDone; e != nil {
		t.Fatal(e)
	}
	if starts != 2 || published != 8 || removed != 8 || acks != 8 {
		t.Fatal("batch multiplied Start budget or lost exact cleanup", starts, published, removed, acks)
	}
}

func TestBatchDeliveryPartialBudgetAndCancellationKeepDurableReceipts(t *testing.T) {
	for _, mode := range []string{"budget", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			h, i := handoffFixture(t)
			second := h
			second.ReceiptKey = Prefix + "second"
			second.ID = second.identity()
			// The closed request grammar is valid; no new restore authority is created.
			observer := &publicationWaitObserver{}
			ctx, cancel := context.WithCancel(ports.WithNativeOperationObserver(context.Background(), observer))
			defer cancel()
			calls, starts := 0, 0
			err := Deliver(ctx, []Handoff{h, second}, func(context.Context) (ports.InstanceInfo, error) { return i, nil }, func(context.Context) error { return nil }, func(call context.Context, v Handoff, before, after string) error {
				calls++
				if calls == 2 {
					if mode == "cancel" {
						cancel()
						return call.Err()
					}
					return errors.New("native invocation budget exhausted")
				}
				i.LocalConfig[v.Key()] = after
				return nil
			}, func(context.Context) error { starts++; return nil })
			if err == nil || calls != 2 || starts != 0 || i.LocalConfig[h.Key()] != h.Encode() || i.LocalConfig[h.ReceiptKey] != h.Receipt || observer.unknown != (mode == "cancel") {
				t.Fatal("partial delivery claimed completion or lost durable state", err, calls, starts, observer.unknown)
			}
		})
	}
}
