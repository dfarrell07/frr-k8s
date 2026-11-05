// SPDX-License-Identifier:Apache-2.0
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metallb/frr-k8s/internal/tlsconfig"
)

// pki issues certificates from one throwaway CA.
type pki struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	pem   []byte
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &pki{ca, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns PEM cert and key for cn, usable as server (127.0.0.1) and client.
func (p *pki) issue(t *testing.T, cn string, serial int64) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type tlsFiles struct{ certFile, keyFile, clientCAFile string }

func start(t *testing.T, c tlsFiles) (string, func()) {
	t.Helper()
	opt, err := tlsconfig.OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsconfig.ForContext(t.Context(), opt, c.certFile, c.keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = withClientCA(cfg, c.clientCAFile, discard)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", newExporter(quiet, time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{Handler: mux, TLSConfig: cfg, ReadTimeout: time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Minute}
	done := make(chan error, 1)
	go func() { done <- serveListener(ctx, srv, listener, time.Second) }()
	return listener.Addr().String(), func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}
}
func get(addr string, tc *tls.Config) (*x509.Certificate, error) {
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 5 * time.Second}
	defer cl.CloseIdleConnections()
	resp, err := cl.Get("https://" + addr + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape status %d", resp.StatusCode)
	}
	return resp.TLS.PeerCertificates[0], nil
}

func TestMutualTLSAndRotation(t *testing.T) {
	p, dir := newPKI(t), t.TempDir()
	cfg := tlsFiles{certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key"), clientCAFile: filepath.Join(dir, "ca.crt")}
	crt, key := p.issue(t, "server-1", 2)
	write(t, cfg.certFile, crt)
	write(t, cfg.keyFile, key)
	write(t, cfg.clientCAFile, p.pem)

	addr, stop := start(t, cfg)
	defer stop()

	roots := x509.NewCertPool()
	roots.AddCert(p.ca)
	cc, ck := p.issue(t, "scraper", 3)
	clientPair, _ := tls.X509KeyPair(cc, ck)
	if _, err := get(addr, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientPair}, MaxVersion: tls.VersionTLS12}); err == nil {
		t.Error("TLS 1.2 was accepted")
	}

	if _, err := get(addr, &tls.Config{RootCAs: roots}); err == nil {
		t.Error("a client without a certificate was served")
	}
	other := newPKI(t)
	oc, ok := other.issue(t, "intruder", 4)
	intruder, _ := tls.X509KeyPair(oc, ok)
	if _, err := get(addr, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{intruder}}); err == nil {
		t.Error("a client certificate from another CA was served")
	}
	good := &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientPair}}
	if cert, err := get(addr, good); err != nil || cert.Subject.CommonName != "server-1" {
		t.Fatalf("scrape with a valid client certificate: cert=%v err=%v", cert, err)
	}

	// Rotate: the next handshake presents the new certificate with no restart.
	crt, key = p.issue(t, "server-2", 5)
	write(t, cfg.certFile, crt)
	write(t, cfg.keyFile, key)
	deadline := time.Now().Add(5 * time.Second)
	for {
		cert, err := get(addr, good)
		if err == nil && cert.Subject.CommonName == "server-2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server certificate did not rotate: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A corrupt rotation keeps serving the last good certificate.
	write(t, cfg.certFile, []byte("garbage"))
	if cert, err := get(addr, good); err != nil || cert.Subject.CommonName != "server-2" {
		t.Fatalf("after corrupt rotation: cert=%v err=%v", cert, err)
	}

	// Recover from the corrupt file, then rotate the client CA: the old scraper is refused, one from the new CA is served.
	np := newPKI(t)
	crt, key = p.issue(t, "server-3", 7)
	write(t, cfg.certFile, crt)
	write(t, cfg.keyFile, key)
	write(t, cfg.clientCAFile, np.pem)
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := get(addr, good); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old client CA was still trusted after reload interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
	nc, nk := np.issue(t, "scraper-2", 6)
	newPair, _ := tls.X509KeyPair(nc, nk)
	if _, err := get(addr, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{newPair}}); err != nil {
		t.Errorf("a client from the new CA was refused: %v", err)
	}
}

