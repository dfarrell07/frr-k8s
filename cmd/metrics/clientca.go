// SPDX-License-Identifier:Apache-2.0

package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
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
		var pool *x509.CertPool
		pool, err = clientCAPool(data)
		if err == nil {
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

// clientCAPool requires every certificate PEM block to parse. Whitespace and
// # comments may separate certificates.
// Decode normally skips damaged blocks. Bound it at the next BEGIN line so every
// block in a rotation must parse before any part of the new bundle is trusted.
func clientCAPool(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	found := false
	for {
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			break
		}
		if data[0] == '#' {
			_, data, _ = bytes.Cut(data, []byte("\n"))
			continue
		}
		if !bytes.HasPrefix(data, []byte("-----BEGIN ")) {
			return nil, errors.New("invalid data in certificate PEM")
		}
		chunk := data
		if next := bytes.Index(data[1:], []byte("\n-----BEGIN ")); next >= 0 {
			chunk = data[:next+2] // Keep the LF terminating a CRLF PEM END line.
		}
		block, rest := pem.Decode(chunk)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("invalid certificate PEM block")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		pool.AddCert(cert)
		found = true
		data = data[len(chunk)-len(rest):]
	}
	if !found {
		return nil, errors.New("no certificates in PEM")
	}
	return pool, nil
}
