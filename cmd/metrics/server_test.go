// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metallb/frr-k8s/cmd/metrics/collector"
	"github.com/metallb/frr-k8s/internal/tlsconfig"
	"github.com/prometheus/client_golang/prometheus"
)

func startServer(t *testing.T, handler http.Handler, grace time.Duration) (string, context.CancelFunc, <-chan error) {
	return startServerContext(t, context.Background(), handler, grace)
}

func startServerContext(t *testing.T, ctx context.Context, handler http.Handler, grace time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	opt, err := tlsconfig.OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsconfig.ForContext(t.Context(), opt, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, TLSConfig: cfg, ReadTimeout: 100 * time.Millisecond, WriteTimeout: time.Minute}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveListener(ctx, srv, listener, grace) }()
	t.Cleanup(cancel)
	return listener.Addr().String(), cancel, done
}

func TestShutdownWaitsForWrappedScrapeCleanup(t *testing.T) {
	started, cleaned := make(chan struct{}), make(chan struct{})
	e := newExporter(func(ctx context.Context, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // process reaping after cancellation
		close(cleaned)
		return nil, ctx.Err()
	}, time.Minute)
	e.cs = e.cs[:1]
	mux := http.NewServeMux()
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.ServeHTTP(w, r) }))
	addr, stop, done := startServer(t, mux, 50*time.Millisecond)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: time.Minute} // #nosec G402 -- test self-signed certificate
	defer client.CloseIdleConnections()
	go func() {
		if res, err := client.Get("https://" + addr + "/metrics"); err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scrape did not start")
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel the scrape")
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("server returned before scrape cleanup")
	}
}

func TestShutdownDrainsActiveRequest(t *testing.T) {
	for _, cause := range []error{nil, errors.New("server certificate watcher stopped")} {
		name := "signal"
		if cause != nil {
			name = "watcher failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			testShutdownDrainsActiveRequest(t, ctx, func() { cancel(cause) }, cause)
		})
	}
}

func testShutdownDrainsActiveRequest(t *testing.T, ctx context.Context, cancel context.CancelFunc, cause error) {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	shutdownStarted := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	addr, _, done := startServerContext(t, ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server := r.Context().Value(http.ServerContextKey).(*http.Server)
		server.RegisterOnShutdown(func() { close(shutdownStarted) })
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		if r.Context().Err() != nil {
			t.Error("canceled during grace period")
		}
		_, _ = io.WriteString(w, "drained")
	}), time.Second)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 5 * time.Second} // #nosec G402 -- test self-signed certificate
	defer client.CloseIdleConnections()
	response := make(chan string, 1)
	go func() {
		res, err := client.Get("https://" + addr)
		if err != nil {
			response <- err.Error()
			return
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		response <- string(body)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-shutdownStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not start")
	}
	close(release)
	if got := <-response; got != "drained" {
		t.Fatal(got)
	}
	if err := <-done; !errors.Is(err, cause) {
		t.Fatalf("shutdown returned %v, want %v", err, cause)
	}
}

func TestShutdownBeforeServingDoesNotLeaveAListener(t *testing.T) {
	addr, stop, done := startServer(t, http.NotFoundHandler(), time.Second)
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup shutdown hung")
	}
	if conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener survived shutdown")
	}
}

// This exercises actual TCP/TLS backpressure through the production server.
// Clients complete requests and then never consume their response bodies.
func TestSlowTLSClientsBoundResponsesAndRecover(t *testing.T) {
	const writeTimeout = 3 * time.Second
	var reads atomic.Int32
	large := prometheus.MustNewConstMetric(
		prometheus.NewDesc("frrk8s_test_large_response", "Test payload", []string{"payload"}, nil),
		prometheus.GaugeValue, 1, strings.Repeat("x", 4<<20),
	)
	e := newExporter(func(context.Context, string) ([]byte, error) {
		reads.Add(1)
		return nil, nil
	}, 10*time.Second)
	e.cs = []collector.Collector{{Name: "network", Collect: func(ctx context.Context, run collector.Runner) ([]prometheus.Metric, error) {
		if _, err := run(ctx, "network"); err != nil {
			return nil, err
		}
		return []prometheus.Metric{large}, nil
	}}}
	opt, err := tlsconfig.OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsconfig.ForContext(t.Context(), opt, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e, TLSConfig: cfg, ReadTimeout: time.Second, WriteTimeout: writeTimeout}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListener(ctx, srv, listener, 100*time.Millisecond) }()
	var clients []*tls.Conn
	t.Cleanup(func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server cleanup hung")
		}
	})
	for range maxResponses {
		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- local test
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
		if err := conn.NetConn().(*net.TCPConn).SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for (len(e.responses) != maxResponses || reads.Load() != int32(maxResponses) || len(e.sem) != 0) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(e.responses) != maxResponses || reads.Load() != int32(maxResponses) || len(e.sem) != 0 {
		t.Fatalf("failed to hold eight responses: slots=%d, reads=%d, collecting=%d", len(e.responses), reads.Load(), len(e.sem))
	}
	time.Sleep(100 * time.Millisecond)
	if len(e.responses) != maxResponses {
		t.Fatalf("clients did not block responses: slots=%d", len(e.responses))
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: time.Second} // #nosec G402 -- local test
	defer client.CloseIdleConnections()
	resp, err := client.Get("https://" + listener.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || reads.Load() != int32(maxResponses) {
		t.Fatalf("overload: status=%d reads=%d", resp.StatusCode, reads.Load())
	}
	cleanupStart := time.Now()
	// Go TLS may spend another five seconds closing the stalled connection.
	deadline = time.Now().Add(writeTimeout + 6*time.Second)
	for len(e.responses) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(e.responses) != 0 {
		t.Fatalf("write deadline retained %d response slots", len(e.responses))
	}
	client.Timeout = 5 * time.Second
	resp, err = client.Get("https://" + listener.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || n < 4<<20 || reads.Load() != int32(maxResponses+1) {
		t.Fatalf("recovery: status=%d bytes=%d reads=%d", resp.StatusCode, n, reads.Load())
	}
	t.Logf("8 unread TLS responses held; ninth returned 503 without reading; deadline/close-notify cleanup released all slots in %v; recovery served %d bytes", time.Since(cleanupStart), n)
}
