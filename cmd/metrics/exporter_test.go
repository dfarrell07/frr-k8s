// SPDX-License-Identifier:Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/metallb/frr-k8s/internal/logging"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestMain(m *testing.M) {
	if err := logging.Init(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestMetricsAuthDefaultsToEnabled(t *testing.T) {
	if *disableMetricsAuth {
		t.Fatal("Kubernetes metrics authentication must remain enabled by default")
	}
}

func TestNewMetricsHandlerAuthDisabled(t *testing.T) {
	handler, err := newMetricsHandler(true, func() (metricsserver.Filter, error) {
		t.Fatal("standalone mode must not construct a Kubernetes auth filter")
		return nil, errors.New("unexpected auth construction")
	})
	if err != nil || handler == nil {
		t.Fatalf("standalone handler = %v, error = %v", handler, err)
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
			handler, err := newMetricsHandler(false, test.factory)
			if !errors.Is(err, wantErr) || handler != nil {
				t.Fatalf("auth failure must not expose metrics: handler = %v, error = %v", handler, err)
			}
		})
	}
}

func TestNewMetricsHandlerUsesAuthFilter(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			handler, err := newMetricsHandler(false, func() (metricsserver.Filter, error) {
				calls++
				return func(_ logr.Logger, next http.Handler) (http.Handler, error) {
					if next == nil {
						t.Fatal("auth filter must wrap the Prometheus handler")
					}
					return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(status)
					}), nil
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if calls != 1 || recorder.Code != status || recorder.Body.Len() != 0 {
				t.Fatalf("filter calls = %d, response = %d %q", calls, recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestNewMetricsHandlerWithoutCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent"))

	if handler, err := newMetricsHandler(false, rbacFilter); err == nil || handler != nil {
		t.Fatalf("default mode must fail without cluster configuration: handler = %v, error = %v", handler, err)
	}
	if handler, err := newMetricsHandler(true, rbacFilter); err != nil || handler == nil {
		t.Fatalf("explicit standalone mode must work without cluster configuration: handler = %v, error = %v", handler, err)
	}
}
