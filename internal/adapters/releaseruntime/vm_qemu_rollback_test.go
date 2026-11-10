package releaseruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPendingRollbackReinspectionReadsReportingVM(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	payload, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(payload)
	if err != nil {
		t.Fatal(err)
	}
	journal.Goal.Direction = releasetransition.DirectionActivatePrevious
	payload, err = releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fixture.journalPath, payload, 0o600)
	if err := os.Symlink("releases/"+fixture.target, filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	writeVMRollbackSettings(t, fixture.configHome, "default", "1")
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetProject("subyard", map[string]any{"name": "subyard", "config": map[string]string{
		"restricted": "true", "restricted.virtual-machines.lowlevel": "allow", "user.subyard.config_home": fixture.configHome,
	}})
	raw := "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\""
	values := map[string]string{"user.subyard.managed": "true", "raw.qemu.conf": raw}
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": "virtual-machine", "status": "Running", "config": values, "expanded_config": values})
	client := incusclient.New(server.SocketPath)
	_, source, _, _, _ := retainedRuntimeFixture(t)
	reads := 0
	runtime := New(Config{RepositoryRoot: source, Inputs: fixtureCommandInputs(fixture.environment()),
		YardInstance: func(ctx context.Context, loaded config.Loaded) (ports.InstanceInfo, error) {
			reads++
			return client.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
		},
	})
	inspection, err := runtime.InspectMutationGateInspection(context.Background(), fixture.runtimeRoot, fixture.configHome, "default")
	if err != nil || inspection == nil || inspection.Resume == nil || reads != 1 {
		t.Fatalf("authorized rollback cannot resume after boot: inspection=%+v reads=%d err=%v", inspection, reads, err)
	}
}

func TestVMQEMURollbackBoundsAndRendersCompleteFindingsOnce(t *testing.T) {
	for _, layout := range []bool{false, true} {
		t.Run(fmt.Sprintf("layout refusal %t", layout), func(t *testing.T) {
			runtime, parsed, candidate, request, _ := vmQEMURollbackFixture(t, false)
			if layout {
				writeConfigLayoutManifest(t, request.ConfigHome, 2, true)
			}
			writeVMRollbackSettings(t, request.ConfigHome, "default", "1")
			for n := 0; n < 10; n++ {
				writeVMRollbackSettings(t, request.ConfigHome, fmt.Sprintf("yard-%02d", n), "1")
			}
			runtime.config.YardInstance = func(context.Context, config.Loaded) (ports.InstanceInfo, error) {
				return ports.InstanceInfo{Type: domain.YardVM, LocalConfig: map[string]string{"raw.qemu.conf": domain.VMRawQEMUConfig(true)}}, nil
			}
			_, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
			var failure publicReleaseInspectionError
			if !errors.As(err, &failure) {
				t.Fatalf("refusal = %v", err)
			}
			count := 11
			if layout {
				count++
			}
			if !strings.Contains(failure.outcome.Message, fmt.Sprintf("%d findings omitted", count)) ||
				!strings.Contains(failure.outcome.Message, "stderr of yard update --rollback") ||
				!strings.Contains(failure.outcome.Message, "headless reporting VM") ||
				layout && !strings.Contains(failure.outcome.Message, "Git configuration cache") {
				t.Fatalf("bounded summary lacks real detail location: %q", failure.outcome.Message)
			}
			stderr := runtime.config.Stderr.(*bytes.Buffer)
			if stderr.Len() != 0 {
				t.Fatalf("inspection rendered diagnostics: %s", stderr)
			}
			// The same preparation boundary used by a rollback resume returns the
			// typed public cause for the CLI's ordinary error rendering.
			_, boundaryErr := runtime.inspectionFailure(options{}, err)
			outcome, ok := TransitionOutcome(boundaryErr)
			if !ok || outcome.Message != failure.outcome.Message {
				t.Fatalf("public outcome lost: %v", boundaryErr)
			}
			if err := outcome.ValidateInspection(releasetransition.Goal{Target: request.Target, Direction: request.Direction}); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(stderr, boundaryErr)
			names := []string{"default"}
			for n := 0; n < 10; n++ {
				names = append(names, fmt.Sprintf("yard-%02d", n))
			}
			for _, name := range names {
				if count := strings.Count(stderr.String(), "yard "+name+":"); count != 1 {
					t.Fatalf("yard %s rendered %d times: %s", name, count, stderr)
				}
			}
			if layout && strings.Count(stderr.String(), "installed local-first Git configuration cache") != 1 {
				t.Fatalf("layout refusal missing or duplicated: %s", stderr)
			}
		})
	}
}

