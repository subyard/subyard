package audit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRPCErrorLogKeepsPrivateBoundedRecordsNewestLast(t *testing.T) {
	home := testkit.TempDir(t)
	log := RPCErrorLog{Home: home, Maximum: 2048}
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.FixedZone("x", 3600))
	if err := log.Append(context.Background(), RPCError{At: at, Request: "veranda-1", Operation: "op-1", Method: "operation.plan",
		Code: "plan_failed", Error: "plan start: line one\nline two " + strings.Repeat("x", maxRPCErrorText)}); err != nil {
		t.Fatal(err)
	}
	records, err := log.Read(10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%v err=%v", records, err)
	}
	first := records[0]
	if !first.At.Equal(at) || first.At.Location() != time.UTC || first.Request != "veranda-1" ||
		!strings.HasPrefix(first.Error, "plan start: line one\nline two ") || len(first.Error) > maxRPCErrorText+len("…") {
		t.Fatalf("record = %+v", first)
	}
	for index := 0; index < 40; index++ {
		if err := log.Append(context.Background(), RPCError{Request: fmt.Sprintf("veranda-%d", index+2), Method: "owner.inventory", Code: "internal", Error: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "logs", rpcErrorFile)
	for _, name := range []string{path, path + ".1"} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0o600 || info.Size() > 2048+maxRPCErrorText+512 {
			t.Fatalf("%s: info=%v err=%v", name, info, err)
		}
	}
	records, err = log.Read(3)
	if err != nil || len(records) != 3 || records[0].Request != "veranda-39" || records[2].Request != "veranda-41" {
		t.Fatalf("newest records = %+v err=%v", records, err)
	}
	if _, err := log.Read(0); err == nil {
		t.Fatal("a zero limit was accepted")
	}
}

func TestRPCErrorLogRefusesALinkedLog(t *testing.T) {
	home := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "elsewhere")
	testkit.WriteFile(t, target, []byte(""), 0o600)
	if err := os.Symlink(target, filepath.Join(home, "logs", rpcErrorFile)); err != nil {
		t.Fatal(err)
	}
	if err := (RPCErrorLog{Home: home}).Append(context.Background(), RPCError{Request: "r", Method: "m", Code: "c", Error: "e"}); err == nil {
		t.Fatal("a linked log was written")
	}
	if _, err := (RPCErrorLog{Home: home}).Read(1); err == nil {
		t.Fatal("a linked log was read")
	}
	if payload, _ := os.ReadFile(target); len(payload) != 0 {
		t.Fatalf("link target received %q", payload)
	}
}

func TestRPCErrorLogNarrowsAnExistingReadableLogAndItsRotation(t *testing.T) {
	// A small log is narrowed and kept; a full one is narrowed and rotated.
	for name, size := range map[string]int{"kept": 8, "rotated": 64} {
		t.Run(name, func(t *testing.T) {
			home := testkit.TempDir(t)
			path := filepath.Join(home, "logs", rpcErrorFile)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, []byte(strings.Repeat("x", size-1)+"\n"), 0o644)
			if err := (RPCErrorLog{Home: home, Maximum: 32}).Append(context.Background(), RPCError{Request: "r", Method: "m", Code: "c", Error: "e"}); err != nil {
				t.Fatal(err)
			}
			names := []string{path}
			if size > 32 {
				names = append(names, path+".1")
			}
			for _, name := range names {
				if info, err := os.Stat(name); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("%s: info=%v err=%v", name, info, err)
				}
			}
		})
	}
}

func TestRPCErrorLogGivesUpOnAHeldLockWhenItsContextEnds(t *testing.T) {
	home := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := safefs.Lock(context.Background(), filepath.Join(home, "logs", ".yard.lock"), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- (RPCErrorLog{Home: home}).Append(ctx, RPCError{Request: "r", Method: "m", Code: "c", Error: "e"})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a record was written under a held lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Append kept waiting for the lock after its context ended")
	}
	if _, err := os.Stat(filepath.Join(home, "logs", rpcErrorFile)); !os.IsNotExist(err) {
		t.Fatal("the log was written", err)
	}
}

// Not parallel: the umask is process-wide.
func TestRPCErrorLogKeepsExactModesUnderARestrictiveUmask(t *testing.T) {
	home := testkit.TempDir(t)
	for _, mask := range []int{0o400, 0o600} {
		previous := syscall.Umask(mask)
		log := RPCErrorLog{Home: home}
		for _, request := range []string{"create", "append"} {
			if err := log.Append(context.Background(), RPCError{Request: request, Method: "m", Code: "c", Error: "e"}); err != nil {
				syscall.Umask(previous)
				t.Fatalf("umask %o %s: %v", mask, request, err)
			}
		}
		syscall.Umask(previous)
		path := filepath.Join(home, "logs", rpcErrorFile)
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("umask %o: info=%v err=%v", mask, info, err)
		}
		if records, err := log.Read(10); err != nil || len(records) != 2 {
			t.Fatalf("umask %o: records=%+v err=%v", mask, records, err)
		}
		if err := os.RemoveAll(filepath.Join(home, "logs")); err != nil {
			t.Fatal(err)
		}
	}
}
