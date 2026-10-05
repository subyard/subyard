package application

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func projectCloneGit(t *testing.T, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native git fixture: %v\n%s", err, output)
	}
	return string(bytes.TrimSpace(output))
}

func projectCloneBare(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	source := filepath.Join(testkit.TempDir(t), "Empty.git")
	projectCloneGit(t, "init", "--bare", "--initial-branch=main", source)
	return source
}

func projectCloneAddRef(t *testing.T, source string) {
	t.Helper()
	tree := projectCloneGit(t, "-C", source, "mktree")
	commit := projectCloneGit(t, "-C", source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-m", "fixture")
	projectCloneGit(t, "-C", source, "update-ref", "refs/heads/main", commit)
}

type emptyCloneNativeData struct {
	projectProcessExecutor
	beforeClone func()
	afterClone  func()
	requests    []ports.InstanceExecRequest
}

func (data *emptyCloneNativeData) Execute(ctx context.Context, yard domain.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	data.requests = append(data.requests, request)
	clone := len(request.Command) > 1 && request.Command[0] == "git" && request.Command[1] == "clone"
	if clone && data.beforeClone != nil {
		data.beforeClone()
	}
	result, err := data.projectProcessExecutor.Execute(ctx, yard, request)
	if clone && err == nil && data.afterClone != nil {
		data.afterClone()
	}
	return result, err
}

func TestProjectCloneEmptySourceRetainsNativeUnbornPostcondition(t *testing.T) {
	for _, change := range []string{"none", "ref-before", "ref-during", "file-after", "foreign-workspace"} {
		t.Run(change, func(t *testing.T) {
			source := projectCloneBare(t)
			record := cloneRecord()
			record.HostPath = source
			record.YardPath = filepath.Join(testkit.TempDir(t), "copy", "src")
			workspace := filepath.Dir(record.YardPath)
			data := &emptyCloneNativeData{}
			switch change {
			case "ref-before":
				projectCloneAddRef(t, source)
			case "ref-during":
				data.beforeClone = func() { projectCloneAddRef(t, source) }
			case "file-after":
				data.afterClone = func() { testkit.WriteFile(t, filepath.Join(record.YardPath, "unexpected"), []byte("foreign"), 0o600) }
			case "foreign-workspace":
				if err := os.Mkdir(workspace, 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(workspace, "foreign"), []byte("retain"), 0o600)
			}
			runner := ProjectActionRunner{Data: data, Yard: domain.Context{AccessKind: domain.AccessRemote, YardName: "test"}, Project: record, CloneUnborn: true}
			err := runner.clone(context.Background())
			metadataPath := filepath.Join(workspace, ".subyard-meta.json")
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
				metadata, _ := ProjectMetadata(record, "test")
				actual, readErr := os.ReadFile(metadataPath)
				if readErr != nil || !bytes.Equal(actual, metadata) {
					t.Fatalf("verified unborn clone metadata differs: %v", readErr)
				}
				if projectCloneGit(t, "-C", record.YardPath, "symbolic-ref", "HEAD") != "refs/heads/main" {
					t.Fatal("native empty clone lost its symbolic HEAD")
				}
				return
			}
			if err == nil {
				t.Fatal("unapproved clone change accepted")
			}
			if change != "foreign-workspace" && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(metadataPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified clone metadata published: %v", err)
			}
			if change == "ref-before" {
				if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("source gained a ref before apply but workspace was written")
				}
				if len(data.requests) != 1 || !slices.Contains(data.requests[0].Command, "ls-remote") {
					t.Fatal("stale source reached a target request")
				}
			} else if change == "foreign-workspace" {
				content, err := os.ReadFile(filepath.Join(workspace, "foreign"))
				if err != nil || string(content) != "retain" {
					t.Fatal("existing foreign workspace was changed")
				}
			} else if _, err := os.Stat(record.YardPath); err != nil {
				t.Fatal("verification failure removed the partial native workspace")
			}
		})
	}
}

func TestProjectEmptyCloneSourceRefusesFailedOrMalformedAdvertisement(t *testing.T) {
	for _, result := range []ports.InstanceExecResult{{ExitCode: 1}, {Stdout: []byte("\n")}, {Stdout: []byte("malformed ref")}} {
		data := &projectDataStub{run: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) { return result, nil }}
		if err := CheckEmptyCloneSource(context.Background(), data, domain.Context{}, "fixture.git"); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("invalid advertisement accepted: %v", err)
		}
	}
}

func TestProjectClonePinnedNativeRevisionRemainsIndependentOfNewHEAD(t *testing.T) {
	source := projectCloneBare(t)
	projectCloneAddRef(t, source)
	revision := projectCloneGit(t, "-C", source, "rev-parse", "HEAD")
	tree := projectCloneGit(t, "-C", source, "mktree")
	later := projectCloneGit(t, "-C", source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-p", revision, "-m", "later")
	projectCloneGit(t, "-C", source, "update-ref", "refs/heads/main", later)
	record := cloneRecord()
	record.HostPath, record.YardPath = source, filepath.Join(testkit.TempDir(t), "copy", "src")
	runner := ProjectActionRunner{Data: projectProcessExecutor{}, Yard: domain.Context{AccessKind: domain.AccessRemote}, Project: record, CloneRevision: revision}
	if err := runner.clone(context.Background()); err != nil {
		t.Fatal(err)
	}
	if projectCloneGit(t, "-C", record.YardPath, "rev-parse", "HEAD") != revision {
		t.Fatal("native committed clone followed a newly advertised HEAD")
	}
}
