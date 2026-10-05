package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

type preparedArchiveFixture struct {
	payload  string
	opens    int
	closeErr error
}

func (fixture *preparedArchiveFixture) Open(context.Context, string) (io.ReadCloser, error) {
	fixture.opens++
	return &preparedArchiveInput{Reader: strings.NewReader(fixture.payload), closeErr: fixture.closeErr}, nil
}

type preparedArchiveInput struct {
	io.Reader
	closeErr error
}

func (input *preparedArchiveInput) Close() error { return input.closeErr }

func preparedArchiveExecution(t *testing.T, fixture *preparedArchiveFixture) (*CLI, *projectExecution) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, ProjectArchive: fixture})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution := &projectExecution{Loaded: loaded, Record: domain.ProjectRecord{Mode: domain.ProjectSync, HostPath: root}}
	t.Cleanup(func() {
		if err := execution.closePreparedSource(); err != nil {
			t.Fatal(err)
		}
	})
	return program, execution
}

func TestProjectPreparedArchiveReplaysOriginalInputAndCleansDraft(t *testing.T) {
	fixture := &preparedArchiveFixture{payload: "original bytes"}
	program, execution := preparedArchiveExecution(t, fixture)
	if err := execution.prepareSource(context.Background(), program); err != nil {
		t.Fatal(err)
	}
	fixture.payload = "later source bytes"
	if err := execution.prepareSource(context.Background(), program); err != nil {
		t.Fatal(err)
	}
	input, err := execution.preparedArchive.Open(context.Background(), execution.Record.HostPath)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(input)
	if err := errors.Join(err, input.Close()); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "original bytes" || fixture.opens != 1 || execution.sourceSize != int64(len(payload)) {
		t.Fatalf("payload=%q opens=%d size=%d", payload, fixture.opens, execution.sourceSize)
	}
	path, directory := execution.preparedArchive.path, execution.preparedArchive.directory
	for target, mode := range map[string]os.FileMode{path: 0o600, directory: 0o700} {
		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private draft mode=%v error=%v", mode, err)
		}
	}
	if err := execution.closePreparedSource(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("draft cleanup=%v", err)
	}
}

func TestProjectPreparedArchiveRejectsChangedBytesAndMode(t *testing.T) {
	for _, change := range []string{"bytes", "mode"} {
		t.Run(change, func(t *testing.T) {
			fixture := &preparedArchiveFixture{payload: "original"}
			program, execution := preparedArchiveExecution(t, fixture)
			if err := execution.prepareSource(context.Background(), program); err != nil {
				t.Fatal(err)
			}
			if change == "bytes" {
				testkit.WriteFile(t, execution.preparedArchive.path, []byte("tampered"), 0o600)
			} else if err := os.Chmod(execution.preparedArchive.path, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := execution.checkPreparedSource(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestProjectPreparedArchiveRejectsProducerCloseFailure(t *testing.T) {
	failure := errors.New("archive producer failed")
	fixture := &preparedArchiveFixture{payload: "partial", closeErr: failure}
	program, execution := preparedArchiveExecution(t, fixture)
	if err := execution.prepareSource(context.Background(), program); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if execution.preparedArchive != nil {
		t.Fatal("failed producer input retained")
	}
}
