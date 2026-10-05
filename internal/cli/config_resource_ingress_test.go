package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestPublicIngressResourceMustBeDownBeforeConfigDropsOrChangesIt(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp"} {
		for _, test := range []struct {
			name, setting, value string
			enabled              bool
			wantCode             int
		}{
			{"selected profile in use", "ENVIRONMENT_PROFILES", "", true, 1},
			{"endpoint in use", "RESOURCE_RELAY_PORT", "42001", true, 1},
			{"selected profile closed", "ENVIRONMENT_PROFILES", "", false, 0},
			{"endpoint closed", "RESOURCE_RELAY_PORT", "42001", false, 0},
		} {
			t.Run(protocol+"/"+test.name, func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				yardFile := filepath.Join(root, "state", "yards", "private", "config.env")
				if err := os.MkdirAll(filepath.Dir(yardFile), 0o700); err != nil {
					t.Fatal(err)
				}
				original := "YARD_KIND=vm\nENVIRONMENT_PROFILES=sample\nRESOURCE_RELAY_PORT=42000\n"
				writeCLIFile(t, yardFile, original, 0o600)
				resourceDir := filepath.Join(root, "config", "profiles", "sample", "resources")
				if err := os.MkdirAll(filepath.Join(resourceDir, "relay"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, filepath.Join(resourceDir, "relay.res"), fmt.Sprintf(`
COMMAND=relay
HANDLER=resources/relay/handler.sh
TITLE="Sample relay"
PROXY="sample-relay RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT RESOURCE_RELAY_INTERFACE %s:guest:41999 owner-metadata-v1 owner-ipv4-%s"
ACTION="up up public-ingress-change reversible"
ACTION="down down public-ingress-change reversible"
BRINGUP=up
SHUTDOWN=down
`, protocol, protocol), 0o600)
				writeCLIFile(t, filepath.Join(resourceDir, "relay", "handler.sh"), `#!/bin/sh
set -eu
if [ "${SUBYARD_RESOURCE_MODE:-}" = apply ]; then
  : >"$SUBYARD_REPOSITORY_ROOT/relay-applied"
  exit 70
fi
[ "$1" = down ] || exit 2
changed=false
[ ! -e "$SUBYARD_REPOSITORY_ROOT/relay-enabled" ] || changed=true
printf '{"schema":"yard.resource-action-assessment.v1","action":"down","changed":%s,"consequences":["close sample relay"]}\n' "$changed"
`, 0o700)
				if test.enabled {
					writeCLIFile(t, filepath.Join(root, "relay-enabled"), "synthetic\n", 0o600)
				}
				var stdout, stderr bytes.Buffer
				program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: append(environment, "SUBYARD_OPERATION_ID=config-relay"),
					WorkingDir: root, Stdout: &stdout, Stderr: &stderr})
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := program.loadContext("private")
				if err != nil {
					t.Fatal(err)
				}
				code := program.runConfigAuthoring(context.Background(), loaded, "set",
					[]string{test.setting, test.value, "--scope", "yard"}, true)
				if code != test.wantCode {
					t.Fatalf("config code=%d, want=%d; stderr=%s", code, test.wantCode, stderr.String())
				}
				content, err := os.ReadFile(yardFile)
				if err != nil {
					t.Fatal(err)
				}
				if test.enabled && string(content) != original {
					t.Fatalf("active ingress config changed: %q", content)
				}
				if !test.enabled {
					settings, err := config.ReadAssignments(yardFile)
					if err != nil || settings[test.setting] != test.value {
						t.Fatalf("closed ingress config was not updated: %q, %v", content, err)
					}
				}
				if _, err := os.Stat(filepath.Join(root, "relay-applied")); !os.IsNotExist(err) {
					t.Fatalf("config change ran resource apply: %v", err)
				}
			})
		}
	}
}
