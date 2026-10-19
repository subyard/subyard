package testvmsruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestPreparedBackendOriginalRecordRestoresSealedPayloads(t *testing.T) {
	for _, change := range []string{"none", "payload", "scope", "record", "uuid"} {
		t.Run(change, func(t *testing.T) {
			backend, uuid, calls := preparedBackendFixture(t)
			p, err := backend.PrepareApply(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			record, files, err := p.Record(backend)
			if err != nil {
				t.Fatal(err)
			}
			p.Close()
			switch change {
			case "payload":
				files["provision"] = []byte("foreign")
			case "scope":
				backend.Instance = "foreign"
			case "record":
				record.Binding = "a" + record.Binding[1:]
			case "uuid":
				*uuid = "replacement"
			}
			*calls = nil
			restored, err := backend.RestorePreparedApply(context.Background(), record, files)
			if change == "uuid" {
				if err != nil {
					t.Fatal(err)
				}
				defer restored.Close()
				if err := backend.ApplyPrepared(context.Background(), restored); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("replaced instance admitted", err)
				}
				return
			}
			if change != "none" {
				if !errors.Is(err, domain.ErrPlanStale) {
					if restored != nil {
						restored.Close()
					}
					t.Fatal("invalid original record restored", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			actual, err := io.ReadAll(io.NewSectionReader(restored.engine, 0, 32<<20))
			if err != nil || !bytes.Equal(actual, files["engine"]) || restored.Binding() != record.Binding {
				t.Fatal("original sealed payload mismatch", err)
			}
			if _, err := restored.engine.WriteAt([]byte("foreign"), 0); err == nil {
				t.Fatal("restored engine mutable")
			}
		})
	}
}
