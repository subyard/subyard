package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func ownerPowerFixture(t *testing.T) (Runtime, string) {
	t.Helper()
	// Include arbitrary legal pathname bytes in the native/source bindings.
	root := filepath.Join(testkit.TempDir(t), "runtime-\xff")
	for _, name := range []string{"", "bin", "config", "scripts"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "bin", "yard-engine"), []byte("approved engine bytes"), 0o755)
	testkit.WriteFile(t, filepath.Join(root, "scripts", "install-power-reconciler.sh"), []byte("#!/bin/sh\nprintf effect > \"$SUBYARD_EFFECT_PROBE\"\n"), 0o755)
	return ownerPowerRuntime(t, root, root, root, false), root
}

func ownerPowerRuntime(t *testing.T, physical, logical, fixture string, explicitPID bool) Runtime {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(physical, "bin", "yard-engine")
	if explicitPID {
		source = strings.Replace(source, "/proc/self/", fmt.Sprintf("/proc/%d/", os.Getpid()), 1)
	}
	return Runtime{RepositoryRoot: physical, LogicalRepositoryRoot: logical, Environment: []string{
		"SUBYARD_USER=" + account.Username,
		"SUBYARD_POWER_ENGINE_SOURCE=" + source,
		"SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(fixture, "installed-engine"),
		"SUBYARD_POWER_UNIT_PATH=" + filepath.Join(fixture, "installed-unit"),
		"SUBYARD_EFFECT_PROBE=" + filepath.Join(fixture, "effect-probe"),
	}}
}

func recordOwnerPower(t *testing.T, runtime Runtime) (*PreparedHostPower, OwnerHostPowerRecord) {
	t.Helper()
	p, err := runtime.PrepareHostPowerRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	record, err := runtime.RecordOwnerHostPower(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, record
}

func TestOwnerHostPowerRecordFreshProcess(t *testing.T) {
	if os.Getenv("SUBYARD_OWNER_POWER_RECORD_CHILD") == "1" {
		// The parent preparation and its memfd do not exist in this process.
		args := os.Args[len(os.Args)-2:]
		root, recordPath := args[0], args[1]
		var record OwnerHostPowerRecord
		raw, err := os.ReadFile(recordPath)
		if err != nil || json.Unmarshal(raw, &record) != nil {
			t.Fatal("read original power record", err)
		}
		// Occupy descriptors so the physical map key necessarily changes.
		for range 12 {
			file, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
		}
		file, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		runtime := ownerPowerRuntime(t, fmt.Sprintf("/proc/self/fd/%d", file.Fd()), root, root, true)
		p, err := runtime.RestoreOwnerHostPower(context.Background(), record)
		if err != nil {
			t.Fatal("restore original record", err)
		}
		defer p.Close()
		if p.Binding() != string(record.Binding) || p.NativeBefore() != string(record.NativeBefore) {
			t.Fatal("original binding or installed-before changed")
		}
		payload, err := io.ReadAll(io.NewSectionReader(p.engine, 0, 128<<20))
		if err != nil || string(payload) != "approved engine bytes" {
			t.Fatal("sealed bytes differ", err)
		}
		if _, err := p.engine.WriteAt([]byte("x"), 0); err == nil {
			t.Fatal("restored engine is mutable")
		}
		currentBefore, err := runtime.hostPowerFiles()
		if err != nil || currentBefore == p.NativeBefore() {
			t.Fatal("fixture did not change installed baseline", err)
		}
		if err := runtime.ApplyPreparedHostPowerRuntime(context.Background(), p); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatal("own installed effects replaced original baseline", err)
		}
		if _, err := os.Stat(filepath.Join(root, "effect-probe")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("restore or stale apply reached effects", err)
		}
		return
	}
	_, root := ownerPowerFixture(t)
	file, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	runtime := ownerPowerRuntime(t, fmt.Sprintf("/proc/self/fd/%d", file.Fd()), root, root, false)
	p, record := recordOwnerPower(t, runtime)
	testkit.WriteFile(t, filepath.Join(root, "installed-engine"), []byte("own installed engine"), 0o755)
	testkit.WriteFile(t, filepath.Join(root, "installed-unit"), []byte("own installed unit"), 0o644)
	afterEffects, err := runtime.RecordOwnerHostPower(p)
	if err != nil || !bytes.Equal(record.NativeBefore, afterEffects.NativeBefore) || !bytes.Equal(record.Binding, afterEffects.Binding) {
		t.Fatal("recording own effects replaced the original consent", err)
	}
	record = afterEffects
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(testkit.TempDir(t), "power-record.json")
	testkit.WriteFile(t, recordPath, raw, 0o600)
	command := exec.Command(os.Args[0], "-test.run=^TestOwnerHostPowerRecordFreshProcess$", "--", root, recordPath)
	command.Env = append(os.Environ(), "SUBYARD_OWNER_POWER_RECORD_CHILD=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, output)
	}
}

