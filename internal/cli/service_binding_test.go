package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInfrastructureServiceOperationBinding(t *testing.T) {
	for _, kind := range []string{"same", "changed", "source", "malformed", "stopped"} {
		t.Run(kind, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "Running"
			if kind == "stopped" {
				instance.Status = "Stopped"
			}
			incus.Instances["subyard/yard"] = instance
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
			if err != nil {
				t.Fatal(err)
			}
			selection := infrastructureFixture(t, program, incus, "fixture-service")
			payload := filepath.Join(root, "binding.json")
			handler := filepath.Join(selection.definition.Root, "binding.sh")
			valid := fmt.Sprintf(`{"schema_version":1,"binding":%q,"facts":["synthetic-allocation generation=3 epoch=4"]}`, strings.Repeat("a", 64))
			testkit.WriteFile(t, payload, []byte(valid), 0o600)
			testkit.WriteFile(t, handler, []byte(fmt.Sprintf("#!/bin/sh\ncat %q\n", payload)), 0o755)
			selection.definition.InfrastructureService.BindingHandler = "binding.sh"
			applied := 0
			prepared := &preparedCommand{CLI: program, steps: func() []domain.OperationStep { return nil }, execute: func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
				applied++
				return domain.AdapterResult{Status: "ok"}, nil
			}}
			if kind == "malformed" {
				testkit.WriteFile(t, payload, []byte(`{"schema_version":1,"binding":"unsafe"}`), 0o600)
			}
			err = prepared.bindServiceOperation(context.Background(), selection)
			if kind == "malformed" {
				if err == nil {
					t.Fatal("malformed binding accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "changed" {
				testkit.WriteFile(t, payload, []byte(strings.ReplaceAll(valid, strings.Repeat("a", 64), strings.Repeat("b", 64))), 0o600)
			}
			if kind == "source" {
				file, err := os.OpenFile(handler, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = file.WriteString("# changed source\n")
				_ = file.Close()
			}
			_, err = prepared.execute(context.Background(), nil, io.Discard)
			if kind == "changed" || kind == "source" {
				if !errors.Is(err, domain.ErrPlanStale) || applied != 0 {
					t.Fatalf("changed scope applied: %v effects=%d", err, applied)
				}
			} else if err != nil || applied != 1 {
				t.Fatalf("exact scope failed: %v effects=%d", err, applied)
			}
		})
	}
}
