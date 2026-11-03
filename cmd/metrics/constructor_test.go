// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metallb/frr-k8s/internal/logging"
)

var constructorFixture = flag.Bool("constructor-fixture", false, "Run the in-cluster metrics constructor test in an isolated container with an empty service-account fixture")

const constructorServiceAccountToken = "metrics-constructor-fixture-service-account"

type constructorReviews struct {
	t                                            *testing.T
	tokens, access, active, peak, canceledTokens atomic.Int32
	canceledAccess                               atomic.Int32
	started                                      chan struct{}
	release                                      chan struct{}
}

func (a *constructorReviews) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+constructorServiceAccountToken {
		a.t.Error("review did not use the fixture service-account credentials")
		http.Error(w, "wrong API credentials", http.StatusForbidden)
		return
	}
	var review struct {
		Spec struct {
			Token                 string `json:"token"`
			User                  string `json:"user"`
			NonResourceAttributes struct {
				Path string `json:"path"`
				Verb string `json:"verb"`
			} `json:"nonResourceAttributes"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		a.t.Error(err)
		http.Error(w, "invalid review", http.StatusBadRequest)
		return
	}
	active := a.active.Add(1)
	defer a.active.Add(-1)
	for peak := a.peak.Load(); active > peak; peak = a.peak.Load() {
		if a.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/apis/authentication.k8s.io/v1/tokenreviews":
		a.tokens.Add(1)
		if review.Spec.Token == "hold-access-delayed" {
			// Authentication spends part of the scrape's budget before SAR starts.
			timer := time.NewTimer(600 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		if strings.HasPrefix(review.Spec.Token, "hold-token-") {
			a.started <- struct{}{}
			select {
			case <-a.release:
			case <-r.Context().Done():
				a.canceledTokens.Add(1)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "TokenReview", "apiVersion": "authentication.k8s.io/v1", "status": map[string]any{"authenticated": true, "user": map[string]any{"username": review.Spec.Token}}})
	case "/apis/authorization.k8s.io/v1/subjectaccessreviews":
		a.access.Add(1)
		if review.Spec.NonResourceAttributes.Path != "/metrics" || review.Spec.NonResourceAttributes.Verb != "get" {
			a.t.Error("wrong authorization attributes", review.Spec)
		}
		if strings.HasPrefix(review.Spec.User, "hold-access-") {
			a.started <- struct{}{}
			select {
			case <-a.release:
			case <-r.Context().Done():
				a.canceledAccess.Add(1)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "SubjectAccessReview", "apiVersion": "authorization.k8s.io/v1", "status": map[string]any{"allowed": review.Spec.User != "denied"}})
	default:
		a.t.Error("unexpected API request", r.URL.Path)
		http.NotFound(w, r)
	}
}

// InClusterConfig uses fixed service-account paths. The opt-in harness mounts
// its own empty directory there; ordinary unit tests must never write to them.
func TestMetricsHandlerInClusterConstructor(t *testing.T) {
	if !*constructorFixture {
		t.Skip("run bash hack/metrics-constructor-test.sh for the isolated constructor test")
	}
	const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	entries, err := os.ReadDir(serviceAccountDir)
	if err != nil {
		t.Fatal("an empty service-account fixture must be mounted:", err)
	}
	if len(entries) != 0 {
		t.Fatal("refusing to write to a nonempty service-account directory")
	}
	if err := logging.InitWithWriter(io.Discard); err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[constructorReviews]
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current.Load().ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	writeFixture := func(name string, data []byte) {
		t.Helper()
		path := filepath.Join(serviceAccountDir, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(path) })
		_, writeErr := file.Write(data)
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	writeFixture("token", []byte(constructorServiceAccountToken))
	writeFixture("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}))
	host, port, err := net.SplitHostPort(strings.TrimPrefix(api.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)

	// A new, unchanged constructor per phase also gives each phase its own
	// production TokenReview and SubjectAccessReview rate-limit buckets.
	phase := func(t *testing.T) (*exporter, *constructorReviews, *atomic.Int32, func(string, string) (int, string), func()) {
		t.Helper()
		reviews := &constructorReviews{t: t, started: make(chan struct{}, maxResponses), release: make(chan struct{})}
		current.Store(reviews)
		e, err := newMetricsHandler(false)
		if err != nil {
			t.Fatal(err)
		}
		if e.authenticated == nil || e.closeAuth == nil {
			t.Fatal("constructor omitted Kubernetes authentication or shutdown cleanup")
		}
		var reads atomic.Int32
		e.run = func(ctx context.Context, command string) ([]byte, error) {
			reads.Add(1)
			return quiet(ctx, command)
		}
		addr, stop, done := startServer(t, e, 50*time.Millisecond)
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 5 * time.Second} // #nosec G402 -- local test server
		var stopped bool
		shutdown := func() {
			if !stopped {
				stop()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(2 * time.Second):
					t.Error("metrics server did not stop")
				}
				stopped = true
			}
		}
		t.Cleanup(func() {
			// Release the fake API even when an assertion fails, so broken
			// cancellation reports a failure without hanging fixture cleanup.
			close(reviews.release)
			shutdown()
			e.closeAuth()
			client.CloseIdleConnections()
		})
		request := func(token, budget string) (int, string) {
			req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/metrics", nil)
			if err != nil {
				t.Error(err)
				return 0, ""
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if budget != "" {
				req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", budget)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return 0, ""
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Error(err)
			}
			return res.StatusCode, string(body)
		}
		return e, reviews, &reads, request, shutdown
	}

	t.Run("allowed and denied", func(t *testing.T) {
		_, reviews, reads, request, _ := phase(t)
		for _, tc := range []struct {
			token string
			code  int
		}{{"", 401}, {"denied", 403}, {"allowed", 200}} {
			code, body := request(tc.token, "")
			if code != tc.code {
				t.Fatalf("token %q: status %d, want %d: %s", tc.token, code, tc.code, body)
			}
			if code != 200 && reads.Load() != 0 {
				t.Fatal("rejected request reached FRR")
			}
			if code == 200 && (reads.Load() == 0 || !strings.Contains(body, `frrk8s_scrape_collector_success{collector="bgp"} 1`)) {
				t.Fatal("allowed request did not collect metrics")
			}
		}
		if reviews.tokens.Load() != 2 || reviews.access.Load() != 2 {
			t.Fatalf("reviews: token=%d, access=%d", reviews.tokens.Load(), reviews.access.Load())
		}
	})

	for _, budget := range []struct {
		name, header string
		timeout      time.Duration
	}{
		{"announced", "1", 8 * time.Second},
		{"configured", "", 900 * time.Millisecond},
	} {
		t.Run("authorization shares the "+budget.name+" deadline and recovers", func(t *testing.T) {
			original := *scrapeTimeout
			*scrapeTimeout = budget.timeout
			t.Cleanup(func() { *scrapeTimeout = original })
			e, reviews, reads, request, _ := phase(t)
			start := time.Now()
			if code, _ := request("hold-access-delayed", budget.header); code != http.StatusInternalServerError {
				t.Fatalf("expired authorization returned %d", code)
			}
			if elapsed := time.Since(start); elapsed < 750*time.Millisecond || elapsed > 1250*time.Millisecond {
				t.Fatalf("authentication and authorization did not share the 900ms budget: %v", elapsed)
			}
			deadline := time.Now().Add(time.Second)
			for reviews.active.Load() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if reviews.canceledAccess.Load() != 1 || reviews.active.Load() != 0 || len(e.responses) != 0 || reads.Load() != 0 {
				t.Fatal("expired authorization retained API/response work or read FRR")
			}
			if code, _ := request("recovered", ""); code != http.StatusOK || reads.Load() == 0 {
				t.Fatalf("authorization did not recover: status=%d, reads=%d", code, reads.Load())
			}
		})
	}

	t.Run("expired TokenReview waves stay bounded and shutdown cancels them", func(t *testing.T) {
		e, reviews, reads, request, shutdown := phase(t)
		finished := make(chan int, maxResponses)
		for i := range maxResponses {
			go func() { code, _ := request(fmt.Sprintf("hold-token-%d", i), "1"); finished <- code }()
		}
		for range maxResponses {
			select {
			case <-reviews.started:
			case <-time.After(2 * time.Second):
				t.Fatal("eight TokenReviews did not reach the fake API")
			}
		}
		if code, _ := request("overloaded", "1"); code != http.StatusServiceUnavailable {
			t.Fatalf("ninth active scrape returned %d", code)
		}
		for range maxResponses {
			select {
			case code := <-finished:
				if code != http.StatusInternalServerError {
					t.Fatalf("expired authentication returned %d", code)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("authentication outlived its scrape budget")
			}
		}
		// Refill the default 5 QPS / burst 10 bucket. Otherwise rate limiting
		// could hide unbounded detached API calls in the later request waves.
		time.Sleep(2 * time.Second)
		for wave := range 2 {
			for i := range maxResponses {
				go func() { code, _ := request(fmt.Sprintf("wave-%d-%d", wave, i), "1"); finished <- code }()
			}
			for range maxResponses {
				select {
				case code := <-finished:
					if code != http.StatusInternalServerError {
						t.Fatalf("wave %d: excess authentication returned %d", wave, code)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("excess authentication queued")
				}
			}
			if reviews.tokens.Load() != maxResponses || reviews.active.Load() != maxResponses || reviews.peak.Load() != maxResponses || reviews.access.Load() != 0 || len(e.responses) != 0 || reads.Load() != 0 {
				t.Fatalf("wave %d escaped admission: token=%d active=%d peak=%d access=%d responses=%d reads=%d", wave, reviews.tokens.Load(), reviews.active.Load(), reviews.peak.Load(), reviews.access.Load(), len(e.responses), reads.Load())
			}
		}
		shutdown()
		if reviews.active.Load() != maxResponses {
			t.Fatal("test did not retain detached TokenReviews after serving stopped")
		}
		closed := make(chan struct{})
		go func() { e.closeAuth(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("closeAuth did not drain Kubernetes API work")
		}
		deadline := time.Now().Add(time.Second)
		for reviews.active.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if reviews.active.Load() != 0 || reviews.canceledTokens.Load() != maxResponses || reads.Load() != 0 {
			t.Fatalf("shutdown retained API work: active=%d canceled=%d reads=%d", reviews.active.Load(), reviews.canceledTokens.Load(), reads.Load())
		}
		t.Log("three request waves retained exactly eight API calls; closeAuth canceled all eight after serving stopped")
	})
}
