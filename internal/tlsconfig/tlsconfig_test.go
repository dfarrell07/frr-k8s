// SPDX-License-Identifier:Apache-2.0

package tlsconfig

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	certutil "k8s.io/client-go/util/cert"
)

// TestForKeepsDefaultSignalHandling checks that watching certificate files
// installs no signal handler, so SIGTERM still terminates the process.
func TestForKeepsDefaultSignalHandling(t *testing.T) {
	if dir := os.Getenv("TLSCONFIG_SIGNAL_CHILD_DIR"); dir != "" {
		_, err := For(context.Background(), func(*tls.Config) {}, filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatalf("kill: %v", err)
		}
		time.Sleep(5 * time.Second)
		return // still running: SIGTERM was swallowed
	}

	dir := t.TempDir()
	certPEM, keyPEM, err := certutil.GenerateSelfSignedCertKey("test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestForKeepsDefaultSignalHandling$")
	cmd.Env = append(os.Environ(), "TLSCONFIG_SIGNAL_CHILD_DIR="+dir)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected the child to be killed by SIGTERM, got err=%v; child output:\n%s", err, out)
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("expected the child to be killed by SIGTERM, got %v; child output:\n%s", exitErr, out)
	}
}
