// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// serve drains requests before canceling their reads. A forced close cancels
// vtysh and waits for every handler, including handlers behind the auth filter.
func serve(ctx context.Context, srv *http.Server, grace time.Duration) error {
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	return serveListener(ctx, srv, listener, grace)
}

func serveListener(ctx context.Context, srv *http.Server, listener net.Listener, grace time.Duration) error {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.BaseContext = func(net.Listener) context.Context { return base }
	handler := srv.Handler
	var mu sync.Mutex
	var active sync.WaitGroup
	stopping := false
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if stopping {
			mu.Unlock()
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		active.Add(1)
		mu.Unlock()
		defer active.Done()
		handler.ServeHTTP(w, r)
	})
	var err error
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(listener, "", "") }()
	select {
	case err = <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	stopping = true
	mu.Unlock()
	drain, stop := context.WithTimeout(context.Background(), grace)
	defer stop()
	if shutdownErr := srv.Shutdown(drain); shutdownErr != nil {
		cancel()
		_ = srv.Close()
	}
	_ = listener.Close()
	if err == nil {
		err = <-done
	}
	cancel()
	active.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
