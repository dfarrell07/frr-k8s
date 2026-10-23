// SPDX-License-Identifier:Apache-2.0

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestMetricsAuthDefaultsToEnabled(t *testing.T) {
	if *disableMetricsAuth {
		t.Fatal("Kubernetes metrics authentication must remain enabled by default")
	}
}

func TestNewMetricsHandlerAuthDisabled(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler, err := newMetricsHandler(base, true, func() (metricsserver.Filter, error) {
		t.Fatal("standalone mode must not construct a Kubernetes auth filter")
		return nil, errors.New("unexpected auth construction")
	})
	if err != nil || handler == nil {
		t.Fatalf("standalone handler = %v, error = %v", handler, err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("standalone mode did not reach the supplied handler: status = %d", recorder.Code)
	}
}

func TestNewMetricsHandlerAuthErrors(t *testing.T) {
	wantErr := errors.New("auth unavailable")
	for _, test := range []struct {
		name    string
		factory func() (metricsserver.Filter, error)
	}{
		{
			name: "constructing filter",
			factory: func() (metricsserver.Filter, error) {
				return nil, wantErr
			},
		},
		{
			name: "applying filter",
			factory: func() (metricsserver.Filter, error) {
				return func(logr.Logger, http.Handler) (http.Handler, error) {
					return nil, wantErr
				}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, err := newMetricsHandler(http.NotFoundHandler(), false, test.factory)
			if !errors.Is(err, wantErr) || handler != nil {
				t.Fatalf("auth failure must not expose metrics: handler = %v, error = %v", handler, err)
			}
		})
	}
}

func TestNewMetricsHandlerUsesAuthFilter(t *testing.T) {
	calls := 0
	base := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("denied request reached the supplied handler")
	})
	handler, err := newMetricsHandler(base, false, func() (metricsserver.Filter, error) {
		calls++
		return func(_ logr.Logger, next http.Handler) (http.Handler, error) {
			if next == nil {
				t.Fatal("auth filter must wrap the Prometheus handler")
			}
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			}), nil
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if calls != 1 || recorder.Code != http.StatusForbidden || recorder.Body.Len() != 0 {
		t.Fatalf("filter calls = %d, response = %d %q", calls, recorder.Code, recorder.Body.String())
	}
}

func TestNewMetricsHandlerPassesBaseToFilter(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler, err := newMetricsHandler(base, false, func() (metricsserver.Filter, error) {
		return func(_ logr.Logger, next http.Handler) (http.Handler, error) {
			return next, nil
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("auth filter did not receive the supplied handler: status = %d", recorder.Code)
	}
}

func TestNewMetricsHandlerWithoutCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent"))

	if handler, err := newMetricsHandler(http.NotFoundHandler(), false, rbacFilter); err == nil || handler != nil {
		t.Fatalf("default mode must fail without cluster configuration: handler = %v, error = %v", handler, err)
	}
	if handler, err := newMetricsHandler(http.NotFoundHandler(), true, rbacFilter); err != nil || handler == nil {
		t.Fatalf("explicit standalone mode must work without cluster configuration: handler = %v, error = %v", handler, err)
	}
}
