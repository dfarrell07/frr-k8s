// SPDX-License-Identifier:Apache-2.0

package main

import (
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestDisableMetricsAuthDefault(t *testing.T) {
	f := flag.Lookup("disable-metrics-auth")
	if f == nil || f.DefValue != "false" {
		t.Fatalf("disable-metrics-auth must exist and default to false, got %+v", f)
	}
}

func TestNewMetricsHandler(t *testing.T) {
	errAuth := errors.New("auth unavailable")
	allow := func(_ logr.Logger, next http.Handler) (http.Handler, error) { return next, nil }
	deny := func(logr.Logger, http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}), nil
	}

	tests := []struct {
		desc        string
		disableAuth bool
		filter      metricsserver.Filter
		factoryErr  error
		wantStatus  int // 0 means no handler is returned
		wantErr     error
	}{
		{desc: "auth enabled, request allowed", filter: allow, wantStatus: http.StatusNoContent},
		{desc: "auth enabled, request denied", filter: deny, wantStatus: http.StatusForbidden},
		{desc: "auth disabled", disableAuth: true, wantStatus: http.StatusNoContent},
		{desc: "filter construction fails", factoryErr: errAuth, wantErr: errAuth},
		{desc: "filter application fails", filter: func(logr.Logger, http.Handler) (http.Handler, error) { return nil, errAuth }, wantErr: errAuth},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			factoryCalled := false
			factory := func() (metricsserver.Filter, error) {
				factoryCalled = true
				return tc.filter, tc.factoryErr
			}
			base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})

			handler, err := newMetricsHandler(tc.disableAuth, base, factory)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if wantFactoryCall := !tc.disableAuth; factoryCalled != wantFactoryCall {
				t.Fatalf("auth factory called = %t, want %t", factoryCalled, wantFactoryCall)
			}
			if tc.wantStatus == 0 {
				if handler != nil {
					t.Fatalf("auth failure must not expose metrics, got handler %v", handler)
				}
				return
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
		})
	}
}
