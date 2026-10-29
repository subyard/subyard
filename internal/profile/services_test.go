package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestServiceReadinessDiagnosticIsOptInAndClosed(t *testing.T) {
	const valid = "subyard-profile-readiness-v1 boot-dependency-unloaded\n"
	for _, test := range []struct {
		name, stderr, reason string
	}{
		{"valid", valid, "boot-dependency-unloaded"},
		{"missing", "", ""},
		{"arbitrary", "private-canary\n", ""},
		{"unknown", "subyard-profile-readiness-v1 private-canary\n", ""},
		{"duplicate", valid + valid, ""},
		{"extra", valid + "private-canary\n", ""},
		{"unterminated", strings.TrimSuffix(valid, "\n"), ""},
		{"whitespace", "subyard-profile-readiness-v1  boot-dependency-unloaded\n", ""},
		{"oversized", valid + strings.Repeat("x", 4096), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			fixture(t, root, "sample", fmt.Sprintf("#!/bin/bash\nprintf '%%s' '%s' >&2\nexit 1\n", test.stderr), Definition{})
			for _, optIn := range []string{"", "1"} {
				env := map[string]string{"SUBYARD_ENGINE_CONTEXT": "1", "SUBYARD_ENGINE_CONTEXT_SCHEMA": "1", "SUBYARD_PROFILE_DIAGNOSTICS": optIn}
				var output bytes.Buffer
				err := RunServices(context.Background(), root, []string{"--check"}, env, &output, &output)
				var failure *ServiceCheckError
				want := ""
				if optIn == "1" {
					want = test.reason
				}
				message := "profile service check owner=sample native-exit=1"
				if want != "" {
					message += " reason=" + want
				}
				if !errors.As(err, &failure) || failure.Reason != want || err.Error() != message || ServiceCheckExit(err) != 1 || output.Len() != 0 {
					t.Fatalf("opt-in=%q: err=%v output=%q", optIn, err, output.String())
				}
			}
		})
	}
}

func TestServiceReadinessBufferRetainsBoundAcrossWrites(t *testing.T) {
	var buffer serviceReadinessBuffer
	for range 1000 {
		_, _ = buffer.Write([]byte("subyard-profile-readiness-v1 unit-inactive\n"))
	}
	if len(buffer) > 128 || buffer.reason() != "" {
		t.Fatal("readiness capture exceeded its bound or admitted duplicate output")
	}
}
