package reconcileruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAIObserverConvergenceTracksPackageSelectionAndRoute(t *testing.T) {
	bin := t.TempDir()
	testkit.WriteFile(t, filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin)
	hook := filepath.Join(t.TempDir(), "provision.sh")
	if err := os.WriteFile(hook, []byte("hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Yard: domain.Context{YardKind: domain.YardContainer}, Environment: []string{
		"CODING_TOOL_INTEGRATIONS=codex aiobserver", "AGENT_aiobserver_PROVISION=" + hook, "AI_OBSERVER_HOST_PORT=22222",
	}}
	instance := ports.InstanceInfo{Config: map[string]string{
		"user.subyard.ai_observer_provision": "2383e6e7f97323f296b550b96ab77fec203b11775e5283689ffb0045277104d5",
		"user.subyard.ai_observer_proxy":     "v1:22222",
	}, Devices: map[string]map[string]string{"ai-observer": {
		"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:22222", "connect": "tcp:127.0.0.1:8080",
	}}}
	assert := func(want bool) {
		t.Helper()
		got, err := runtime.aiObserverConverged(context.Background(), instance)
		if err != nil || got != want {
			t.Fatalf("converged = %v, %v; want %v", got, err, want)
		}
	}
	assert(true)
	// A matching new route still needs the guest's allowed browser origin updated.
	runtime.Environment[2] = "AI_OBSERVER_HOST_PORT=22223"
	instance.Config["user.subyard.ai_observer_proxy"] = "v1:22223"
	instance.Devices["ai-observer"]["listen"] = "tcp:127.0.0.1:22223"
	assert(false)
	runtime.Environment[2] = "AI_OBSERVER_HOST_PORT=22222"
	instance.Config["user.subyard.ai_observer_proxy"] = "v1:22222"
	instance.Devices["ai-observer"]["listen"] = "tcp:127.0.0.1:22222"
	runtime.Environment[0] = "CODING_TOOL_INTEGRATIONS=claude codex aiobserver"
	assert(false)
	runtime.Environment[0] = "CODING_TOOL_INTEGRATIONS=codex aiobserver"
	runtime.Environment = append(runtime.Environment, "HOST_BASE=/new/mount/source")
	assert(false)
	runtime.Environment = runtime.Environment[:len(runtime.Environment)-1]
	instance.Devices["ai-observer"]["listen"] = "tcp:0.0.0.0:22222"
	assert(false)
	instance.Devices["ai-observer"]["listen"] = "tcp:127.0.0.1:22222"
	if err := os.WriteFile(hook, []byte("new package\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	assert(false)
	if err := os.WriteFile(hook, []byte("hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime.Yard.YardKind = domain.YardVM
	delete(instance.Devices, "ai-observer")
	delete(instance.Config, "user.subyard.ai_observer_proxy")
	assert(true)
	runtime.Environment[0] = "CODING_TOOL_INTEGRATIONS=codex"
	assert(false)
	delete(instance.Config, "user.subyard.ai_observer_provision")
	assert(true)
}

func TestAIObserverProvisionIdentityTracksDashboardOrigin(t *testing.T) {
	hook := filepath.Join(testkit.TempDir(t), "provision.sh")
	testkit.WriteFile(t, hook, []byte("fixture\n"), 0o755)
	runtime := Runtime{Yard: domain.Context{YardKind: domain.YardVM}, Environment: []string{
		"CODING_TOOL_INTEGRATIONS=aiobserver", "AGENT_aiobserver_PROVISION=" + hook, "AI_OBSERVER_HOST_PORT=18080",
	}}
	if got := runtime.aiObserverFrontendURL(context.Background()); got != "http://127.0.0.1:18080" {
		t.Fatalf("VM browser origin = %q", got)
	}
	identities := map[string]bool{}
	for _, origin := range []string{
		"http://127.0.0.1:18080", "http://100.100.100.42:18080",
		"http://100.100.100.43:18080", "http://100.100.100.43:18081",
	} {
		identity, err := runtime.aiObserverProvisionIdentity(origin)
		if err != nil || identities[identity] {
			t.Fatalf("origin %q did not get a distinct identity: %v", origin, err)
		}
		identities[identity] = true
	}
}
