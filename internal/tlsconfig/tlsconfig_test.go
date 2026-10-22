// SPDX-License-Identifier:Apache-2.0

package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	certutil "k8s.io/client-go/util/cert"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

func TestForCertificatePair(t *testing.T) {
	opt, err := OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ cert, key string }{{"cert.pem", ""}, {"", "key.pem"}} {
		if _, _, err := For(context.Background(), opt, tc.cert, tc.key); err == nil {
			t.Fatalf("accepted incomplete certificate pair: %+v", tc)
		}
	}
	cfg, watcher, err := For(context.Background(), opt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if watcher != nil || len(cfg.Certificates) != 1 || cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("unexpected self-signed config: %+v, watcher %v", cfg, watcher)
	}
}

func TestSuppliedCertificateTrustAndReload(t *testing.T) {
	cert1, key1 := testCertificate(t)
	cert2, key2 := testCertificate(t)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeReplacement(t, certPath, cert1)
	writeReplacement(t, keyPath, key1)
	ctx, cancel := context.WithCancel(context.Background())
	opt, err := OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, result, err := For(ctx, opt, certPath, keyPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Errorf("watcher cancellation: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("watcher did not stop on cancellation")
		}
	})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	server.TLS = cfg
	server.StartTLS()
	defer server.Close()

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert1)
	roots.AppendCertsFromPEM(cert2)
	clientCfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	dial := func() (*x509.Certificate, error) {
		conn, err := tls.Dial("tcp", server.Listener.Addr().String(), clientCfg)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0], nil
	}
	first, err := dial()
	if err != nil {
		t.Fatalf("trusted supplied certificate: %v", err)
	}
	untrusted := clientCfg.Clone()
	untrusted.RootCAs = x509.NewCertPool()
	if conn, err := tls.Dial("tcp", server.Listener.Addr().String(), untrusted); err == nil {
		conn.Close()
		t.Fatal("untrusted certificate accepted")
	}

	// Rotation can briefly present a mismatched pair. Fresh handshakes must
	// continue using the last valid certificate until both files are replaced.
	writeReplacement(t, certPath, cert2)
	for range 3 {
		got, err := dial()
		if err != nil || !got.Equal(first) {
			t.Fatalf("mismatched replacement did not retain original cert: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	writeReplacement(t, keyPath, key2)
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := dial()
		if err == nil && !got.Equal(first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("certificate was not reloaded: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCertificateWatcherRegistrationFailure(t *testing.T) {
	cert, key := testCertificate(t)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeReplacement(t, certPath, cert)
	writeReplacement(t, keyPath, key)
	watcher, err := certwatcher.New(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the initial pair before Start registers watches. A successful
	// initial parse must not conceal failure to begin watching for rotation.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	result := startCertWatcher(context.Background(), watcher)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("watch registration unexpectedly succeeded")
		}
	case <-time.After(12 * time.Second):
		t.Fatal("watch registration failure was not reported")
	}
}

func testCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	cert, key, err := certutil.GenerateSelfSignedCertKey("localhost", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func writeReplacement(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