func TestOwnerHostPowerRecordRejectsChangedAuthorityAndScope(t *testing.T) {
	runtime, root := ownerPowerFixture(t)
	_, original := recordOwnerPower(t, runtime)
	for _, name := range []string{"operator", "runtime", "source", "binding", "digest", "identity", "role", "repository", "absent", "missing-before"} {
		t.Run(name, func(t *testing.T) {
			record := original
			switch name {
			case "operator":
				record.Operator = []byte("unapproved-operator")
			case "runtime":
				record.Runtime = bytes.Repeat([]byte("0"), 64)
			case "source":
				record.Source = bytes.Repeat([]byte("0"), 64)
			case "binding":
				record.Binding = bytes.Repeat([]byte("0"), 64)
			case "digest":
				record.EngineDigest = strings.Repeat("0", 64)
			case "identity":
				record.EngineIdentity = strings.Repeat("0", 64)
			case "role":
				record.EngineRole = "other-engine"
			case "repository":
				record.Repository.Inode++
			case "absent":
				record.Present = false
			case "missing-before":
				record.NativeBefore = nil
			}
			if p, err := runtime.RestoreOwnerHostPower(context.Background(), record); !errors.Is(err, domain.ErrPlanStale) || p != nil {
				t.Fatal("changed record restored", err)
			}
		})
	}
	changed := runtime
	changed.Yard.DevUser = "unapproved-developer"
	if p, err := changed.RestoreOwnerHostPower(context.Background(), original); !errors.Is(err, domain.ErrPlanStale) || p != nil {
		t.Fatal("changed runtime restored", err)
	}
	changed = runtime
	changed.PowerYards = []domain.Context{{YardName: "unapproved-yard"}}
	if p, err := changed.RestoreOwnerHostPower(context.Background(), original); !errors.Is(err, domain.ErrPlanStale) || p != nil {
		t.Fatal("changed yard scope restored", err)
	}
	otherRoot := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(otherRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "bin", "yard-engine"), filepath.Join(otherRoot, "bin", "yard-engine")); err != nil {
		t.Fatal(err)
	}
	changed = ownerPowerRuntime(t, otherRoot, root, root, false)
	if p, err := changed.RestoreOwnerHostPower(context.Background(), original); !errors.Is(err, domain.ErrPlanStale) || p != nil {
		t.Fatal("another root restored original engine role", err)
	}
	if _, err := os.Stat(filepath.Join(root, "effect-probe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal reached effects", err)
	}
}

