package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/observerroute"
	"github.com/Subyard/Subyard/internal/ports"
)

func (runtime Runtime) aiObserverConverged(ctx context.Context, instance ports.InstanceInfo) (bool, error) {
	selected := slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), "aiobserver")
	marker := instance.Config["user.subyard.ai_observer_provision"]
	proxyMarker := instance.Config["user.subyard.ai_observer_proxy"]
	if !selected {
		return marker == "" && proxyMarker == "", nil
	}
	frontendURL := runtime.aiObserverFrontendURL(ctx)
	digest, err := runtime.aiObserverProvisionIdentity(frontendURL)
	if err != nil || marker != digest {
		return false, err
	}
	if runtime.Yard.YardKind == domain.YardVM {
		return proxyMarker == "", nil
	}
	host, observedPort, ready := observerroute.Owned(proxyMarker, instance.Devices["ai-observer"])
	return ready && frontendURL == "http://"+host+":"+observedPort, nil
}

// The browser uses the published port for both direct access and SSH forwarding.
func (runtime Runtime) aiObserverFrontendURL(ctx context.Context) string {
	if !slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), "aiobserver") {
		return ""
	}
	host := "127.0.0.1"
	if runtime.Yard.YardKind != domain.YardVM {
		host = observerroute.Host(ctx)
	}
	return "http://" + host + ":" + runtime.environmentValue("AI_OBSERVER_HOST_PORT")
}

// Both the package and its inputs must converge. Docker bind mounts retain the
// original backing mount, so a new host mount at the same guest path also needs
// a fresh observer container.
func (runtime Runtime) aiObserverProvisionIdentity(frontendURL string) (string, error) {
	agents := strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS"))
	if !slices.Contains(agents, "aiobserver") {
		return "", nil
	}
	file := guestConfigFile{source: runtime.environmentValue("AGENT_aiobserver_PROVISION")}
	digest, err := file.sourceHash()
	if err != nil {
		return "", err
	}
	fields := []string{"v1", digest, runtime.devUser(), runtime.environmentDefault("DEV_UID", "1000"), frontendURL}
	for _, source := range []string{"claude", "codex"} {
		fields = append(fields, fmt.Sprint(slices.Contains(agents, source)))
	}
	for _, name := range []string{"HOST_BASE", "HOST_LINKS", "HOST_MOUNTS"} {
		fields = append(fields, runtime.environmentValue(name))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(fields, "\x00")+"\x00"))), nil
}
