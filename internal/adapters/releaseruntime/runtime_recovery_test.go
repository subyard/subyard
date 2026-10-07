//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestVerifiedRecoveryProcessNegotiatesOnlyTheSealedOwner(t *testing.T) {
	ctx := context.Background()
	root := testkit.TempDir(t)
	marker := filepath.Join(root, "executed")
	capabilities := releasetransition.ActivationOnlyRecoveryCapabilities()
	response, err := releasetransition.MarshalRecoveryProcessResponse(releasetransition.RecoveryProcessResponse{
		SchemaVersion: releasetransition.ProcessRecoverySchemaV2, Capabilities: &capabilities})
	if err != nil {
		t.Fatal(err)
	}
	engine := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    cat >/dev/null
    : > %q
    printf '%%s\n' '%s'
    ;;
  *) exit 98 ;;
esac
`, marker, response)
	candidate := writeDelegateRuntimeFixture(t, root, "1.2.3-owner", "owner", []byte("{}\n"), engine)
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	verified, err := runtime.verifyPublishedCandidate(ctx, candidate, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	request := releasetransition.RecoveryProcessRequest{SchemaVersion: releasetransition.ProcessRecoverySchemaV2,
		Mode: releasetransition.RecoveryProcessInspect, RuntimeRoot: root, ConfigHome: filepath.Join(root, "config"),
		Yard: "default", Target: verified.candidate.release, Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: verified.manifestDigest, RegistryDigest: verified.registryDigest,
		Recovery: &releasetransition.ActivationOnlyRecoveryRequest{Transaction: "tx-original", Fingerprint: releasetransition.Fingerprint(strings.Repeat("a", 64))}}
	foreign := request
	foreign.ArtifactDigest = releasetransition.Fingerprint(strings.Repeat("b", 64))
	if _, err := runtime.invokeVerifiedRecoveryTransition(ctx, verified, root, foreign, ""); err == nil {
		t.Fatal("foreign artifact recovery request accepted")
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("foreign recovery request executed an engine")
	}
	probe := releasetransition.RecoveryProcessRequest{SchemaVersion: releasetransition.ProcessRecoverySchemaV2,
		Mode: releasetransition.RecoveryProcessCapabilities}
	result, err := runtime.invokeVerifiedRecoveryTransition(ctx, verified, root, probe, "")
	if err != nil || result.Capabilities == nil || result.Capabilities.Validate() != nil {
		t.Fatalf("sealed owner capability negotiation: %#v %v", result, err)
	}
	if _, err := runtime.invokeVerifiedRecoveryTransition(ctx, verified, root, request, ""); err == nil {
		t.Fatal("capability response accepted as a fresh inspection")
	}
}

func TestVerifiedRecoveryProcessRefusesV1OnlyOwnersWithoutWrites(t *testing.T) {
	ctx := context.Background()
	root := testkit.TempDir(t)
	candidate := writeDelegateRuntimeFixture(t, root, "1.2.3-owner", "owner", []byte("{}\n"), fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat >/dev/null; printf '%%s\n' '%s' ;;
  *) exit 98 ;;
esac
`, candidateProtocolFixtureResponse))
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	owner, err := runtime.verifyPublishedCandidate(ctx, candidate, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	probe := releasetransition.RecoveryProcessRequest{SchemaVersion: releasetransition.ProcessRecoverySchemaV2,
		Mode: releasetransition.RecoveryProcessCapabilities}
	if _, err := runtime.invokeVerifiedRecoveryTransition(ctx, owner, root, probe, ""); err == nil {
		t.Fatal("V1-only owner admitted new recovery writes")
	}
	if _, err := os.Lstat(filepath.Join(root, "config")); !os.IsNotExist(err) {
		t.Fatal("capability negotiation created protected configuration")
	}
}
