package reconcileruntime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestAIObserverConvergenceTracksPackageSelectionAndRoute(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "provision.sh")
	if err := os.WriteFile(hook, []byte("hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Yard: domain.Context{YardKind: domain.YardContainer}, Environment: []string{
		"CODING_TOOL_INTEGRATIONS=codex aiobserver", "AGENT_aiobserver_PROVISION=" + hook, "AI_OBSERVER_HOST_PORT=22222",
	}}
	instance := ports.InstanceInfo{Config: map[string]string{
		"user.subyard.ai_observer_provision": "0cd86dd6301cea63f02f2c93254cbdbc995788f54f99992ca93a79ac0ed14baa",
		"user.subyard.ai_observer_proxy":     "v1:22222",
	}, Devices: map[string]map[string]string{"ai-observer": {
		"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:22222", "connect": "tcp:127.0.0.1:8080",
	}}}
	assert := func(want bool) {
		t.Helper()
		got, err := runtime.aiObserverConverged(instance)
		if err != nil || got != want {
			t.Fatalf("converged = %v, %v; want %v", got, err, want)
		}
	}
	assert(true)
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
