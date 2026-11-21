package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSSHAgentKeyPathIsRedacted(t *testing.T) {
	for _, args := range [][]string{
		{"ssh-agent", "unlock", "--key", "/private/key", "--ttl", "2h"},
		{"ssh-agent", "unlock", "--key=/private/key", "--ttl", "2h"},
	} {
		got := redactArguments(args)
		if strings.Contains(got, "/private/key") || !strings.Contains(got, "2h") {
			t.Fatalf("unredacted key path: %s", got)
		}
	}
}

func TestWriteInvocationRedactsAndRotates(t *testing.T) {
	home := testkit.TempDir(t)
	invocation := Invocation{
		Home: home, Command: "clone", Arguments: []string{
			"https://user:password@example.test/repo?token=private#credential",
			"--", "secret",
		},
		WorkingDir: "/workspace", Yard: "named", Remote: "owner", Side: "host",
		OperationID: "op-fixture",
		Now:         time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), PID: 42, Maximum: 1,
	}
	if err := WriteInvocation(invocation); err != nil {
		t.Fatal(err)
	}
	if err := WriteInvocation(invocation); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"yard.log", "yard.log.1"} {
		data, err := os.ReadFile(filepath.Join(home, "logs", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, "password") || strings.Contains(text, "private") ||
			strings.Contains(text, "credential") || strings.Contains(text, " secret") ||
			!strings.Contains(text, "***") {
			t.Fatalf("audit redaction failed in %s: %q", name, text)
		}
		if !strings.Contains(text, " op=op-fixture ") {
			t.Fatalf("operation correlation missing in %s: %q", name, text)
		}
	}
}

func TestOperationLogCorrelatesLifecycleWithoutEventData(t *testing.T) {
	home := testkit.TempDir(t)
	sink := OperationLog{Home: home, WorkingDir: "/workspace", Yard: "named"}
	err := sink.WriteAudit(context.Background(), domain.OperationEvent{
		OperationID: "op-lifecycle", Kind: "operation.finished",
		At:   time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
		Data: map[string]any{"password": "must-not-be-logged"},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(home, "logs", "yard.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	if !strings.Contains(text, "op=op-lifecycle") || !strings.Contains(text, "operation.finished") ||
		strings.Contains(text, "must-not-be-logged") {
		t.Fatalf("unexpected operation audit: %q", text)
	}
}

func TestAuditWritersRepairBroadOwnedDirectories(t *testing.T) {
	home := testkit.TempDir(t)
	logs := filepath.Join(home, "logs")
	updates := filepath.Join(logs, "updates")
	if err := os.MkdirAll(updates, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{logs, updates} {
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteInvocation(Invocation{Home: home, Command: "status", Now: time.Now(), PID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := (UpdateHistory{Home: home}).Begin(UpdateRecord{OperationID: "op-repair", Direction: "activate"}); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{logs, updates} {
		if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s not repaired: %v %v", directory, info, err)
		}
	}
}
