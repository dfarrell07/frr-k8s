// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"github.com/metallb/frr-k8s/internal/tlsconfig"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func startServer(t *testing.T, handler http.Handler, grace time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	opt, err := tlsconfig.OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsconfig.ForContext(t.Context(), opt, "", "")
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
func TestShutdownDrainsActiveRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	addr, stop, done := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
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
	stop()
	close(release)
	if got := <-response; got != "drained" {
		t.Fatal(got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
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