func TestRollbackReportsIndependentCompatibilityRefusals(t *testing.T) {
	for _, invalidManifest := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid manifest %t", invalidManifest), func(t *testing.T) {
			runtime, parsed, candidate, request, called := vmQEMURollbackFixture(t, false)
			writeConfigLayoutManifest(t, request.ConfigHome, 2, true)
			if invalidManifest {
				testkit.WriteFile(t, filepath.Join(request.ConfigHome, ".sync/manifest.json"), []byte("invalid"), 0600)
			}
			writeVMRollbackSettings(t, request.ConfigHome, "default", "1")
			runtime.config.YardInstance = func(context.Context, config.Loaded) (ports.InstanceInfo, error) {
				return ports.InstanceInfo{Type: domain.YardVM, LocalConfig: map[string]string{"raw.qemu.conf": domain.VMRawQEMUConfig(true)}}, nil
			}
			_, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
			message := fmt.Sprint(err)
			var failure publicReleaseInspectionError
			if !invalidManifest {
				if !errors.As(err, &failure) {
					t.Fatalf("refusal = %v", err)
				}
				message = failure.outcome.Message
				if err := failure.outcome.ValidateInspection(releasetransition.Goal{Target: request.Target, Direction: request.Direction}); err != nil {
					t.Fatal(err)
				}
			}
			if !strings.Contains(message, "configuration") || !strings.Contains(message, "yard default:") {
				t.Fatalf("independent refusal missing: %s", message)
			}
			if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incompatible target planned rollback: %v", err)
			}
		})
	}
}

func TestVMQEMURollbackCompatibility(t *testing.T) {
	for _, scenario := range []struct {
		name, reporting, raw string
		yards                []string
		supports, allowed    bool
	}{
		{"one affected yard", "1", domain.VMRawQEMUConfig(true), []string{"default"}, false, false},
		{"all affected yards", "1", domain.VMRawQEMUConfig(true), []string{"zulu", "default", "alpha"}, false, false},
		{"non-reporting headless", "0", domain.VMRawQEMUConfig(false), []string{"default"}, false, true},
		{"reporting only", "1", "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\"", []string{"default"}, false, true},
		{"target reads headless reporting", "1", domain.VMRawQEMUConfig(true), []string{"default"}, true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime, parsed, candidate, request, called := vmQEMURollbackFixture(t, scenario.supports)
			for _, name := range scenario.yards {
				writeVMRollbackSettings(t, request.ConfigHome, name, scenario.reporting)
			}
			runtime.config.YardInstance = func(context.Context, config.Loaded) (ports.InstanceInfo, error) {
				return ports.InstanceInfo{Type: domain.YardVM, LocalConfig: map[string]string{"raw.qemu.conf": scenario.raw}}, nil
			}
			_, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
			if scenario.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(called); err != nil {
					t.Fatalf("eligible rollback never inspected: %v", err)
				}
				return
			}
			var failure publicReleaseInspectionError
			if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible {
				t.Fatalf("rollback failure = %v", err)
			}
			last := -1
			names := slices.Clone(scenario.yards)
			slices.SortStableFunc(names, func(a, b string) int {
				if a == "default" {
					return -1
				}
				if b == "default" {
					return 1
				}
				return strings.Compare(a, b)
			})
			for _, name := range names {
				index := strings.Index(failure.outcome.Message, "yard "+name+":")
				if index <= last {
					t.Fatalf("affected yard %s missing or out of order: %s", name, failure.outcome.Message)
				}
				last = index
			}
			if failure.outcome.Retry == "" {
				t.Fatal("refusal has no next step")
			}
			if err := failure.outcome.ValidateInspection(releasetransition.Goal{Target: request.Target, Direction: request.Direction}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incompatible target planned rollback: %v", err)
			}
		})
	}
}

func TestVMQEMURollbackRechecksBeforeExecution(t *testing.T) {
	runtime, parsed, candidate, request, called := vmQEMURollbackFixture(t, false)
	writeVMRollbackSettings(t, request.ConfigHome, "default", "1")
	raw := ""
	runtime.config.YardInstance = func(context.Context, config.Loaded) (ports.InstanceInfo, error) {
		return ports.InstanceInfo{Type: domain.YardVM, LocalConfig: map[string]string{"raw.qemu.conf": raw}}, nil
	}
	prepared, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(called)
	if err != nil {
		t.Fatal(err)
	}
	raw = domain.VMRawQEMUConfig(true)
	err = prepared.Execute(context.Background())
	var failure publicReleaseInspectionError
	if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("rollback execution failure = %v", err)
	}
	after, err := os.ReadFile(called)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("incompatible target executed: %v", err)
	}
}

func vmQEMURollbackFixture(t *testing.T, supports bool) (*Runtime, options, publishedCandidate, releasetransition.ProcessRequest, string) {
	t.Helper()
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	settings, source, _, _, _ := retainedRuntimeFixture(t)
	runtime.config.RepositoryRoot, runtime.config.Inputs = source, settings.config.Inputs
	if supports {
		engine := filepath.Join(candidate.root, "bin", "yard-engine")
		payload, err := os.ReadFile(engine)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.Replace(payload, []byte("*) exit 64"), []byte("--command-effect) printf 'mutate\\n' ;;\n*) exit 64"), 1)
		testkit.WriteFile(t, engine, payload, 0o700)
		registry := []byte("_vm-start-policy||@vm-start-policy||local|mutate|never|hidden|internal|none|_vm-start-policy|apply headless VM policy||\n")
		testkit.WriteFile(t, filepath.Join(candidate.root, "config", "commands.registry"), registry, 0o600)
		rewriteReleaseOwnerManifest(t, candidate)
	}
	return runtime, parsed, candidate, request, called
}

func writeVMRollbackSettings(t *testing.T, root, name, reporting string) {
	t.Helper()
	directory := filepath.Join(root, "yards", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "config.env"), []byte("YARD_KIND=vm\nVM_FREE_PAGE_REPORTING="+reporting+"\n"), 0o600)
}
