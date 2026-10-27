package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

func Serve(ctx context.Context, s *Store, options Options) error {
	if options.Listen == "" {
		options.Listen = "127.0.0.1:4318"
	}
	if options.Policy.Schema == "" {
		options.Policy = DefaultPolicy()
	}
	if err := validateListen(options.Listen); err != nil {
		return err
	}
	token, err := readToken(options.AuthFile)
	if err != nil {
		return err
	}
	spool := filepath.Join(s.dir, "outbox")
	if options.Spool != "" && filepath.Clean(options.Spool) != spool {
		return errors.New("spool must be the data directory outbox")
	}
	if err = privateDir(spool); err != nil {
		return err
	}
	socket := filepath.Join(s.dir, "control.sock")
	if st, e := os.Lstat(socket); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return errors.New("control path is not a socket")
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err = os.Chmod(socket, 0600); err != nil {
		listener.Close()
		return err
	}
	defer os.Remove(socket)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var failures atomic.Int64
	admission := make(chan struct{}, 16)
	mux := http.NewServeMux()
	upstream, _ := url.Parse("http://" + options.Listen)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	baseDirector := proxy.Director
	proxy.Director = func(r *http.Request) { baseDirector(r); r.Header.Set("Authorization", "Bearer "+token) }
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "telemetry unavailable", 503) }
	for _, path := range []string{"/v1/logs", "/v1/metrics", "/v1/traces"} {
		mux.Handle("POST "+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, s.limits.RequestBytes)
			proxy.ServeHTTP(w, r)
		}))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "origin_id": s.origin, "outbox_failures": failures.Load()})
	})
	mux.HandleFunc("POST /v1/records", func(w http.ResponseWriter, r *http.Request) {
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "telemetry capacity exceeded", http.StatusServiceUnavailable)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.limits.RequestBytes))
		if err != nil {
			http.Error(w, "record request too large", 413)
			return
		}
		data = bytes.TrimSpace(data)
		var records []Record
		if len(data) > 0 && data[0] == '[' {
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			err = dec.Decode(&records)
		} else {
			var record Record
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			err = dec.Decode(&record)
			records = []Record{record}
		}
		if err != nil {
			http.Error(w, "invalid record request", 400)
			return
		}
		if err = s.Ingest(r.Context(), records); err != nil {
			if errors.Is(err, ErrCapacity) || errors.Is(err, ErrStorage) {
				http.Error(w, "telemetry unavailable", 503)
			} else {
				http.Error(w, "invalid record", 400)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"ok\":true}\n"))
	})
	control := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	controlErrors := make(chan error, 1)
	go func() {
		err := control.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			controlErrors <- err
		}
	}()
	defer func() {
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		control.Shutdown(shutdown)
	}()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := s.IngestOutbox(ctx, spool); err != nil && !errors.Is(err, context.Canceled) {
				failures.Add(1)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	finished := make(chan error, 1)
	receive := options.Receiver
	if receive == nil {
		receive = serveOTLP
	}
	go func() { finished <- receive(ctx, s, options, &localAuth{s, token, admission}) }()
	select {
	case err = <-finished:
		return err
	case err = <-controlErrors:
		cancel()
		<-finished
		return err
	case <-ctx.Done():
		cancel()
		return <-finished
	}
}

func (s *Store) IngestOutbox(ctx context.Context, path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		count++
		if count > 256 {
			break
		}
		target := filepath.Join(path, entry.Name())
		info, err := os.Lstat(target)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > s.limits.RequestBytes {
			return errors.New("invalid outbox file")
		}
		f, err := os.OpenFile(target, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, s.limits.RequestBytes+1))
		f.Close()
		if err != nil {
			return err
		}
		var r Record
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err = dec.Decode(&r); err != nil {
			return errors.New("invalid outbox record")
		}
		if err = s.Ingest(ctx, []Record{r}); err != nil {
			return err
		}
		current, err := os.Lstat(target)
		if err != nil {
			return err
		}
		if !os.SameFile(info, current) {
			return errors.New("outbox changed during commit")
		}
		if err = os.Remove(target); err != nil {
			return err
		}
		dir, err := os.Open(path)
		if err != nil {
			return err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
