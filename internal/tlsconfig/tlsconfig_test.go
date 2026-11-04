// SPDX-License-Identifier:Apache-2.0

package tlsconfig

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	certutil "k8s.io/client-go/util/cert"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

func TestForContextRejectsPartialKeyPair(t *testing.T) {
	opt, err := OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range [][2]string{{"cert", ""}, {"", "key"}} {
		if _, err := ForContext(t.Context(), opt, files[0], files[1], nil); err == nil {
			t.Fatal("partial key pair silently generated a self-signed certificate")
		}
	}
	cfg, err := For(opt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 || len(cfg.Certificates) != 1 || cfg.ClientAuth != tls.NoClientCert {
		t.Fatal("default TLS configuration changed")
	}
}

func TestWatcherStartupFailureAndCancellation(t *testing.T) {
	cert, key, err := certutil.GenerateSelfSignedCertKey("localhost", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	for name, data := range map[string][]byte{certFile: cert, keyFile: key} {
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	watcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	// Start only closes its file watcher after registration succeeds. Restore
	// the file and let a final start/cancel close that resource after this test.
	t.Cleanup(func() {
		if err := os.WriteFile(certFile, cert, 0o600); err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := watcher.Start(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	// Exercise real failed registration. Shorten Start's retry window without
	// canceling the owner: a terminal watcher error must stop the server owner.
	runCertWatcher(ctx, func(ctx context.Context) error {
		registration, stop := context.WithTimeout(ctx, 50*time.Millisecond)
		defer stop()
		return watcher.Start(registration)
	}, cancel)
	if !errors.Is(context.Cause(ctx), os.ErrNotExist) || ctx.Err() == nil {
		t.Fatalf("watcher failure did not cancel its owner with the real error: %v", context.Cause(ctx))
	}
	// A signal-driven owner cancellation is expected, even if registration
	// has not completed. It must not be promoted to a watcher failure.
	stopped, stop := context.WithCancel(t.Context())
	stop()
	runCertWatcher(stopped, watcher.Start, func(err error) {
		t.Fatalf("normal cancellation reported a watcher failure: %v", err)
	})
}
