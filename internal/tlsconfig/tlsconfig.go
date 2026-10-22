// SPDX-License-Identifier:Apache-2.0

package tlsconfig

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"

	certutil "k8s.io/client-go/util/cert"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

// For builds a *tls.Config by applying the given TLS option function.
// If certFile and keyFile are provided, a background certwatcher goroutine
// is started with ctx to hot-reload them on change. Its result channel must be
// monitored for unexpected termination. With no certificate pair, a self-signed
// certificate is generated in memory once and the result channel is nil.
func For(ctx context.Context, tlsOpt func(*tls.Config), certFile, keyFile string) (*tls.Config, <-chan error, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, nil, fmt.Errorf("certificate and private key must be provided together")
	}
	cfg := &tls.Config{}
	tlsOpt(cfg)

	if certFile != "" && keyFile != "" {
		return withCertWatcher(ctx, cfg, certFile, keyFile)
	}
	cfg, err := withSelfSigned(cfg)
	return cfg, nil, err
}

// selfSignedCert generates a self-signed TLS certificate in memory using
// the hostname as the CN. Copied from kube-rbac-proxy self-signed cert logic:
// https://github.com/brancz/kube-rbac-proxy/blob/v0.21.0/cmd/kube-rbac-proxy/app/kube-rbac-proxy.go#L327-L340
func selfSignedCert() (tls.Certificate, error) {
	host, err := os.Hostname()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("getting hostname: %w", err)
	}
	certBytes, keyBytes, err := certutil.GenerateSelfSignedCertKey(host, nil, nil)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating self-signed cert: %w", err)
	}
	cert, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("loading self-signed cert: %w", err)
	}
	return cert, nil
}

func withSelfSigned(cfg *tls.Config) (*tls.Config, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("generating self-signed cert: %w", err)
	}
	cfg.Certificates = []tls.Certificate{cert}
	return cfg, nil
}

func withCertWatcher(ctx context.Context, cfg *tls.Config, certFile, keyFile string) (*tls.Config, <-chan error, error) {
	cw, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("creating cert watcher: %w", err)
	}
	cfg.GetCertificate = cw.GetCertificate
	return cfg, startCertWatcher(ctx, cw), nil
}

func startCertWatcher(ctx context.Context, cw *certwatcher.CertWatcher) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- cw.Start(ctx)
		close(result)
	}()
	return result
}
