package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func projectedMaterializedFixture(t *testing.T, root string) (Runtime, string, []byte) {
	t.Helper()
	runtime, source := materializedConfigFixture(t, root)
	payload := []byte("approved projected original\n")
	fd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	files := &config.ProspectiveFiles{Root: root, PinnedRoot: fd, ReadPreparedInput: func(path string) (config.PersistentFileSnapshot, bool, error) {
		if path == source {
			return config.PersistentFileSnapshot{Exists: true, Content: payload}, true, nil
		}
		return config.PersistentFileSnapshot{}, false, nil
	}, ReadSnapshot: func(path string) (config.PersistentFileSnapshot, error) {
		return config.ReadPersistentFileSnapshot(root, path)
	}}
	runtime.SourceReader = files.ReadAsset
	runtime.PreparedSources = files
	return runtime, source, payload
}

func TestMaterializedProjectedSourceOriginalRecipeFreshProcess(t *testing.T) {
	if root := os.Getenv("SUBYARD_PROJECTED_RECIPE_CHILD"); root != "" {
		runtime, _, _ := projectedMaterializedFixture(t, root)
		runtime.SourceReader = nil
		raw, err := os.ReadFile(filepath.Join(root, "original.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if !bytes.Equal(raw, p.Recipe()) {
			t.Fatal("fresh process replaced original recipe")
		}
		return
	}
	root := testkit.TempDir(t)
	runtime, source, payload := projectedMaterializedFixture(t, root)
	prepared, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	testkit.WriteFile(t, filepath.Join(root, "original.recipe"), p.Recipe(), 0o600)
	testkit.WriteFile(t, source, payload, 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestMaterializedProjectedSourceOriginalRecipeFreshProcess$", "-test.v")
	command.Env = append(os.Environ(), "SUBYARD_PROJECTED_RECIPE_CHILD="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh projected recipe: %v\n%s", err, output)
	}
}

func TestMaterializedProjectedSourcePublicationGuards(t *testing.T) {
	for _, change := range []string{"old-to-approved", "wrong-bytes", "changed-path", "root-replaced"} {
		t.Run(change, func(t *testing.T) {
			parent := testkit.TempDir(t)
			root := filepath.Join(parent, "scope")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			runtime, source, payload := projectedMaterializedFixture(t, root)
			testkit.WriteFile(t, source, []byte("old native bytes\n"), 0o600)
			prepared, err := runtime.PrepareConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p, err := runtime.PrepareMaterializedConfigs(context.Background(), prepared)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if change == "root-replaced" {
				if err := os.Rename(root, root+"-retired"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, source+".new", payload, 0o600)
			if err := os.Rename(source+".new", source); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "wrong-bytes":
				testkit.WriteFile(t, source, []byte("foreign"), 0o600)
			case "changed-path":
				other := filepath.Join(root, "other")
				testkit.WriteFile(t, other, payload, 0o600)
				runtime.Environment[1] = "HOST_CODEX_AGENTS_MD=" + other
			}
			runtime.SourceReader = nil
			bound, err := runtime.BindInstalledRuntimeContext(context.Background(), p.Recipe())
			if change == "old-to-approved" {
				if err != nil {
					t.Fatal("approved successor refused", err)
				}
				bound.Close()
			} else if !errors.Is(err, domain.ErrPlanStale) {
				if bound != nil {
					bound.Close()
				}
				t.Fatal("unapproved projected successor admitted", err)
			}
			if len(runtime.Incus.(*testkit.Incus).ExecCalls) != 0 {
				t.Fatal("source guard made guest effects")
			}
		})
	}
}

func TestMaterializedIntegrationProjectedProvisionPublication(t *testing.T) {
	for _, change := range []string{"approved", "wrong-bytes"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime, executor := materializedIntegrationFixture(t, root, true)
			source := filepath.Join(root, "provision.sh")
			payload := []byte("#!/bin/sh\n# approved projected provision\n")
			fd, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			files := &config.ProspectiveFiles{Root: root, PinnedRoot: fd, ReadPreparedInput: func(path string) (config.PersistentFileSnapshot, bool, error) {
				if path == source {
					return config.PersistentFileSnapshot{Exists: true, Content: payload}, true, nil
				}
				return config.PersistentFileSnapshot{}, false, nil
			}, ReadSnapshot: func(path string) (config.PersistentFileSnapshot, error) {
				return config.ReadPersistentFileSnapshot(root, path)
			}}
			runtime.SourceReader, runtime.PreparedSources = files.ReadAsset, files
			p := prepareMaterializedIntegrationFixture(t, runtime)
			defer p.Close()
			if change == "wrong-bytes" {
				payload = []byte("unapproved provision\n")
			}
			testkit.WriteFile(t, source+".new", payload, 0o600)
			if err := os.Rename(source+".new", source); err != nil {
				t.Fatal(err)
			}
			runtime.SourceReader = nil
			bound, err := runtime.BindInstalledRuntimeContext(context.Background(), p.Recipe())
			if change == "wrong-bytes" {
				if !errors.Is(err, domain.ErrPlanStale) || executor.commits != 0 {
					t.Fatal("unapproved provision admitted", err)
				}
				return
			}
			if err != nil {
				t.Fatal("approved provision successor refused", err)
			}
			defer bound.Close()
			if err := runtime.ApplyPreparedMaterializedIntegrations(context.Background(), bound); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(root, "result"))
			if err != nil || !bytes.Equal(body, payload) || executor.commits != 1 {
				t.Fatal("original projected provision was not executed", err)
			}
		})
	}
}
