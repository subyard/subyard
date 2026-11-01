package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOwnerRuntimeSourcesRetainSharedWriterWithoutSelectedProfiles(t *testing.T) {
	root := testkit.TempDir(t)
	runtime, _ := materializedIntegrationFixture(t, root, true)
	const relative = "internal/adapters/configmaterial/tomli_w.py"
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	approved := []byte("# approved shared writer\n")
	testkit.WriteFile(t, path, approved, 0644)
	ctx := t.Context()
	p, err := runtime.PrepareOwnerRuntimeSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.FinishOwnerSources(nil)
	originalRecipe, originalBinding := p.Recipe(), p.Binding()
	shared := map[string][]byte{}
	if err := p.ShareSources(func(payload []byte) ([]byte, error) {
		key := materializedBlobPath(payload)
		if shared[key] == nil {
			shared[key] = bytes.Clone(payload)
		}
		return shared[key], nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalRecipe, p.Recipe()) || originalBinding != p.Binding() {
		t.Fatal("source sharing changed original authority")
	}
	for _, payload := range p.executionFiles {
		if len(payload) > 0 && &payload[0] != &shared[materializedBlobPath(payload)][0] {
			t.Fatal("execution retained duplicate approved source bytes")
		}
	}
	execution, err := runtime.OwnerSourceRuntime(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(execution.RepositoryRoot, relative))
	if err != nil || !bytes.Equal(actual, approved) {
		t.Fatal("retained owner runtime lost the shared writer", err)
	}
	testkit.WriteFile(t, path, []byte("# changed shared writer\n"), 0644)
	if _, err := runtime.OwnerSourceRuntime(ctx, p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed shared source admitted", err)
	}
}

func TestOwnerRuntimeSourcesRetainApprovedBytesAndCheckActualStage(t *testing.T) {
	for _, change := range []string{"after-guard", "before-guard", "stage-bytes", "stage-inode", "environment"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime, _ := materializedIntegrationFixture(t, root, true)
			p, err := runtime.PrepareOwnerRuntimeSources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer p.FinishOwnerSources(nil)
			raw := p.Recipe()
			restored, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
			if err != nil {
				t.Fatal("original source bind", err)
			}
			defer restored.FinishOwnerSources(nil)
			if !bytes.Equal(raw, restored.Recipe()) {
				t.Fatal("original owner source authority changed")
			}
			source := filepath.Join(root, "scripts/reconcile-integrations.sh")
			if change == "before-guard" {
				testkit.WriteFile(t, source, []byte("unapproved"), 0700)
			}
			execution, err := runtime.OwnerSourceRuntime(context.Background(), restored)
			if change == "before-guard" {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("changed live bytes admitted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			approved, err := os.ReadFile(filepath.Join(execution.RepositoryRoot, "scripts/reconcile-integrations.sh"))
			if err != nil {
				t.Fatal(err)
			}
			original, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "after-guard":
				testkit.WriteFile(t, source, []byte("unapproved"), 0700)
			case "stage-bytes":
				if err := os.Chmod(filepath.Dir(filepath.Join(execution.RepositoryRoot, "scripts/reconcile-integrations.sh")), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(execution.RepositoryRoot, "scripts/reconcile-integrations.sh"), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(execution.RepositoryRoot, "scripts/reconcile-integrations.sh"), []byte("unapproved"), 0500)
			case "stage-inode":
				path := filepath.Join(execution.RepositoryRoot, "scripts/reconcile-integrations.sh")
				if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path+".new", approved, 0500)
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			case "environment":
				execution.Environment = append(execution.Environment, "UNAPPROVED=\xff")
			}
			actual, err := execution.SelectedRuntimeInputs("core-guest-runtime")
			if change == "after-guard" {
				if err != nil || actual != original {
					t.Fatal("approved cached source binding changed", err)
				}
				body, err := execution.SourceReader(config.MaterializedAsset{Source: filepath.Join(execution.RepositoryRoot, "scripts/reconcile-integrations.sh")})
				if err != nil || !bytes.Equal(body, approved) {
					t.Fatal("execution recaptured changed live source", err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("unapproved stage/input accepted original source hash", err)
			}
		})
	}
}
