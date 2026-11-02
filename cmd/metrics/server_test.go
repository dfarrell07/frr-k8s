// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/metallb/frr-k8s/internal/tlsconfig"
)

func startServer(t *testing.T, handler http.Handler, grace time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	opt, err := tlsconfig.OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsconfig.For(opt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, TLSConfig: cfg, ReadTimeout: 100 * time.Millisecond, WriteTimeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
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
	started, release := make(chan struct{}), make(chan struct{})
	addr, stop, done := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		if r.Context().Err() != nil {
			t.Error("cancelled during grace period")
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
	stop()
	close(release)
	if got := <-response; got != "drained" {
		t.Fatal(got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIncompleteBodyCannotHoldAResponseSlot(t *testing.T) {
	addr, stop, done := startServer(t, newExporter(quiet, time.Second), time.Second)
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- test self-signed certificate
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, "GET /metrics HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("incomplete body exceeded read deadline")
		}
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
