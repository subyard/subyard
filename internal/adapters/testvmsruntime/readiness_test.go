package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDoctorDiagnosticUsesActualChecksAndDoesNotExposeRawErrors(t *testing.T) {
	root := testkit.TempDir(t)
	config := filepath.Join(root, "config")
	engine := filepath.Join(root, "engine")
	testkit.WriteFile(t, config, []byte("fixture config"), 0o600)
	testkit.WriteFile(t, engine, []byte("fixture engine"), 0o700)
	for _, test := range []struct {
		name, enabled, config, engine, reason string
	}{
		{"enabled", "1", config, engine, "backend enabled state differs"},
		{"config", "0", filepath.Join(root, "private-config-value"), engine, "backend config is missing"},
		{"engine hash", "0", config, engine, "installed test-vms engine hash differs"},
		{"engine inspection", "0", config, filepath.Join(root, "private-engine-value"), "installed test-vms engine cannot be inspected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			runtime := &Runtime{ConfigPath: test.config, ExecutablePath: test.engine, Stdout: &output}
			want := map[string]string{"WANT_ENABLED": test.enabled, "WANT_ENGINE_HASH": "different", "WANT_DIAGNOSTIC": "1"}
			if err := runtime.Run(context.Background(), []string{"doctor"}, want); err == nil {
				t.Fatal("doctor accepted a failed check")
			}
			converged, reason := parseDoctorReport(output.String())
			if converged || reason != test.reason || strings.Contains(output.String(), "private-") {
				t.Fatalf("doctor report = %q", output.String())
			}
			output.Reset()
			delete(want, "WANT_DIAGNOSTIC")
			if err := runtime.Run(context.Background(), []string{"doctor"}, want); err == nil || output.Len() != 0 {
				t.Fatalf("ordinary doctor changed: error=%v, report=%q", err, output.String())
			}
		})
	}
}

func TestDoctorReportRejectsUnmarkedAndInvalidDiagnostics(t *testing.T) {
	for _, err := range []error{
		errors.New("private guest stderr and capability"),
		doctorCheck("private guest stderr and capability", nil),
		doctorCheck("firewall table is missing", errors.New("private guest stderr and capability")),
		doctorCheck("inner Incus version cannot be inspected", errors.New("private guest stderr and capability")),
		nil,
	} {
		var output bytes.Buffer
		if err := writeDoctorReport(&output, err); err != nil {
			t.Fatal(err)
		}
		converged, reason := parseDoctorReport(output.String())
		if strings.Contains(output.String(), "private") || converged != (err == nil) {
			t.Fatalf("unsafe report = %q", output.String())
		}
		if check, ok := err.(*doctorCheckError); ok && !strings.Contains(check.reason, "private") && reason != check.reason {
			t.Fatalf("source-owned failure lost: %q", output.String())
		}
	}
	for _, payload := range []string{
		"", "private raw guest output",
		`{"converged":false,"reason":"private secret"}`,
		`{"converged":true,"reason":"private secret"}`,
		`{"converged":true}`, `{"reason":"firewall table is missing"}`,
		`{"converged":null,"reason":"firewall table is missing"}`,
		`{"converged":true,"reason":null}`,
		`{"converged":false,"converged":true,"reason":""}`,
		`{"Converged":true,"reason":""}`,
		`{"converged":false,"reason":"firewall table is missing","stderr":"private secret"}`,
		`{"converged":true,"reason":""} private trailing output`,
		strings.Repeat("x", 4097),
	} {
		if converged, reason := parseDoctorReport(payload); converged || reason != doctorFallback {
			t.Fatalf("invalid report accepted: converged=%t reason=%q", converged, reason)
		}
	}
}

