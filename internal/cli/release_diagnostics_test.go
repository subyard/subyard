package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestReleaseDiagnosticsIdentifyMalformedNamedYard(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "broken", "config.env"),
		"# location\nSSH_PORT=${UNSET:?synthetic-private-value}\n")
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &materializedConfigActivationReconciler{cli: program, yard: "default", configHome: configHome, allLocal: true, scopeResolved: true}
	_, err = reconciler.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(err, &diagnostic) {
		t.Fatalf("missing named-yard diagnostic: %v", err)
	}
	message, retry := diagnostic.ActivationDiagnostic()
	for _, want := range []string{"yard broken", "yards/broken/config.env:2", "SSH_PORT", "required parameter is unset"} {
		if !strings.Contains(message, want) {
			t.Fatalf("diagnostic omits %q: %q", want, message)
		}
	}
	if strings.Contains(message, "synthetic-private-value") || strings.Contains(message, configHome) || retry != "run yard -Y broken config status" {
		t.Fatalf("unsafe or misdirected named-yard diagnostic: %q / %q", message, retry)
	}
	var stderr bytes.Buffer
	reader, err := New(Options{RepositoryRoot: root, Environment: environment,
		Arguments: []string{"-Y", "broken", "config", "status"}, Stdout: io.Discard, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if reader.Run(context.Background()) == 0 {
		t.Fatal("malformed config status unexpectedly succeeded")
	}
	output := stderr.String()
	if !strings.Contains(output, "yards/broken/config.env:2") || !strings.Contains(output, "SSH_PORT") ||
		strings.Contains(output, configHome) || strings.Contains(output, "synthetic-private-value") {
		t.Fatalf("suggested inspection lost safe source context: %q", output)
	}
}

type releaseDiagnosticConfigPlatform struct {
	*initPlatformFixture
	cause error
}

func (fixture *releaseDiagnosticConfigPlatform) RefreshConfigs(context.Context) error {
	fixture.configs++
	if fixture.configs == 2 {
		return fixture.cause
	}
	return nil
}

func TestReleaseDiagnosticsApplySecondTargetPreservesNativeCause(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join(environment, "\n")+"\n", 0o600)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "second", "config.env"), "SSH_PORT=2233\n")
	cause := publicReconcileTestError{}
	platform := &releaseDiagnosticConfigPlatform{initPlatformFixture: newInitPlatformFixture(), cause: cause}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	applier := releaseTransitionConfigApplier{cli: program}
	if err := applier.ApplyConfig(context.Background(), "default"); err != nil {
		t.Fatalf("first target failed: %v", err)
	}
	err = applier.ApplyConfig(context.Background(), "second")
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if !errors.Is(err, cause) || !errors.As(err, &diagnostic) || platform.configs != 2 {
		t.Fatalf("native second-target cause lost: calls=%d error=%v", platform.configs, err)
	}
	message, retry := diagnostic.ActivationDiagnostic()
	if message != "known integration ownership conflict" || retry != "run yard -Y default integration status" || strings.Contains(err.Error(), "exited with status") {
		t.Fatalf("second-target cause was replaced: %q / %q, error=%v", message, retry, err)
	}
}

type releaseDiagnosticProfilePlatform struct {
	*initPlatformFixture
	phase             string
	cause             error
	observes, applies int
}

func (fixture *releaseDiagnosticProfilePlatform) ObserveProfileRuntimes(context.Context) (map[string]ports.RuntimeObservation, error) {
	fixture.observes++
	if fixture.phase == "inspect" || (fixture.phase == "verify" && fixture.applies != 0) {
		return nil, fixture.cause
	}
	return map[string]ports.RuntimeObservation{"sample-runtime": {State: "stale", Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}}, nil
}

func (fixture *releaseDiagnosticProfilePlatform) ApplyProfileRuntime(context.Context, string, string, string, string) error {
	fixture.applies++
	if fixture.phase == "apply" {
		return fixture.cause
	}
	return nil
}

func TestReleaseDiagnosticsProfileFailurePreservesPhaseAndPublicCause(t *testing.T) {
	for _, phase := range []string{"inspect", "apply", "verify"} {
		for _, public := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/public=%t", phase, public), func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join(environment, "\n")+"\n", 0o600)
				configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
				seedCurrentReleaseLedger(t, root, configHome)
				cause := error(errors.New("synthetic-private-profile-cause"))
				if public {
					cause = publicReconcileTestError{}
				}
				platform := &releaseDiagnosticProfilePlatform{initPlatformFixture: newInitPlatformFixture(), phase: phase, cause: cause}
				program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform, Incus: &testkit.Incus{}, Stdout: io.Discard, Stderr: io.Discard})
				if err != nil {
					t.Fatal(err)
				}
				reconciler := program.profileActivationReconciler(releasetransition.ProcessRequest{ConfigHome: configHome}, "sample-runtime")
				if phase == "inspect" {
					_, err = reconciler.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
				} else {
					err = reconciler.Reconcile(context.Background(), releasetransition.ReleaseLinks{})
				}
				var diagnostic interface{ ActivationDiagnostic() (string, string) }
				var phased interface{ ActivationPhase() string }
				if !errors.Is(err, cause) || !errors.As(err, &diagnostic) || !errors.As(err, &phased) || phased.ActivationPhase() != phase {
					t.Fatalf("profile cause/phase lost: %v", err)
				}
				message, retry := diagnostic.ActivationDiagnostic()
				if public {
					if message != "known integration ownership conflict" || retry != "run yard -Y default integration status" {
						t.Fatalf("specific profile diagnostic replaced: %q / %q", message, retry)
					}
				} else if !strings.Contains(message, "yard default") || !strings.Contains(message, "profile sample-runtime") || !strings.Contains(message, phase) || strings.Contains(message, cause.Error()) || retry != "run yard -Y default status" {
					t.Fatalf("profile context missing or unsafe: %q / %q", message, retry)
				}
			})
		}
	}
}
