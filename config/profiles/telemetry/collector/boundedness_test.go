package collector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestPrivateRecordsAdmissionPrecedesBodyAndSharesReceiverBudget(t *testing.T) {
	s := fixtureStore(t)
	authFile := filepath.Join(s.dir, "auth")
	testkit.WriteFile(t, authFile, []byte(strings.Repeat("a", 32)), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan *LocalAuth, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, s, Options{Listen: "127.0.0.1:0", AuthFile: authFile,
			Receiver: func(ctx context.Context, _ *Store, _ Options, auth *LocalAuth) error {
				ready <- auth
				<-ctx.Done()
				return nil
			}})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	var auth *LocalAuth
	select {
	case auth = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("receiver readiness deadline")
	}
	for range cap(auth.slots) {
		auth.slots <- struct{}{}
	}
	connection, err := net.Dial("unix", filepath.Join(s.dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(time.Second))
	// No body is sent. A 100 Continue would prove decoding/read started before
	// admission; saturation must instead reject immediately and retryably.
	if _, err = connection.Write([]byte("POST /v1/records HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 || response.Header.Get("Retry-After") != "1" {
		t.Fatalf("prebody saturation status=%d", response.StatusCode)
	}
	<-auth.slots
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(s.dir, "control.sock"))
	}}}
	defer client.CloseIdleConnections()
	body, _ := json.Marshal(record("run", map[string]any{"agent_run_id": "admitted"}))
	response, err = client.Post("http://localhost/v1/records", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || !s.RegisteredRun(context.Background(), "admitted") {
		t.Fatal("record acknowledgement preceded durable commit")
	}
}

func TestQueryRejectsOversizedNativeScalarBeforeReturningSmallResult(t *testing.T) {
	s := fixtureStore(t)
	db, err := OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// At most 16MiB+1 on the old path, never a harmful allocation. Wrapping in
	// length makes the result tiny: a post-Scan byte check cannot catch this.
	for _, expression := range []string{"zeroblob(16777217)", "randomblob(16777217)", "printf('%16777217s','x')"} {
		t.Run(expression, func(t *testing.T) {
			_, err := Query(context.Background(), db, "SELECT length("+expression+") AS n")
			var native *sqlite.Error
			if !errors.As(err, &native) || native.Code() != sqlite3.SQLITE_TOOBIG {
				t.Fatalf("native scalar was not rejected before materialization: %v", err)
			}
		})
	}
	result, err := Query(context.Background(), db, "SELECT length(randomblob(1024)) AS n")
	if err != nil || len(result) != 1 || result[0]["n"] != int64(1024) {
		t.Fatalf("bounded scalar query failed: %v", err)
	}
}

func TestQueryNativeLimitIsConnectionLocalAndPreservesStricterLimit(t *testing.T) {
	s := fixtureStore(t)
	db, err := OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := sqlite.Limit(connection, sqlite3.SQLITE_LIMIT_LENGTH, -1)
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Query(ctx, db, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if _, err = Query(ctx, db, "SELECT length(randomblob(16777217))"); err == nil {
		t.Fatal("oversized query unexpectedly succeeded")
	}
	connection, err = db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sqlite.Limit(connection, sqlite3.SQLITE_LIMIT_LENGTH, 4096)
	connection.Close()
	if err != nil || got != initial {
		t.Fatal("query changed pooled connection limit", err)
	}
	if _, err = Query(ctx, db, "SELECT length(randomblob(8192))"); err == nil {
		t.Fatal("query raised the caller's stricter native limit")
	}
	connection, err = db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	got, err = sqlite.Limit(connection, sqlite3.SQLITE_LIMIT_LENGTH, -1)
	if err != nil || got != 4096 {
		t.Fatal("stricter connection limit was not preserved", err)
	}
}
