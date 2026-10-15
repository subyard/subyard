package githubbroker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRuntimeStatusReportsLoadedExecutableWithoutCredentials(t *testing.T) {
	status, err := runtimeStatus(false)
	if err != nil || status["protocol"] != runtimeProtocol || status["configured"] != false || !validRuntimeDigest(status["executable_sha256"].(string)) {
		t.Fatalf("runtime status = %#v, %v", status, err)
	}
	if len(status) != 3 {
		t.Fatalf("unexpected status fields: %#v", status)
	}
}

func TestClientRuntimeStatusRejectsPredecessorAndIncompatibleIdentity(t *testing.T) {
	for _, payload := range []string{
		`{"configured":false}`,
		`{"configured":true,"protocol":2,"executable_sha256":"` + strings.Repeat("a", 64) + `"}`,
		`{"configured":true,"protocol":1,"executable_sha256":"` + strings.Repeat("z", 64) + `"}`,
	} {
		socket := brokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, payload) }))
		var output, diagnostic strings.Builder
		if code := RunClient(context.Background(), []string{"--socket", socket, "status", "--runtime"}, strings.NewReader(""), &output, &diagnostic); code != 1 || output.Len() != 0 || !strings.Contains(diagnostic.String(), "incompatible") {
			t.Fatalf("legacy/skew status accepted: code=%d output=%q diagnostic=%q", code, output.String(), diagnostic.String())
		}
	}
}

func TestClientRuntimeStatusProjectsBothLoadedIdentities(t *testing.T) {
	wanted := strings.Repeat("a", 64)
	socket := brokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"configured": false, "protocol": runtimeProtocol, "executable_sha256": wanted, "private": "do-not-print"})
	}))
	var output, diagnostic strings.Builder
	code := RunClient(context.Background(), []string{"--socket", socket, "status", "--runtime"}, strings.NewReader(""), &output, &diagnostic)
	var status map[string]any
	if code != 0 || json.Unmarshal([]byte(output.String()), &status) != nil || len(status) != 3 || status["broker_sha256"] != wanted || !validRuntimeDigest(status["client_sha256"].(string)) || diagnostic.Len() != 0 {
		t.Fatalf("runtime projection = %d %q %q", code, output.String(), diagnostic.String())
	}
}
