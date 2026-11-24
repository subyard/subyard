package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func deadSettingsFixture(t *testing.T) (*CLI, string, string, *bytes.Buffer) {
	t.Helper()
	f := newFixture(t, withWorkingDir(""))
	root := f.Root
	if err := os.MkdirAll(filepath.Join(root, "config/agents/sample"), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config/agents/sample/integration.json"), `{"schema_version":2,"command":"sample","artifacts":[{"id":"config","kind":"config","source":"settings.json","destination":".sample/settings.json","format":"destination"}]}`, 0600)
	writeCLIFile(t, filepath.Join(root, "config/agents/sample/settings.json"), "{}\n", 0600)
	home := environmentValue(f.Environment, "SUBYARD_CONFIG_HOME")
	source := filepath.Join(home, "overrides/host/agents/sample/settings.json")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, source, "{}\n", 0600)
	path := filepath.Join(home, "config.env")
	writeCLIFile(t, path, "# retained\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json\"\n", 0640)
	var output bytes.Buffer
	program := f.New(Options{RepositoryRoot: root, Environment: f.Environment, Stdout: &output, Stderr: &output})
	return program, home, path, &output
}

func TestDeadSettingsValidationReportsLineAndSourceWithoutWriting(t *testing.T) {
	cli, home, path, output := deadSettingsFixture(t)
	before, err := config.ReadPersistentFileSnapshot(home, path)
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.runConfigValidate("default", nil); code != 0 {
		t.Fatalf("validate=%d output=%s", code, output)
	}
	if !strings.Contains(output.String(), path+":2") || !strings.Contains(output.String(), "update will remove this line") || !strings.Contains(output.String(), filepath.Join(home, "overrides/host/agents/sample/settings.json")) {
		t.Fatal(output.String())
	}
	after, err := config.ReadPersistentFileSnapshot(home, path)
	if err != nil || !bytes.Equal(before.Content, after.Content) || before.Identity != after.Identity {
		t.Fatal("validation changed settings", err)
	}
	output.Reset()
	if code := cli.runConfigValidate("default", []string{"--json"}); code != 0 || !strings.Contains(output.String(), `"deadAssignments"`) {
		t.Fatalf("json=%d %s", code, output)
	}
}

type interruptedDeadSettings struct{ *deadSettingsActivation }

func (owner interruptedDeadSettings) Reconcile(ctx context.Context, links releasetransition.ReleaseLinks) error {
	if err := owner.deadSettingsActivation.Reconcile(ctx, links); err != nil {
		return err
	}
	return errors.New("interrupted after atomic removal")
}

