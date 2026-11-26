package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRPCErrorLogKeepsPrivateBoundedRecordsNewestLast(t *testing.T) {
	home := testkit.TempDir(t)
	log := RPCErrorLog{Home: home, Maximum: 2048}
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.FixedZone("x", 3600))
	if err := log.Append(RPCError{At: at, Request: "veranda-1", Operation: "op-1", Method: "operation.plan",
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
		if err := log.Append(RPCError{Request: fmt.Sprintf("veranda-%d", index+2), Method: "owner.inventory", Code: "internal", Error: "failed"}); err != nil {
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
	if err := (RPCErrorLog{Home: home}).Append(RPCError{Request: "r", Method: "m", Code: "c", Error: "e"}); err == nil {
		t.Fatal("a linked log was written")
	}
	if _, err := (RPCErrorLog{Home: home}).Read(1); err == nil {
		t.Fatal("a linked log was read")
	}
	if payload, _ := os.ReadFile(target); len(payload) != 0 {
		t.Fatalf("link target received %q", payload)
	}
}
