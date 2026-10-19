package testvmsruntime

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPreparedBackendEngineBytesCannotChangeAfterConsent(t *testing.T) {
	backend, _, _ := preparedBackendFixture(t)
	original, err := os.ReadFile(backend.Dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	p, err := backend.PrepareApply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	seals, err := unix.FcntlInt(p.engine.Fd(), unix.F_GET_SEALS, 0)
	want := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if err != nil || seals&want != want {
		t.Fatal("prepared engine is mutable", seals, err)
	}
	if _, err := p.engine.WriteAt([]byte("unapproved"), 0); err == nil {
		t.Fatal("sealed approved engine accepted a write")
	}
	testkit.WriteFile(t, backend.Dispatcher, []byte("unapproved source replacement"), 0o755)
	actual, err := io.ReadAll(io.NewSectionReader(p.engine, 0, 128<<20))
	if err != nil || !bytes.Equal(original, actual) {
		t.Fatal("original path mutation changed approved engine", err)
	}
	if err := backend.ApplyPrepared(context.Background(), p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("original source guard was weakened", err)
	}
}

func preparedBackendFixture(t *testing.T) (*Backend, *string, *[]string) {
	t.Helper()
	backend := fixtureBackend(t)
	uuid := "instance-uuid"
	calls := []string{}
	backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		call := strings.Join(arguments, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "config get yard-test volatile.uuid ") {
			return []byte(uuid), nil, nil
		}
		if strings.HasPrefix(call, "config get ") {
			return []byte("old"), nil, nil
		}
		if strings.HasPrefix(call, "list ") {
			return []byte("RUNNING"), nil, nil
		}
		return nil, nil, errors.New("unexpected mutation")
	}}
	return backend, &uuid, &calls
}

func TestPreparedBackendRefusesChangedInputsBeforeNativeEffects(t *testing.T) {
	for _, change := range []string{"provision", "recipe", "engine", "configuration", "instance"} {
		t.Run(change, func(t *testing.T) {
			backend, uuid, calls := preparedBackendFixture(t)
			prepared, err := backend.PrepareApply(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			switch change {
			case "provision":
				testkit.WriteFile(t, filepath.Join(backend.RepositoryRoot, "scripts/e2e-lab/provision.sh"), []byte("changed"), 0o755)
			case "recipe":
				testkit.WriteFile(t, filepath.Join(backend.RepositoryRoot, recipeFiles[0]), []byte("changed"), 0o644)
			case "engine":
				testkit.WriteFile(t, backend.Dispatcher, []byte("changed"), 0o755)
			case "configuration":
				backend.Environment["E2E_VM_SLOT_COUNT"] = "3"
			case "instance":
				*uuid = "replacement-instance"
			}
			*calls = nil
			if err := backend.ApplyPrepared(context.Background(), prepared); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed %s was not stale: %v", change, err)
			}
			for _, call := range *calls {
				if !strings.HasPrefix(call, "config get ") && !strings.HasPrefix(call, "list ") {
					t.Fatalf("unapproved native mutation: %s", call)
				}
			}
		})
	}
}

func TestPreparedBackendPublishesPinnedEngineHandle(t *testing.T) {
	backend, _, _ := preparedBackendFixture(t)
	backend.Environment["NESTED_E2E_VMS"] = "0"
	prepared, err := backend.PrepareApply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	approved, err := os.ReadFile(backend.Dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backend.Dispatcher, backend.Dispatcher+".original"); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, backend.Dispatcher, approved, 0o755)
	stop := errors.New("stop after native source observation")
	observed := false
	base := backend.Runner
	backend.Runner = &fakeRunner{handler: func(name string, arguments, environment []string, input io.Reader) ([]byte, []byte, error) {
		if len(arguments) >= 3 && arguments[0] == "file" && arguments[1] == "push" {
			if !strings.HasPrefix(arguments[2], "/proc/") || arguments[2] == backend.Dispatcher {
				t.Fatal("engine source was reopened by path")
			}
			actual, err := os.ReadFile(arguments[2])
			if err != nil || string(actual) != string(approved) {
				t.Fatalf("pinned engine bytes differ: %v", err)
			}
			observed = true
			return nil, nil, stop
		}
		return base.Run(context.Background(), name, arguments, environment, input)
	}}
	if err := backend.ApplyPrepared(context.Background(), prepared); !errors.Is(err, stop) || !observed {
		t.Fatalf("pinned source not published: %v observed=%v", err, observed)
	}
}
