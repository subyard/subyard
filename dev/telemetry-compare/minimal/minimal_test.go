package minimal

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/config/profiles/telemetry/collector"
	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

func TestMinimalCollectorCommitsOfficialOTLP(t *testing.T) {
	dir := testkit.TempDir(t)
	s, err := collector.Open(dir, collector.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	token := strings.Repeat("a", 32)
	auth := filepath.Join(dir, "auth")
	testkit.WriteFile(t, auth, []byte(token), 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- collector.Serve(ctx, s, collector.Options{Listen: address, AuthFile: auth, Receiver: Serve})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	r := plogotlp.NewExportRequest()
	r.Logs().ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("synthetic private body")
	raw, _ := r.MarshalProto()
	client := &http.Client{Timeout: time.Second}
	code := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		req, _ := http.NewRequest("POST", "http://"+address+"/v1/logs", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer "+token)
		response, e := client.Do(req)
		if e == nil {
			code = response.StatusCode
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatal("Collector startup failed", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code != 200 {
		t.Fatalf("Collector OTLP status %d", code)
	}
	db, err := collector.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stats, err := collector.Stats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if stats["records"].(int64) != 1 {
		t.Fatalf("ack without durable record: %+v", stats)
	}
}