func TestBackendVerifyReportsDriftAndSafeDoctorReasons(t *testing.T) {
	for _, test := range []struct {
		name, stage, report, reason string
		runError                    bool
	}{
		{"memory", "memory", "", "host memory device is not converged", false},
		{"ownership", "ownership", "", "physical memory device ownership conflict", false},
		{"revision", "revision", "", "backend revision differs", false},
		{"power", "power", "", "outer yard power state differs", false},
		{"doctor", "doctor", `{"converged":false,"reason":"firewall table is missing"}`, "firewall table is missing", true},
		{"raw doctor", "doctor", "private guest output", doctorFallback, true},
		{"unsafe reason", "doctor", `{"converged":false,"reason":"private lease secret"}`, doctorFallback, true},
		{"legacy ready", "doctor", "", "", false},
		{"legacy failure", "doctor", "", doctorFallback, true},
		{"malformed successful doctor", "doctor", "private guest output", doctorFallback, false},
		{"success with transport failure", "doctor", `{"converged":true,"reason":""}`, doctorFallback, true},
		{"ready", "doctor", `{"converged":true,"reason":""}`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := fixtureBackend(t)
			backend.Environment["NESTED_E2E_VMS"] = "0"
			backend.DesiredPower = "running"
			state, err := backend.state()
			if err != nil {
				t.Fatal(err)
			}
			memory := backendMemoryFixture(test.stage == "memory")
			if test.stage == "ownership" {
				memory.Config[hostMemoryOwnerKey] = "private owner value"
			}
			var doctorCalls int
			backend.Runner = &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if body, handled := memory.handle(t, args); handled {
					return body, nil, nil
				}
				call := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(call, "config get "):
					if test.stage == "revision" {
						return []byte("private stale marker"), nil, nil
					}
					return []byte(state.marker), nil, nil
				case strings.HasPrefix(call, "list "):
					if test.stage == "power" {
						return []byte("STOPPED"), nil, nil
					}
					return []byte("RUNNING"), nil, nil
				case strings.HasSuffix(call, "_test-vms-worker doctor"):
					doctorCalls++
					var runErr error
					if test.runError {
						runErr = errors.New("private command args and stderr")
					}
					if !strings.Contains(call, "WANT_DIAGNOSTIC=1") {
						return nil, nil, runErr
					}
					return []byte(test.report), []byte("private stderr"), runErr
				}
				t.Fatalf("unexpected call: %s", call)
				return nil, nil, nil
			}}
			initialReady, initialErr := backend.Converged(context.Background())
			if test.stage == "ownership" {
				var diagnostic interface{ ActivationDiagnostic() (string, string) }
				if initialReady || !errors.As(initialErr, &diagnostic) {
					t.Fatalf("initial inspection lost the guarded cause: %t %v", initialReady, initialErr)
				}
				message, _ := diagnostic.ActivationDiagnostic()
				if !strings.Contains(message, test.reason) || strings.Contains(message, "private") {
					t.Fatalf("initial inspection diagnostic = %q", message)
				}
			} else if test.stage != "doctor" && (initialReady || initialErr != nil) {
				t.Fatalf("expected drift became an error: %t %v", initialReady, initialErr)
			}
			doctorCalls = 0
			converged, err := backend.Verify(context.Background())
			if test.reason == "" {
				if !converged || err != nil {
					t.Fatalf("ready backend rejected: %t %v", converged, err)
				}
				return
			}
			if converged || err == nil {
				t.Fatalf("failed readiness accepted: %t %v", converged, err)
			}
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if !errors.As(err, &diagnostic) {
				t.Fatalf("missing diagnostic: %v", err)
			}
			message, retry := diagnostic.ActivationDiagnostic()
			if !strings.Contains(message, test.reason) || !strings.Contains(message, "test-yard") || retry != "run yard -Y test-yard init" || strings.Contains(message, "private") {
				t.Fatalf("unsafe or imprecise diagnostic: %q %q", message, retry)
			}
			if test.stage == "doctor" && doctorCalls != 1 {
				t.Fatalf("doctor was probed %d times", doctorCalls)
			}
		})
	}
}

func TestReadinessErrorBoundsPublicContextAndPreservesCause(t *testing.T) {
	cause := errors.New("private transport failure")
	for _, test := range []struct{ yard, reason string }{
		{"test-yard", "engine differs"},
		{"private\nunsafe", "engine differs"},
		{"", "private\nunsafe"},
		{"test-yard", strings.Repeat("x", 257)},
	} {
		err := NewReadinessError(test.yard, test.reason, cause)
		if !errors.Is(err, cause) {
			t.Fatal("underlying failure was lost")
		}
		message, retry := err.(interface{ ActivationDiagnostic() (string, string) }).ActivationDiagnostic()
		payload, _ := json.Marshal([]string{message, retry, err.Error()})
		if strings.Contains(string(payload), "private") || len(message) > 512 {
			t.Fatalf("unsafe diagnostic: %s", payload)
		}
	}
}