func TestOwnerHostPowerRecordRejectsChangedSource(t *testing.T) {
	for _, change := range []string{"same-bytes-inode", "mode", "bytes", "symlink", "runtime-source", "foreign-pid"} {
		t.Run(change, func(t *testing.T) {
			runtime, root := ownerPowerFixture(t)
			p, record := recordOwnerPower(t, runtime)
			source := filepath.Join(root, "bin", "yard-engine")
			switch change {
			case "same-bytes-inode", "symlink":
				replacement := filepath.Join(root, "replacement")
				testkit.WriteFile(t, replacement, []byte("approved engine bytes"), 0o755)
				if change == "symlink" {
					link := filepath.Join(root, "replacement-link")
					if err := os.Symlink(replacement, link); err != nil {
						t.Fatal(err)
					}
					replacement = link
				}
				if err := os.Rename(replacement, source); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(source, 0o775); err != nil {
					t.Fatal(err)
				}
			case "bytes":
				testkit.WriteFile(t, source, []byte("unapproved engine bytes"), 0o755)
			case "runtime-source":
				testkit.WriteFile(t, filepath.Join(root, "config", "changed"), []byte("changed native source"), 0o600)
			case "foreign-pid":
				file, err := os.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				runtime = ownerPowerRuntime(t, fmt.Sprintf("/proc/self/fd/%d", file.Fd()), root, root, false)
				runtime.Environment[1] = fmt.Sprintf("SUBYARD_POWER_ENGINE_SOURCE=/proc/%d/fd/%d/bin/yard-engine", os.Getpid()+1, file.Fd())
			}
			if restored, err := runtime.RestoreOwnerHostPower(context.Background(), record); err == nil || restored != nil {
				t.Fatal("changed source restored", err)
			}
			if _, err := runtime.RecordOwnerHostPower(p); err == nil {
				t.Fatal("changed original preparation recorded")
			}
			if _, err := os.Stat(filepath.Join(root, "effect-probe")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("changed source reached effects", err)
			}
		})
	}
}

func TestOwnerHostPowerRecordNilCancellationAndFixedRole(t *testing.T) {
	var runtime Runtime
	record, err := runtime.RecordOwnerHostPower(nil)
	if err != nil || record.Present {
		t.Fatal("nil preparation was not absent", err)
	}
	if p, err := runtime.RestoreOwnerHostPower(context.Background(), record); err != nil || p != nil {
		t.Fatal("absent preparation did not restore nil", err)
	}
	runtime, root := ownerPowerFixture(t)
	_, record = recordOwnerPower(t, runtime)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, err := runtime.RestoreOwnerHostPower(ctx, record); !errors.Is(err, context.Canceled) || p != nil {
		t.Fatal("canceled restore retained an engine", err)
	}
	other := filepath.Join(root, "arbitrary-dispatcher")
	testkit.WriteFile(t, other, []byte("approved engine bytes"), 0o755)
	runtime.Environment[1] = "SUBYARD_POWER_ENGINE_SOURCE=" + other
	p, err := runtime.PrepareHostPowerRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := runtime.RecordOwnerHostPower(p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("arbitrary caller asset path became a durable role", err)
	}
}

func TestOwnerHostPowerRecordSupportsEngineLimitWithoutBody(t *testing.T) {
	runtime, root := ownerPowerFixture(t)
	file, err := os.OpenFile(filepath.Join(root, "bin", "yard-engine"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(128 << 20); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	// Release assets retain the existing executable-mode contract, including
	// group write bits; protected record/directory rules are separate.
	if err := os.Chmod(filepath.Join(root, "bin", "yard-engine"), 0o775); err != nil {
		t.Fatal(err)
	}
	p, record := recordOwnerPower(t, runtime)
	p.Close()
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 2048 {
		t.Fatal("engine body entered durable record", len(raw), err)
	}
	restored, err := runtime.RestoreOwnerHostPower(context.Background(), record)
	if err != nil {
		t.Fatal("supported 128 MiB engine refused", err)
	}
	defer restored.Close()
	info, err := restored.engine.Stat()
	if err != nil || info.Size() != 128<<20 {
		t.Fatal("restored engine limit changed", err)
	}
	digest, err := restored.engineDigest()
	if err != nil || digest != record.EngineDigest {
		t.Fatal("original large engine digest changed", err)
	}
}
