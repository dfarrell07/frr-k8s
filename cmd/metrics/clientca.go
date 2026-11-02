// SPDX-License-Identifier:Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

// clientCA reloads trust for standalone scraping at TLS handshakes. Server
// certificate rotation remains owned by the existing tlsconfig certwatcher.
// A bad CA rotation keeps the last good configuration and never disables auth.
type clientCA struct {
	base     *tls.Config
	file     string
	logger   log.Logger
	interval time.Duration
	mu       sync.Mutex
	checked  time.Time
	contents string
	current  *tls.Config
	lastErr  string
}

func (c *clientCA) get(*tls.ClientHelloInfo) (*tls.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil && time.Since(c.checked) < c.interval {
		return c.current, nil
	}
	c.checked = time.Now()
	data, err := os.ReadFile(c.file)
	if err == nil && (c.current == nil || string(data) != c.contents) {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			err = fmt.Errorf("no certificates in client CA file %s", c.file)
		} else {
			cfg := c.base.Clone()
			cfg.GetConfigForClient = nil
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
			cfg.ClientCAs = pool
			c.current, c.contents = cfg, string(data)
		}
	}
	if err != nil {
		if c.current == nil {
			return nil, err
		}
		if c.lastErr != err.Error() {
			level.Error(c.logger).Log("msg", "client CA reload failed; retaining previous trust", "error", err)
		}
		c.lastErr = err.Error()
	} else if c.lastErr != "" {
		level.Info(c.logger).Log("msg", "client CA reloaded")
		c.lastErr = ""
	}
	return c.current, nil
}

func withClientCA(base *tls.Config, file string, logger log.Logger) (*tls.Config, error) {
	ca := &clientCA{base: base, file: file, logger: logger, interval: time.Second}
	if _, err := ca.get(nil); err != nil {
		return nil, err
	}
	cfg := base.Clone()
	cfg.GetConfigForClient = ca.get
	return cfg, nil
}