func TestDeadSettingsTransitionResumesAfterAtomicRemoval(t *testing.T) {
	for _, fault := range []string{"", "after-removal"} {
		t.Run(fault, func(t *testing.T) {
			cli, home, path, output := deadSettingsFixture(t)
			before, err := config.ReadPersistentFileSnapshot(home, path)
			if err != nil {
				t.Fatal(err)
			}
			options := releasetransition.V2Options{ConfigHome: home,
				Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"},
				ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
					return releasetransition.ReleaseLinks{Active: "release-a"}, nil
				},
				RegistryPayload:     []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`),
				ArtifactDigest:      releasetransition.Fingerprint(strings.Repeat("a", 64)),
				Reconcilers:         []releasetransition.V2ActivationReconciler{&deadSettingsActivation{cli: cli, home: home}},
				VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
			}
			if fault != "" {
				options.Reconcilers = []releasetransition.V2ActivationReconciler{interruptedDeadSettings{&deadSettingsActivation{cli: cli, home: home}}}
			}
			transition, err := releasetransition.NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
			preview, err := transition.Inspect(ctx, goal)
			if err != nil || len(preview.Blockers) != 0 || !strings.Contains(strings.Join(preview.Assessment.Consequences, "\n"), "update will remove this line") {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			outcome, err := transition.Converge(ctx, releasetransition.Execution{Plan: preview.Plan})
			if fault == "" {
				if err != nil || outcome.Status != releasetransition.StatusReady {
					t.Fatalf("outcome=%+v err=%v", outcome, err)
				}
			} else {
				if err != nil || outcome.Status != releasetransition.StatusRecovering {
					t.Fatalf("interrupted=%+v err=%v", outcome, err)
				}
				// A fresh owner/process must reconstruct the exact remaining work.
				options.Reconcilers = []releasetransition.V2ActivationReconciler{&deadSettingsActivation{cli: cli, home: home}}
				transition, err = releasetransition.NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				resume, inspectErr := transition.Inspect(ctx, goal)
				if inspectErr != nil || len(resume.Blockers) != 0 {
					t.Fatalf("resume=%+v err=%v", resume, inspectErr)
				}
				outcome, err = transition.Converge(ctx, releasetransition.Execution{Plan: resume.Plan})
				if err != nil || outcome.Status != releasetransition.StatusReady {
					t.Fatalf("resumed=%+v err=%v", outcome, err)
				}
			}
			after, err := config.ReadPersistentFileSnapshot(home, path)
			if err != nil || string(after.Content) != "# retained\n" || after.Identity.Mode != before.Identity.Mode || after.Identity.UID != before.Identity.UID || after.Identity.GID != before.Identity.GID {
				t.Fatalf("after=%+v err=%v", after, err)
			}
			if !strings.Contains(output.String(), "removed dead AGENT_sample_CONFIG assignment") {
				t.Fatal(output.String())
			}
			repeat, err := transition.Inspect(ctx, goal)
			if err != nil || len(repeat.Blockers) != 0 || repeat.Assessment.Changed {
				t.Fatalf("repeat=%+v err=%v", repeat, err)
			}
		})
	}
}

func TestDeadSettingsOwnerRefusesLiveReferenceAndStaleProof(t *testing.T) {
	cli, home, path, _ := deadSettingsFixture(t)
	owner := &deadSettingsActivation{cli: cli, home: home}
	if _, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(home, "overrides/host/agents/sample/settings.json")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, source, "{\"changed\":true}", 0600)
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); !errors.Is(err, config.ErrPersistentTargetStale) {
		t.Fatalf("reconcile=%v", err)
	}
	writeCLIFile(t, path, "AGENT_sample_CONFIG=\"$NAME/config.json\"\nDEV_UID=$OTHER\n", 0640)
	_, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err == nil || !strings.Contains(err.Error(), "only literal values") || !strings.Contains(err.Error(), "DEV_UID") {
		t.Fatalf("refusals=%v", err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "$NAME") {
		t.Fatal("refusal changed settings")
	}
}

type deadSettingsProjectedView struct {
	path    string
	content []byte
}

func (deadSettingsProjectedView) ListYards() ([]string, error) { return nil, nil }
func (view deadSettingsProjectedView) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	if path == view.path {
		return config.PersistentFileSnapshot{Exists: true, Content: view.content}, nil
	}
	return config.PersistentFileSnapshot{}, nil
}

func TestDeadSettingsOwnerUsesProspectiveHostInput(t *testing.T) {
	cli, home, path, _ := deadSettingsFixture(t)
	owner := &deadSettingsActivation{cli: cli, home: home}
	projected := []byte("DEV_UID=1100\n")
	observation, err := owner.ObserveProspective(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}, deadSettingsProjectedView{path, projected})
	digest := sha256.Sum256(projected)
	expected := releasetransition.Fingerprint(fmt.Sprintf("%x", digest))
	if err != nil || observation.Desired != expected || !observation.Converged || len(observation.Consequences) != 0 {
		t.Fatalf("prospective=%+v err=%v", observation, err)
	}
	original, _ := os.ReadFile(path)
	if !bytes.Contains(original, []byte("$SUBYARD_CONFIG_DIR")) {
		t.Fatal("prospective observation changed native settings")
	}
}

func TestDeadSettingsOwnerEmptyResultHasStableDesiredFingerprint(t *testing.T) {
	cli, home, path, _ := deadSettingsFixture(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, strings.TrimPrefix(string(content), "# retained\n"), 0640)
	owner := &deadSettingsActivation{cli: cli, home: home}
	ctx := context.Background()
	before, err := owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Reconcile(ctx, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	after, err := owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || !after.Converged || after.Desired != before.Desired || after.Actual != before.Desired {
		t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
	}
	content, err = os.ReadFile(path)
	if err != nil || len(content) != 0 {
		t.Fatalf("empty result=%q err=%v", content, err)
	}
}

func TestDeadSettingsReobservationDoesNotRenewWriteAuthority(t *testing.T) {
	cli, home, path, _ := deadSettingsFixture(t)
	owner := &deadSettingsActivation{cli: cli, home: home}
	ctx := context.Background()
	before, err := owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path+".replacement", string(content), 0640)
	if err := os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	after, err := owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || after.Desired != before.Desired || after.Binding == before.Binding {
		t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
	}
	if err := owner.Reconcile(ctx, releasetransition.ReleaseLinks{}); !errors.Is(err, config.ErrPersistentTargetStale) {
		t.Fatalf("replacement was not stale: %v", err)
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(unchanged, content) {
		t.Fatal("re-observation authorized an unreviewed edit")
	}
	// Only a fresh recovery assessment can authorize the new exact inode.
	if _, err := owner.PrepareActivationRecovery(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Reconcile(ctx, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
}
