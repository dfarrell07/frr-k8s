// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metallb/frr-k8s/internal/logging"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
)

func TestStandaloneDoesNotChangeKubernetesDefault(t *testing.T) {
	if err := logging.Init(); err != nil {
		t.Fatal(err)
	}
	if *disableMetricsAuth || *tlsClientCAFile != "" || *collectorNames != "bgp,bfd" {
		t.Fatal("deployment defaults changed")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := newMetricsHandler(false); err == nil || !strings.Contains(err.Error(), "in-cluster config") {
		t.Fatalf("default must fail closed outside Kubernetes: %v", err)
	}
	if _, err := newMetricsHandler(true); err != nil {
		t.Fatalf("standalone unexpectedly needs Kubernetes: %v", err)
	}
	for _, tc := range []struct {
		disabled      bool
		ca, cert, key string
		valid         bool
	}{
		{false, "", "", "", true}, {true, "", "", "", true}, {true, "ca", "cert", "key", true},
		{false, "ca", "cert", "key", false}, {true, "ca", "", "", false}, {true, "", "cert", "", false}, {false, "", "", "key", false},
	} {
		if err := validateAccess(tc.disabled, tc.ca, tc.cert, tc.key); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// Exercise the same controller-runtime filter used by rbacFilter, including a
// custom metrics path and an API failure. Rejected requests must never read FRR.
func TestKubernetesTokenReviewAndAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, token                        string
		authenticated, allowed, apiFailure bool
		want                               int
	}{
		{name: "anonymous", want: 401},
		{name: "invalid", token: "invalid", want: 500},
		{name: "denied", token: "denied", authenticated: true, want: 403},
		{name: "allowed", token: "allowed", authenticated: true, allowed: true, want: 200},
		{name: "api failure", token: "failure", apiFailure: true, want: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenReviews, accessReviews, reads atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var review map[string]any
				if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
					t.Error(err)
				}
				switch r.URL.Path {
				case "/apis/authentication.k8s.io/v1/tokenreviews":
					tokenReviews.Add(1)
					if tc.apiFailure {
						http.Error(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unavailable","reason":"Forbidden","code":403}`, http.StatusForbidden)
						return
					}
					if review["spec"].(map[string]any)["token"] != tc.token {
						t.Error("wrong token")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"kind": "TokenReview", "apiVersion": "authentication.k8s.io/v1", "status": map[string]any{"authenticated": tc.authenticated, "user": map[string]any{"username": "scraper"}}})
				case "/apis/authorization.k8s.io/v1/subjectaccessreviews":
					accessReviews.Add(1)
					spec := review["spec"].(map[string]any)
					attr := spec["nonResourceAttributes"].(map[string]any)
					if attr["path"] != "/custom-metrics" || attr["verb"] != "get" {
						t.Error("wrong authorization attributes", spec)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"kind": "SubjectAccessReview", "apiVersion": "authorization.k8s.io/v1", "status": map[string]any{"allowed": tc.allowed}})
				default:
					t.Error("unexpected API request", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			config := &rest.Config{Host: api.URL, Timeout: time.Second}
			filter, err := filters.WithAuthenticationAndAuthorization(config, api.Client())
			if err != nil {
				t.Fatal(err)
			}
			e := newExporter(func(ctx context.Context, command string) ([]byte, error) { reads.Add(1); return quiet(ctx, command) }, time.Second)
			handler, err := filter(logr.Discard(), e)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("GET", "/custom-metrics", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if tc.want == 200 {
				if reads.Load() == 0 || !strings.Contains(rec.Body.String(), `frrk8s_scrape_collector_success{collector="bgp"} 1`) {
					t.Fatal("authorized scrape did not collect")
				}
			} else if reads.Load() != 0 {
				t.Fatal("rejected scrape read FRR")
			}
			if tc.token == "" && tokenReviews.Load() != 0 {
				t.Error("anonymous request reached TokenReview")
			}
			if !tc.authenticated && accessReviews.Load() != 0 {
				t.Error("unauthenticated request reached authorization")
			}
		})
	}
}
