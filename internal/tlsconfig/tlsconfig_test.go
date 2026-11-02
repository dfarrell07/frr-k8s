// SPDX-License-Identifier:Apache-2.0

package tlsconfig

import (
	"context"
	"crypto/tls"
	"testing"
)

func TestForContextRejectsPartialKeyPair(t *testing.T) {
	opt, err := OptFor("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range [][2]string{{"cert", ""}, {"", "key"}} {
		if _, err := ForContext(t.Context(), opt, files[0], files[1]); err == nil {
			t.Fatal("partial key pair silently generated a self-signed certificate")
		}
	}
	cfg, err := ForContext(context.Background(), opt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 || len(cfg.Certificates) != 1 || cfg.ClientAuth != tls.NoClientCert {
		t.Fatal("default TLS configuration changed")
	}
}
