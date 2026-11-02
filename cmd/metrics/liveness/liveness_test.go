// SPDX-License-Identifier:Apache-2.0

package liveness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-kit/log"

	"github.com/metallb/frr-k8s/internal/logging"
)

func TestLiveness(t *testing.T) {
	tests := []struct {
		desc               string
		vtyshRes           string
		vtyshError         error
		expectedStatusCode int
	}{
		{
			desc:               "regular",
			vtyshRes:           " zebra bgpd watchfrr staticd bfdd\n",
			expectedStatusCode: http.StatusOK,
		},
		{
			desc:               "returns error",
			vtyshError:         fmt.Errorf("failed to run"),
			expectedStatusCode: http.StatusInternalServerError,
		},
		{
			desc:               "less daemons",
			vtyshRes:           " zebra bgpd staticd bfdd\n",
			expectedStatusCode: http.StatusNotFound,
		},
	}

	if err := logging.InitWithWriter(os.Stdout); err != nil {
		t.Fatalf("failed to create logger %v", err)
	}
	logger := logging.GetLogger()
	logger.SetLogLevel(logging.LevelError)
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			w := httptest.NewRecorder()
			vtysh := func(_ context.Context, args string) ([]byte, error) {
				return []byte(test.vtyshRes), test.vtyshError
			}
			handler := Handler(vtysh, logger)
			handler.ServeHTTP(w, req)
			res := w.Result()
			defer res.Body.Close()
			if res.StatusCode != test.expectedStatusCode {
				t.Errorf("status code %d different from expected %d", res.StatusCode, test.expectedStatusCode)
			}
		})
	}
}

func TestLivenessBoundsConcurrentReadsAndCancelsWithRequest(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	run := func(ctx context.Context, command string) ([]byte, error) {
		if command != "show daemons" {
			t.Error("unexpected command", command)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second {
			t.Error("liveness read has no bound")
		}
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte("zebra bgpd staticd watchfrr bfdd"), nil
	}
	h := Handler(run, log.NewNopLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/livez", nil).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/livez", nil))
	if rec.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatal("overlapping probe was admitted")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("probe ignored cancellation")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/livez", nil))
	if rec.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatal("probe slot did not recover")
	}
}
