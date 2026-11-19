package collector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestNativeAdmissionRefusesSaturationBeforeDecoding(t *testing.T) {
	auth := localAuth{slots: make(chan struct{}, 1)}
	middleware, err := auth.GetHTTPHandler(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := middleware(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if err != nil {
		t.Fatal(err)
	}
	auth.slots <- struct{}{}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/logs", nil)
	handler.ServeHTTP(w, request)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("pre-decode saturation status=%d", w.Code)
	}
	<-auth.slots
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("native admission status=%d", w.Code)
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