func TestClientCARotationForReusedAndResumedConnections(t *testing.T) {
	p, dir := newPKI(t), t.TempDir()
	cfg := tlsFiles{certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key"), clientCAFile: filepath.Join(dir, "ca.crt")}
	crt, key := p.issue(t, "server", 2)
	write(t, cfg.certFile, crt)
	write(t, cfg.keyFile, key)
	write(t, cfg.clientCAFile, p.pem)
	addr, stop := start(t, cfg)
	defer stop()
	roots := x509.NewCertPool()
	roots.AddCert(p.ca)
	cc, ck := p.issue(t, "scraper", 3)
	pair, err := tls.X509KeyPair(cc, ck)
	if err != nil {
		t.Fatal(err)
	}
	tc := &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{pair}, ClientSessionCache: tls.NewLRUClientSessionCache(4)}
	tr := &http.Transport{TLSClientConfig: tc}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	scrape := func(client *http.Client) (tls.ConnectionState, bool, error) {
		reused := false
		trace := &httptrace.ClientTrace{GotConn: func(c httptrace.GotConnInfo) { reused = c.Reused }}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, "https://"+addr+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return tls.ConnectionState{}, reused, err
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("scrape status = %d, want 200", resp.StatusCode)
		}
		return *resp.TLS, reused, nil
	}
	if state, _, err := scrape(client); err != nil || state.DidResume {
		t.Fatalf("initial handshake: resumed=%v, err=%v", state.DidResume, err)
	}
	client.CloseIdleConnections()
	if state, reused, err := scrape(client); err != nil || reused || !state.DidResume {
		t.Fatalf("resumption baseline: reused=%v, resumed=%v, err=%v", reused, state.DidResume, err)
	}
	// Replacing the CA applies to new TLS connections, including resumption.
	write(t, cfg.clientCAFile, newPKI(t).pem)
	fresh := &http.Client{Transport: &http.Transport{TLSClientConfig: tc.Clone()}, Timeout: 5 * time.Second}
	defer fresh.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for {
		fresh.CloseIdleConnections()
		if _, _, err := scrape(fresh); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old client was accepted on a new TLS connection after CA replacement")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Existing connections retain the authentication established at their handshake.
	if _, reused, err := scrape(client); err != nil || !reused {
		t.Fatalf("existing connection after CA replacement: reused=%v, err=%v", reused, err)
	}
}

func TestClientCARejectsInvalidInitialTrustAndKeepsLastGoodTrust(t *testing.T) {
	p, dir := newPKI(t), t.TempDir()
	file := filepath.Join(dir, "ca.crt")
	base := &tls.Config{MinVersion: tls.VersionTLS12, CurvePreferences: []tls.CurveID{tls.CurveP256}}
	for _, data := range [][]byte{nil, []byte("invalid")} {
		write(t, file, data)
		if _, err := withClientCA(base, file, discard); err == nil {
			t.Fatal("invalid initial CA accepted")
		}
	}
	if _, err := withClientCA(base, filepath.Join(dir, "missing"), discard); err == nil {
		t.Fatal("missing initial CA accepted")
	}
	write(t, file, p.pem)
	wrapped, err := withClientCA(base, file, discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := wrapped.GetConfigForClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert || cfg.MinVersion != base.MinVersion || cfg.CurvePreferences[0] != tls.CurveP256 {
		t.Fatal("client authentication lost existing TLS policy")
	}
	ca := &clientCA{base: base, file: file, logger: discard, interval: time.Hour}
	first, err := ca.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if got, err := ca.get(nil); err != nil || got != first {
		t.Fatal("files reloaded before interval")
	}
	ca.interval = 0
	if got, err := ca.get(nil); err != nil || got != first || ca.lastErr == "" {
		t.Fatal("deleted CA did not keep last good trust")
	}
	write(t, file, nil)
	if got, err := ca.get(nil); err != nil || got != first || ca.lastErr == "" {
		t.Fatal("empty CA disabled authentication")
	}
	write(t, file, newPKI(t).pem)
	if got, err := ca.get(nil); err != nil || got == first || ca.lastErr != "" {
		t.Fatal("valid CA did not recover")
	}
}

func TestClientCABundleRejectsEveryDamagedBlock(t *testing.T) {
	p, q := newPKI(t), newPKI(t)
	first, second := string(p.pem), string(q.pem)
	damaged := map[string]string{
		"garbage":       "garbage\n",
		"malformed PEM": "-----BEGIN CERTIFICATE-----\n!invalid-base64!\n-----END CERTIFICATE-----\n",
		"truncated PEM": "-----BEGIN CERTIFICATE-----\nMIIB\n",
		"invalid DER":   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid DER")})),
		"foreign block": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a certificate")})),
		"PEM headers":   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Unexpected": "header"}, Bytes: p.ca.Raw})),
	}
	for name, bad := range damaged {
		for position, original := range map[string]string{"prefix": bad + first, "middle": first + bad + second, "trailer": first + bad} {
			for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
				bundle := strings.ReplaceAll(original, "\n", ending.value)
				t.Run(name+"/"+position+"/"+ending.name, func(t *testing.T) {
					if _, err := clientCAPool([]byte(bundle)); err == nil {
						t.Fatal("partly damaged CA bundle accepted")
					}
				})
			}
		}
	}
}

func TestClientCABundleAcceptsAllRootsAndComments(t *testing.T) {
	p, q := newPKI(t), newPKI(t)
	bundle := " \n# first root\n\t" + string(p.pem) + string(q.pem) + "\n \t# literal -----BEGIN CERTIFICATE-----\n# end without newline"
	want := x509.NewCertPool()
	want.AddCert(p.ca)
	want.AddCert(q.ca)
	for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
		t.Run(ending.name, func(t *testing.T) {
			got, err := clientCAPool(bytes.ReplaceAll([]byte(bundle), []byte("\n"), []byte(ending.value)))
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				t.Fatal("valid bundle did not retain both roots")
			}
		})
	}
}
