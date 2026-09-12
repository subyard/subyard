package cli

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTailscaleEndpointFallsBackWhenLocalDNSIsUnavailable(t *testing.T) {
	bin := t.TempDir()
	writeCLIFile(t, filepath.Join(bin, "tailscale"), `#!/bin/sh
printf '%s\n' '{"BackendState":"Running","Self":{"DNSName":"owner.example.ts.net.","TailscaleIPs":["100.64.1.2"]}}'
`, 0755)
	writeCLIFile(t, filepath.Join(bin, "getent"), "#!/bin/sh\nexit 2\n", 0755)
	cli := &CLI{baseEnv: map[string]string{"PATH": bin + ":/usr/bin:/bin"}}
	host, err := cli.resourceEndpointManager().Discover(context.Background())
	if err != nil || host != "100.64.1.2" {
		t.Fatalf("host=%q error=%v", host, err)
	}
}

func TestTailscaleEndpointUsesSelfAndFallsBackToIPv4(t *testing.T) {
	for _, test := range []struct {
		name, document, want string
		fail                 bool
	}{
		{"name", `{"BackendState":"Running","Self":{"DNSName":"owner.example.ts.net.","TailscaleIPs":["100.64.1.2","fd00::1"]},"Peer":{"other":{"DNSName":"wrong.example.ts.net"}}}`, "owner.example.ts.net", false},
		{"ip", `{"BackendState":"Running","Self":{"DNSName":"","TailscaleIPs":["100.64.1.2"]}}`, "100.64.1.2", false},
		{"offline", `{"BackendState":"NeedsLogin","Self":{"DNSName":"owner.example.ts.net","TailscaleIPs":["100.64.1.2"]}}`, "", true},
		{"no ipv4", `{"BackendState":"Running","Self":{"DNSName":"owner.example.ts.net","TailscaleIPs":["fd00::1"]}}`, "", true},
		{"malformed", `{`, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := tailscaleEndpointHost([]byte(test.document))
			if (err != nil) != test.fail || got != test.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestEndpointListenerDetectionIncludesWildcardBindings(t *testing.T) {
	for _, test := range []struct {
		listeners string
		occupied  bool
	}{
		{"LISTEN 0 4096 100.64.1.2:6768 0.0.0.0:*\n", true},
		{"LISTEN 0 4096 0.0.0.0:6768 0.0.0.0:*\n", true},
		{"LISTEN 0 4096 *:6768 *:*\n", true},
		{"LISTEN 0 4096 [::]:6768 [::]:*\n", true},
		{"LISTEN 0 4096 127.0.0.1:6768 0.0.0.0:*\n", false},
		{"LISTEN 0 4096 100.64.1.2:6769 0.0.0.0:*\n", false},
	} {
		if got := endpointListenerPresent([]byte(test.listeners), "100.64.1.2", 6768); got != test.occupied {
			t.Fatalf("listeners %q: got %v", test.listeners, got)
		}
	}
}
