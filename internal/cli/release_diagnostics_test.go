package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type diagnosticFailingReconciler struct {
	releasetransition.V2ActivationReconciler
	cause error
}

func (diagnosticFailingReconciler) ID() string { return "materialized-config" }
func (owner diagnosticFailingReconciler) Observe(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	return releasetransition.V2ActivationObservation{}, owner.cause
}

func TestReleaseDiagnosticsPreservePublicWrapperOverRawRefusals(t *testing.T) {
	wrapped := func(yard string) error {
		return materializedConfigActivationError{
			cause: domain.CollectRefusals(errors.New("private asset one"), errors.New("private asset two")),
			yard:  yard, phase: "materialized config", action: "inspect", command: "config status",
		}
	}
	for _, multiple := range []bool{false, true} {
		t.Run(fmt.Sprint(multiple), func(t *testing.T) {
			cause := fmt.Errorf("private wrapper: %w", wrapped("work"))
			message := "yard work: cannot inspect materialized config for release activation"
			retry := "run yard -Y work config status"
			if multiple {
				cause = domain.CollectRefusals(cause, wrapped("default"))
				message = "yard default: cannot inspect materialized config for release activation; " + message
				retry = "run yard -Y default config status, then " + retry
			}
			transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
				ConfigHome: testkit.TempDir(t), Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"},
				RegistryPayload:     []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`),
				ArtifactDigest:      releasetransition.Fingerprint(strings.Repeat("d", 64)),
				VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
				Reconcilers:         []releasetransition.V2ActivationReconciler{diagnosticFailingReconciler{cause: cause}},
				ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
					return releasetransition.ReleaseLinks{Active: "release-a"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := transition.Inspect(context.Background(), releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget})
			if err != nil || inspection.Outcome == nil || inspection.Outcome.Message != message || inspection.Outcome.Retry != retry {
				t.Fatalf("public wrapper lost: %#v, %v", inspection.Outcome, err)
			}
		})
	}
}

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
				} else if !strings.Contains(message, "yard default") || !strings.Contains(message, "profile sample-runtime") || !strings.Contains(message, phase) || strings.Contains(message, cause.Error()) || retry != "run yard -Y default config status" {
					t.Fatalf("profile context missing or unsafe: %q / %q", message, retry)
				}
			})
		}
	}
}

// Pending inspection discovers exactly the repository's configured yards plus
// default, with or without an optional overlay. Missing instances are uninspected.
func TestMaterializedPendingListingNamesEveryYard(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "second", "config.env"), "SSH_PORT=2299\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "alpha", "config.env"), "SSH_PORT=2298\n")
	expected, err := config.YardNames(filepath.Join(root, "config"), configHome)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(expected, "default") {
		expected = append(expected, "default")
	}
	slices.Sort(expected)
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: &testkit.Incus{}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := program.inspectPendingInit(context.Background(), configHome)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, yard := range plan.Yards {
		names = append(names, yard.Name)
		if yard.State != "stopped" || yard.Pending {
			t.Fatalf("absent yard was inspected: %#v", yard)
		}
	}
	if !slices.Equal(names, expected) {
		t.Fatalf("yards=%v want %v", names, expected)
	}
}
