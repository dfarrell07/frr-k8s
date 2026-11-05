// SPDX-License-Identifier:Apache-2.0

package tlsconfig

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"

	certutil "k8s.io/client-go/util/cert"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ForContext applies TLS options and watches an explicit key pair until ctx
// ends. A watcher failure is reported to onWatcherError, or logged when nil.
// Without a key pair, it generates a self-signed certificate once.
func ForContext(ctx context.Context, tlsOpt func(*tls.Config), certFile, keyFile string, onWatcherError func(error)) (*tls.Config, error) {
	cfg := &tls.Config{}
	tlsOpt(cfg)
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("certificate and private key must be supplied together")
	}
	if certFile != "" {
		return withCertWatcher(ctx, cfg, certFile, keyFile, onWatcherError)
	}
	return withSelfSigned(cfg)
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

func withCertWatcher(ctx context.Context, cfg *tls.Config, certFile, keyFile string, onError func(error)) (*tls.Config, error) {
	cw, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("creating cert watcher: %w", err)
	}
	go runCertWatcher(ctx, cw.Start, onError)
	cfg.GetCertificate = cw.GetCertificate
	return cfg, nil
}

func runCertWatcher(ctx context.Context, start func(context.Context) error, onError func(error)) {
	if err := start(ctx); err != nil && ctx.Err() == nil {
		err = fmt.Errorf("watching server certificate: %w", err)
		if onError != nil {
			onError(err)
			return
		}
		logf.Log.Error(err, "Certificate watcher stopped")
	}
}
