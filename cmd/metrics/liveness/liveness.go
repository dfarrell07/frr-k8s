// SPDX-License-Identifier:Apache-2.0

package liveness

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-kit/log"
)

func Handler(frrCli func(context.Context, string) ([]byte, error), logger log.Logger) http.Handler {
	slots := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "probe in progress", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		res, err := frrCli(ctx, "show daemons")
		if err != nil {
			http.Error(w, "failed to call show daemons", http.StatusInternalServerError)
			logger.Log("failed to call show daemons", err)
			return
		}
		expected := map[string]struct{}{
			"bfdd":     {},
			"bgpd":     {},
			"staticd":  {},
			"watchfrr": {},
			"zebra":    {},
		}

		runningDaemons := strings.Fields(string(res))
		for _, d := range runningDaemons {
			delete(expected, d)
		}
		if len(expected) > 0 {
			logger.Log("daemons not running. got: ", res, "missing: ", expected)
			http.NotFound(w, r)
		}
	})
}
