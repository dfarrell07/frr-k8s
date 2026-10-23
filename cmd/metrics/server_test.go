// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/metallb/frr-k8s/internal/tlsconfig"
)

func TestServeWaitsForActiveRequests(t *testing.T) {
	for _, cause := range []string{"signal context", "certificate watcher", "listener failure"} {
		t.Run(cause, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			handlerDone := make(chan struct{})
			srv, listener, client := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				w.WriteHeader(http.StatusOK)
				close(handlerDone)
			}))
			watchResult := make(chan error, 1)
			result := make(chan error, 1)
			go func() { result <- serve(ctx, srv, listener, watchResult) }()
			requestResult := make(chan error, 1)
			go func() {
				resp, err := client.Get("https://" + listener.Addr().String())
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						err = errors.New("unexpected HTTP status")
					}
				}
				requestResult <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not start")
			}
			switch cause {
			case "signal context":
				cancel()
			case "certificate watcher":
				watchResult <- errors.New("watch registration failed")
			case "listener failure":
				listener.Close()
			}
			select {
			case err := <-result:
				t.Fatalf("serve returned before active request cleanup: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			// Sending instead of closing leaves the deferred release safe on failures.
			release <- struct{}{}
			select {
			case err := <-result:
				if (err != nil) != (cause != "signal context") {
					t.Fatalf("serve error for %s: %v", cause, err)
				}
				select {
				case <-handlerDone:
				default:
					t.Fatal("serve returned before handler completed")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("serve did not complete after active request")
			}
			if err := <-requestResult; err != nil {
				t.Fatalf("active request did not finish normally: %v", err)
			}
		})
	}
}

func TestServeClosesConnectionsOnDrainDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, handlerDone := make(chan struct{}), make(chan struct{})
	srv, listener, client := testServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(handlerDone)
	}))
	result := make(chan error, 1)
	go func() { result <- serve(ctx, srv, listener, nil) }()
	requestResult := make(chan error, 1)
	go func() {
		resp, err := client.Get("https://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		requestResult <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected shutdown timeout, got %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("serve did not force close after drain deadline")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("forced connection close did not cancel request")
	}
	if err := <-requestResult; err == nil {
		t.Fatal("expected interrupted request to fail")
	}
}

func TestServeRejectsUnexpectedWatcherCompletion(t *testing.T) {
	srv, listener, _ := testServer(t, http.NotFoundHandler())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	watchResult := make(chan error, 1)
	watchResult <- nil
	if err := serve(ctx, srv, listener, watchResult); err == nil || ctx.Err() != nil {
		t.Fatalf("expected watcher failure before timeout: serve = %v, context = %v", err, ctx.Err())
	}
}

func TestServeClosesListenerOnTLSSetupFailure(t *testing.T) {
	srv, listener, _ := testServer(t, http.NotFoundHandler())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// No certificate source makes ServeTLS fail before Serve registers the listener.
	srv.TLSConfig.Certificates = nil
	if err := serve(ctx, srv, listener, nil); err == nil || ctx.Err() != nil {
		t.Fatalf("expected TLS setup failure before timeout: serve = %v, context = %v", err, ctx.Err())
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("listener remained open after TLS setup failure")
	}
}

func testServer(t *testing.T, handler http.Handler) (*http.Server, net.Listener, *http.Client) {
	t.Helper()
	opt, err := tlsconfig.OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := tlsconfig.For(context.Background(), opt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	srv := &http.Server{TLSConfig: cfg, Handler: handler, ReadTimeout: 3 * time.Second}
	t.Cleanup(func() { srv.Close() })
	// These shutdown tests intentionally use a generated ephemeral certificate;
	// trusted supplied-certificate handshakes are covered in tlsconfig's tests.
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(transport.CloseIdleConnections)
	return srv, listener, &http.Client{Transport: transport, Timeout: 10 * time.Second}
}
