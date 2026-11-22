package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func fakeCore(t *testing.T, replies ...string) string {
	t.Helper()
	path := filepath.Join(testkit.TempDir(t), "core.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, reply := range replies {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request struct {
				ID     int
				Method string
			}
			if err := json.NewDecoder(conn).Decode(&request); err != nil || request.ID != 1 || request.Method == "" {
				t.Errorf("invalid request: %v, %v", request, err)
			}
			fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":1,%s}`+"\n", reply)
			conn.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return path
}

func TestCoreObservation(t *testing.T) {
	path := fakeCore(t, `"result":{"generation":1}`, fmt.Sprintf(`"result":{"core":{"id":"fixture"},"pid":%d,"version":"0.1.21","live":2,"holders":[{"pid":42,"terminals":2,"current":false}]}`, os.Getpid()))
	core, err := readCore(path)
	if err != nil || core == nil || core.Live != 2 || core.Version != "0.1.21" || len(core.Holders) != 1 {
		t.Fatalf("observe: %+v, %v", core, err)
	}
	if core.CleanStart != "absent" {
		t.Fatalf("clean startup: %q", core.CleanStart)
	}
}

func TestCoreAbsentOrUnsupported(t *testing.T) {
	if core, err := readCore(filepath.Join(testkit.TempDir(t), "absent.sock")); core != nil || err != nil {
		t.Fatalf("absent: %+v, %v", core, err)
	}
	if _, err := readCore(fakeCore(t, `"result":{"generation":7}`)); err == nil || !strings.Contains(err.Error(), "generation 7") {
		t.Fatalf("unknown generation: %v", err)
	}
}
