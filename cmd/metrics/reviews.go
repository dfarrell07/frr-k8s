// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

var errReviewsBusy = errors.New("too many Kubernetes review requests")

// newReviewClient retains the standard authentication filter and its cache.
// Cached TokenReviews outlive their callers, so limit the actual API work too.
// Return the copied config so typed clients cannot queue work before admission.
func newReviewClient(config *rest.Config) (*rest.Config, *http.Client, func(), error) {
	config = rest.CopyConfig(config)
	ctx, cancel := context.WithCancel(context.Background())
	newLimiter := func() flowcontrol.RateLimiter {
		if config.RateLimiter != nil {
			return config.RateLimiter
		}
		qps, burst := config.QPS, config.Burst
		if qps == 0 {
			qps = rest.DefaultQPS
		}
		if burst == 0 {
			burst = rest.DefaultBurst
		}
		if qps > 0 {
			return flowcontrol.NewTokenBucketRateLimiter(qps, burst)
		}
		return nil
	}
	// Preserve the separate typed clients' buckets, but check them without
	// queuing detached lookups in client-go before they reach the transport.
	tokens, access := newLimiter(), newLimiter()
	config.QPS, config.RateLimiter = -1, nil
	client, err := rest.HTTPClientFor(config)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	// HTTPClientFor may return http.DefaultClient. Never modify that shared client.
	bounded := *client
	base := bounded.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport := &reviewTransport{base: base, ctx: ctx, cancel: cancel, slots: make(chan struct{}, maxResponses), tokens: tokens, access: access}
	bounded.Transport = transport
	return config, &bounded, transport.close, nil
}

type reviewTransport struct {
	base   http.RoundTripper
	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{}
	tokens flowcontrol.RateLimiter
	access flowcontrol.RateLimiter
	mu     sync.Mutex
	closed bool
	active sync.WaitGroup
}

func (t *reviewTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, context.Canceled
	}
	select {
	case t.slots <- struct{}{}:
	default:
		t.mu.Unlock()
		return nil, errReviewsBusy
	}
	limiter := t.access
	if strings.HasSuffix(req.URL.Path, "/tokenreviews") {
		limiter = t.tokens
	}
	if limiter != nil && !limiter.TryAccept() {
		<-t.slots
		t.mu.Unlock()
		return nil, errReviewsBusy
	}
	t.active.Add(1)
	t.mu.Unlock()
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	finish := func() {
		stop()
		cancel()
		<-t.slots
		t.active.Done()
	}
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		finish()
		return nil, err
	}
	if resp.Body == nil {
		finish()
		return resp, nil
	}
	body := &reviewBody{ReadCloser: resp.Body, cancel: cancel, finish: finish}
	resp.Body = body
	// Close an unread response too: shutdown must not depend on its consumer.
	context.AfterFunc(ctx, func() { _ = body.Close() })
	return resp, nil
}

func (t *reviewTransport) close() {
	t.mu.Lock()
	t.closed = true
	t.cancel()
	t.mu.Unlock()
	t.active.Wait()
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type reviewBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	finish func()
	once   sync.Once
	err    error
}

func (b *reviewBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		_ = b.Close()
	}
	return n, err
}

func (b *reviewBody) Close() error {
	b.once.Do(func() {
		b.cancel()
		b.err = b.ReadCloser.Close()
		b.finish()
	})
	return b.err
}
