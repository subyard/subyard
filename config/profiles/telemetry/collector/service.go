package collector

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	if options.Policy.Schema != PolicyVersion {
		return errors.New("unsupported capture policy")
	}
	options.Policy.Attributes = slices.Clone(options.Policy.Attributes)
	if err := validateListen(options.Listen); err != nil {
		return err
	}
	token, err := readToken(options.AuthFile)
	if err != nil {
		return err
	}
	spool := filepath.Join(s.dir, "spool")
	if options.Spool != "" && filepath.Clean(options.Spool) != spool {
		return errors.New("spool must be the data directory spool")
	}
	if err = s.publishCapturePolicy(options.Policy); err != nil {
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
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "origin_id": s.origin, "spool_failures": failures.Load()})
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
	drained := make(chan struct{})
	defer func() {
		cancel()
		<-drained
	}()
	go func() {
		defer close(drained)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := s.drainSpool(ctx, options.Policy); err != nil && !errors.Is(err, context.Canceled) {
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
