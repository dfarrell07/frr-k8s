// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
)

func TestReviewClientKeepsConfigAndDoesNotQueueRateLimits(t *testing.T) {
	original := &rest.Config{Host: "https://example.invalid"}
	config, client, closeClient, err := newReviewClient(original)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	if config == original || original.RateLimiter != nil || original.QPS != 0 || original.Burst != 0 {
		t.Fatal("review client mutated the caller's configuration")
	}
	transport := client.Transport.(*reviewTransport)
	if client == http.DefaultClient || transport.tokens.QPS() != rest.DefaultQPS || transport.access.QPS() != rest.DefaultQPS {
		t.Fatal("review client lost the default rate limit or reused the global client")
	}
	if config.QPS >= 0 || config.RateLimiter != nil {
		t.Fatal("typed clients can queue detached lookups before API admission")
	}
	for range rest.DefaultBurst {
		if !transport.tokens.TryAccept() {
			t.Fatal("default TokenReview burst was reduced")
		}
	}
	start := time.Now()
	if _, err := client.Post("https://example.invalid/apis/authentication.k8s.io/v1/tokenreviews", "application/json", nil); !errors.Is(err, errReviewsBusy) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("rate limiter queued an excess lookup: %v", err)
	}
	if !transport.access.TryAccept() {
		t.Fatal("TokenReviews consumed SubjectAccessReview admission")
	}
	closeClient()
	if _, err := client.Get("https://example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed review client admitted a lookup: %v", err)
	}
}

func TestReviewTransportBoundsUnreadBodiesAndCancelsOnShutdown(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/complete" {
			_, _ = io.WriteString(w, "complete")
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer api.Close()
	_, client, closeClient, err := newReviewClient(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	transport := client.Transport.(*reviewTransport)
	var responses []*http.Response
	for range maxResponses {
		res, err := client.Get(api.URL + "/slow")
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, res)
		defer res.Body.Close()
	}
	if _, err := client.Get(api.URL + "/excess"); !errors.Is(err, errReviewsBusy) || calls.Load() != maxResponses {
		t.Fatalf("response headers released API admission: calls=%d, error=%v", calls.Load(), err)
	}
	if err := responses[0].Body.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := client.Get(api.URL + "/complete")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := io.ReadAll(res.Body); err != nil || string(body) != "complete" {
		t.Fatalf("recovery body=%q, error=%v", body, err)
	}
	if len(transport.slots) != maxResponses-1 {
		t.Fatal("EOF did not release API admission")
	}
	_ = res.Body.Close()
	closed := make(chan struct{})
	go func() { closeClient(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for consumers of unread response bodies")
	}
	if len(transport.slots) != 0 {
		t.Fatal("shutdown retained API admission")
	}
	before := calls.Load()
	if _, err := client.Get(api.URL); !errors.Is(err, context.Canceled) || calls.Load() != before {
		t.Fatalf("shutdown admitted API work: %v", err)
	}
}

func TestDefaultReviewBurstServesEightHealthyScrapers(t *testing.T) {
	var tokens, access atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var review map[string]any
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			t.Error(err)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/tokenreviews") {
			tokens.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "TokenReview", "apiVersion": "authentication.k8s.io/v1", "status": map[string]any{"authenticated": true, "user": map[string]any{"username": review["spec"].(map[string]any)["token"]}}})
			return
		}
		access.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "SubjectAccessReview", "apiVersion": "authorization.k8s.io/v1", "status": map[string]any{"allowed": true}})
	}))
	defer api.Close()
	config, client, closeClient, err := newReviewClient(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	filter, err := filters.WithAuthenticationAndAuthorization(config, client)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := filter(logr.Discard(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan int, maxResponses)
	for i := range maxResponses {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.Header.Set("Authorization", fmt.Sprintf("Bearer healthy-%d", i))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			finished <- rec.Code
		}()
	}
	for range maxResponses {
		select {
		case code := <-finished:
			if code != http.StatusOK {
				t.Fatalf("healthy default burst returned %d", code)
			}
		case <-time.After(time.Second):
			t.Fatal("healthy default burst stalled")
		}
	}
	if tokens.Load() != maxResponses || access.Load() != maxResponses {
		t.Fatalf("reviews: token=%d, access=%d", tokens.Load(), access.Load())
	}
}

func TestCachedTokenReviewsRemainBoundedAcrossRequestDeadlines(t *testing.T) {
	started := make(chan struct{}, maxResponses)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAPI := func() { releaseOnce.Do(func() { close(release) }) }
	var calls, reads atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var review map[string]any
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			t.Error(err)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/tokenreviews") {
			token := review["spec"].(map[string]any)["token"].(string)
			if strings.HasPrefix(token, "blocked-") {
				started <- struct{}{}
				select {
				case <-r.Context().Done():
					return
				case <-release:
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "TokenReview", "apiVersion": "authentication.k8s.io/v1", "status": map[string]any{"authenticated": true, "user": map[string]any{"username": token}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "SubjectAccessReview", "apiVersion": "authorization.k8s.io/v1", "status": map[string]any{"allowed": true}})
	}))
	defer api.Close()
	defer releaseAPI()
	// Avoid rate throttling here so the independent transport limit is exercised.
	config, client, closeClient, err := newReviewClient(&rest.Config{Host: api.URL, QPS: 1000, Burst: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	filter, err := filters.WithAuthenticationAndAuthorization(config, client)
	if err != nil {
		t.Fatal(err)
	}
	e := newExporter(func(ctx context.Context, command string) ([]byte, error) {
		reads.Add(1)
		return quiet(ctx, command)
	}, 300*time.Millisecond)
	e.authenticated, err = filter(logr.Discard(), http.HandlerFunc(e.serveMetrics))
	if err != nil {
		t.Fatal(err)
	}
	request := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	finished := make(chan int, maxResponses)
	for i := range maxResponses {
		go func() { finished <- request(fmt.Sprintf("blocked-%d", i)) }()
	}
	for range maxResponses {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("cached TokenReviews did not reach the API")
		}
	}
	for range maxResponses {
		select {
		case code := <-finished:
			if code != http.StatusInternalServerError {
				t.Fatalf("timed-out authentication returned %d", code)
			}
		case <-time.After(time.Second):
			t.Fatal("authentication outlived its request budget")
		}
	}
	if len(e.responses) != 0 || len(client.Transport.(*reviewTransport).slots) != maxResponses {
		t.Fatal("test did not retain detached API work after releasing handlers")
	}
	for i := range maxResponses {
		if code := request(fmt.Sprintf("excess-%d", i)); code != http.StatusInternalServerError {
			t.Fatalf("excess authentication returned %d", code)
		}
	}
	if calls.Load() != maxResponses || reads.Load() != 0 {
		t.Fatal("new request deadlines bypassed API admission or reached FRR")
	}
	releaseAPI()
	deadline := time.Now().Add(time.Second)
	for len(client.Transport.(*reviewTransport).slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if code := request("healthy"); code != http.StatusOK || reads.Load() == 0 {
		t.Fatalf("authentication did not recover: status=%d, FRR reads=%d", code, reads.Load())
	}
}
