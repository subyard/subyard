package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The fake installed launcher reports a synthetic pending profile only for the
// default yard and records every provision request it receives.
const pendingProfileLauncher = `#!/bin/sh
yard=${SUBYARD_YARD:-default}
if [ "$1" = "-Y" ]; then yard=$2; shift 2; fi
case "$1" in
profile)
  convergence=current
  [ "$yard" = default ] && convergence=changes-required
  printf '{"schemaVersion":1,"yardName":"%s","profiles":[` +
	`{"name":"synthetic","selected":true,"eligible":true,"provisionable":true,"convergence":"%s"},` +
	`{"name":"unselected","selected":false,"eligible":true,"provisionable":true,"convergence":"changes-required"}]}\n' "$yard" "$convergence" ;;
provision)
  printf '%s %s\n' "$yard" "$2" >>"$PROVISION_LOG"
  exit "${PROVISION_EXIT:-0}" ;;
*) exit 2 ;;
esac
`

func pendingProfileFixture(t *testing.T) (*releaseExecution, string) {
	t.Helper()
	root, configHome := testkit.TempDir(t), testkit.TempDir(t)
	launcher := filepath.Join(root, "current", "bin", "yard")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, launcher, []byte(pendingProfileLauncher), 0o700)
	if err := os.MkdirAll(filepath.Join(configHome, "yards", "second"), 0o700); err != nil {
		t.Fatal(err)
	}
	execution := &releaseExecution{configHome: configHome, prepared: releaseruntime.Prepared{ActiveLauncher: launcher}}
	return execution, filepath.Join(root, "provision.log")
}

func pendingProfileCLI(log string, interactive bool, exit string) (*CLI, *bytes.Buffer) {
	var output bytes.Buffer
	env := map[string]string{"PATH": os.Getenv("PATH"), "PROVISION_LOG": log, "PROVISION_EXIT": exit}
	return &CLI{
		options:             Options{Program: "yard", Stdout: &output, Stderr: &output, Stdin: strings.NewReader("")},
		env:                 env,
		baseEnv:             env,
		promptInputTerminal: func() bool { return interactive },
	}, &output
}

func TestPendingProfileProvisionIsOfferedThroughInstalledLauncher(t *testing.T) {
	execution, log := pendingProfileFixture(t)
	cli, output := pendingProfileCLI(log, true, "0")
	cli.offerPendingProfileProvision(context.Background(), execution, false)
	recorded, err := os.ReadFile(log)
	if err != nil || string(recorded) != "default synthetic\n" {
		t.Fatalf("provision requests = %q, %v; output:\n%s", recorded, err, output)
	}
	if !strings.Contains(output.String(), "Profile synthetic in yard default is selected but not provisioned") ||
		strings.Contains(output.String(), "second") || strings.Contains(output.String(), "unselected") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestPendingProfileProvisionDeclineKeepsOffer(t *testing.T) {
	execution, log := pendingProfileFixture(t)
	cli, output := pendingProfileCLI(log, true, "1")
	cli.offerPendingProfileProvision(context.Background(), execution, false)
	if !strings.Contains(output.String(), "the next yard update or yard migrate offers it again") {
		t.Fatalf("declined provision lost its continuation:\n%s", output)
	}
}

func TestPendingProfileProvisionIsNotRunWithoutOperator(t *testing.T) {
	for name, assumeYes := range map[string]bool{"non-interactive": false, "assume-yes": true} {
		t.Run(name, func(t *testing.T) {
			execution, log := pendingProfileFixture(t)
			// --yes keeps a terminal; only the operator's own answer may start provision.
			cli, output := pendingProfileCLI(log, assumeYes, "0")
			cli.offerPendingProfileProvision(context.Background(), execution, assumeYes)
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("provision ran without an interactive operator: %v", err)
			}
			if !strings.Contains(output.String(), "Provision it with: yard provision synthetic") {
				t.Fatalf("missing provision continuation:\n%s", output)
			}
		})
	}
}

func TestPendingProfileProvisionIgnoresInheritedYardSelection(t *testing.T) {
	execution, log := pendingProfileFixture(t)
	cli, output := pendingProfileCLI(log, true, "0")
	cli.baseEnv["SUBYARD_YARD"] = "second"
	cli.offerPendingProfileProvision(context.Background(), execution, false)
	if recorded, err := os.ReadFile(log); err != nil || string(recorded) != "default synthetic\n" {
		t.Fatalf("provision requests = %q, %v; output:\n%s", recorded, err, output)
	}
}

func TestPendingProfileProvisionIsSuppressedInsideInit(t *testing.T) {
	execution, log := pendingProfileFixture(t)
	cli, output := pendingProfileCLI(log, true, "0")
	cli.profileOfferSuppressed = true
	cli.offerPendingProfileProvision(context.Background(), execution, false)
	if _, err := os.Stat(log); !os.IsNotExist(err) || output.Len() != 0 {
		t.Fatalf("init-embedded migration offered provisioning: %v\n%s", err, output)
	}
}
