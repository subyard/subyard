package credentialruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConsumerStopHandlerFailsClosed(t *testing.T) {
	for _, item := range []struct{ name, body string }{
		{"exit", "exit 1"},
		{"oversized", "head -c 65537 /dev/zero"},
		{"cancel", "sleep 30 & wait"},
	} {
		t.Run(item.name, func(t *testing.T) {
			runtime := credentialFixture(t)
			path := filepath.Join(runtime.config.RepositoryRoot, "config", "profiles", "mapping", "stop.sh")
			writeCredentialFile(t, path, "#!/bin/sh\n"+item.body+"\n", 0o700)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := runtime.runConsumerStopHandler(ctx, "fixture-env", "demo"); err == nil {
				t.Fatal("unverified stop succeeded")
			}
		})
	}
	runtime := credentialFixture(t)
	if err := runtime.runConsumerStopHandler(context.Background(), "fixture-env", "demo"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ consumer, zone string }{{"none", "demo"}, {"missing", "demo"}, {"fixture-env", "../outside"}} {
		if _, err := runtime.consumerStopHandler(item.consumer, item.zone); err == nil {
			t.Fatalf("unsafe stop handler selected: %+v", item)
		}
	}
	path := filepath.Join(runtime.config.RepositoryRoot, "config", "profiles", "mapping", "stop.sh")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/true", path); err != nil {
		t.Fatal(err)
	}
	if err := runtime.runConsumerStopHandler(context.Background(), "fixture-env", "demo"); err == nil {
		t.Fatal("replaced symlink hook executed")
	}
}

func TestDynamicConsumerDetectionRejectsNestedOrEmptyZones(t *testing.T) {
	runtime := credentialFixture(t)
	for _, path := range []string{"mapped/.env", "mapped/nested/demo.env", "mapped/../outside.env"} {
		if consumer := runtime.detectConsumer(filepath.Join(runtime.config.ConsumerRoot, path)); consumer != "none" {
			t.Fatalf("detected unsafe consumer %s for %s", consumer, path)
		}
	}
}

func TestMoveRejectsUnverifiedStopBeforePublishing(t *testing.T) {
	runtime := credentialFixture(t)
	identity := installCredentialIdentity(t, runtime)
	head := credentialMetadata("host-fixture-000000000001-aaaaaaaa", identity.ActorID, 1)
	head.Exclusive, head.AuthorityHost = true, identity.ActorID
	head.AssignedYard, head.AssignmentEpoch = runtime.currentYardID(), 1
	writeCredentialRecord(t, runtime, sharedLedger, head)
	dispatcher := filepath.Join(runtime.config.RepositoryRoot, "dispatcher")
	writeCredentialFile(t, dispatcher, "#!/bin/sh\nexit 1\n", 0o700)
	runtime.config.Dispatcher = dispatcher
	runtime.config.Resolve = func(context.Context, string) (Target, error) {
		return Target{Name: "other", Transport: "local"}, nil
	}
	plan, err := runtime.planMove(context.Background(), head.CredentialID, "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.executeMove(context.Background(), plan); err == nil {
		t.Fatal("unverified old consumer permitted handoff")
	}
	_, current, err := runtime.singleHead(context.Background(), head.CredentialID)
	if err != nil || current.RevisionID != head.RevisionID || current.AssignedYard != head.AssignedYard || current.AssignmentEpoch != head.AssignmentEpoch {
		t.Fatalf("failed stop changed assignment: head=%+v err=%v", current, err)
	}
}

func TestConsumerStopHandlerIgnoresAmbientShellStartupCode(t *testing.T) {
	runtime := credentialFixture(t)
	path := filepath.Join(runtime.config.RepositoryRoot, "config", "profiles", "mapping", "stop.sh")
	writeCredentialFile(t, path, "#!/usr/bin/env bash\nexit 0\n", 0o700)
	startup := filepath.Join(runtime.config.RepositoryRoot, "startup.sh")
	writeCredentialFile(t, startup, "exit 1\n", 0o700)
	runtime.config.TargetEnvironment = []string{"PATH=/usr/bin:/bin", "BASH_ENV=" + startup}
	if err := runtime.runConsumerStopHandler(context.Background(), "fixture-env", "demo"); err != nil {
		t.Fatalf("ambient shell startup code replaced the shipped handler: %v", err)
	}
}
