package githubbroker

import (
	"bytes"
	"strings"
	"testing"
)

func TestSelectedDiagnosticsRedactSplitSecretsAndStreamProgress(t *testing.T) {
	for _, secret := range []string{"synthetic-token", "c3ludGhldGljLXRva2Vu", "https://user@github.com/owner/repo.git"} {
		for cut := 1; cut < len(secret); cut++ {
			var output bytes.Buffer
			diagnostic := &gitDiagnostics{output: &output, sensitive: []string{secret}}
			_, _ = diagnostic.Write([]byte("tentative upstream failure " + secret))
			if output.Len() != 0 {
				t.Fatal("tentative diagnostic escaped")
			}
			diagnostic.selectBackend()
			_, _ = diagnostic.Write([]byte("native progress\n"))
			if output.String() != "native progress\n" {
				t.Fatal("native progress did not stream")
			}
			_, _ = diagnostic.Write([]byte(secret[:cut]))
			_, _ = diagnostic.Write([]byte(secret[cut:] + " native error\n"))
			diagnostic.flush()
			if output.String() != "native progress\n[redacted] native error\n" {
				t.Fatal("split secret was not redacted")
			}
		}
	}
	var output bytes.Buffer
	diagnostic := &gitDiagnostics{output: &output, enabled: true, sensitive: []string{"synthetic-token"}}
	_, _ = diagnostic.Write([]byte("error synthet"))
	diagnostic.flush()
	if strings.Contains(output.String(), "synthet") {
		t.Fatal("partial credential escaped at EOF")
	}
}
