package reconcileruntime

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func (runtime Runtime) aiObserverConverged(instance ports.InstanceInfo) (bool, error) {
	selected := slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), "aiobserver")
	marker := instance.Config["user.subyard.ai_observer_provision"]
	proxyMarker := instance.Config["user.subyard.ai_observer_proxy"]
	if !selected {
		return marker == "" && proxyMarker == "", nil
	}
	digest, err := runtime.aiObserverProvisionIdentity()
	if err != nil || marker != digest {
		return false, err
	}
	if runtime.Yard.YardKind == domain.YardVM {
		return proxyMarker == "", nil
	}
	port := runtime.environmentValue("AI_OBSERVER_HOST_PORT")
	device := instance.Devices["ai-observer"]
	return port != "" && proxyMarker == "v1:"+port && maps.Equal(device, map[string]string{
		"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:" + port, "connect": "tcp:127.0.0.1:8080",
	}), nil
}

// Both the package and its inputs must converge. Docker bind mounts retain the
// original backing mount, so a new host mount at the same guest path also needs
// a fresh observer container.
func (runtime Runtime) aiObserverProvisionIdentity() (string, error) {
	agents := strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS"))
	if !slices.Contains(agents, "aiobserver") {
		return "", nil
	}
	file := guestConfigFile{source: runtime.environmentValue("AGENT_aiobserver_PROVISION")}
	digest, err := file.sourceHash()
	if err != nil {
		return "", err
	}
	fields := []string{"v1", digest, runtime.devUser(), runtime.environmentDefault("DEV_UID", "1000")}
	for _, source := range []string{"claude", "codex"} {
		fields = append(fields, fmt.Sprint(slices.Contains(agents, source)))
	}
	for _, name := range []string{"HOST_BASE", "HOST_LINKS", "HOST_MOUNTS"} {
		fields = append(fields, runtime.environmentValue(name))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(fields, "\x00")+"\x00"))), nil
}
