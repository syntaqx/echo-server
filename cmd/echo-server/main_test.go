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
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func runCmd(t *testing.T, ctx context.Context, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := run(ctx, args, &out, &out)
	return code, out.String()
}

func TestCommandErrors(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		env  map[string]string
		args []string
		code int
		out  string
	}{
		{name: "help", args: []string{"-h"}, code: 0, out: "-route-prefix"},
		{name: "healthcheck help", args: []string{"healthcheck", "-h"}, code: 0, out: "-url"},
		{name: "unknown command", args: []string{"bogus"}, code: 2, out: "unknown command"},
		{name: "unknown flag", args: []string{"-nope"}, code: 2},
		{name: "stray argument", args: []string{"serve", "extra"}, code: 2, out: "unexpected arguments"},
		{name: "half TLS", args: []string{"-tls-cert", "x.pem"}, code: 2, out: "both -tls-cert and -tls-key"},
		{name: "bad prefix", args: []string{"-route-prefix", "nope"}, code: 2, out: "invalid route prefix"},
		{name: "bad env", env: map[string]string{"ECHO_MAX_DELAY": "soon"}, code: 2, out: `invalid ECHO_MAX_DELAY="soon"`},
		{name: "bad log format", args: []string{"-addr", "127.0.0.1:0", "-log-format", "xml"}, code: 1, out: "invalid log format"},
		{name: "bad log level", args: []string{"-addr", "127.0.0.1:0", "-log-level", "loud"}, code: 1, out: "invalid log level"},
		{name: "missing TLS files", args: []string{"-addr", "127.0.0.1:0", "-tls-cert", "nope.pem", "-tls-key", "nope.key"}, code: 1, out: "loading TLS key pair"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			code, out := runCmd(t, ctx, tt.args...)
			if code != tt.code {
				t.Errorf("exit = %d, want %d\n%s", code, tt.code, out)
			}
			if !strings.Contains(out, tt.out) {
				t.Errorf("output missing %q:\n%s", tt.out, out)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	version = "v9.9.9"
	t.Cleanup(func() { version = "" })
	if code, out := runCmd(t, context.Background(), "version"); code != 0 || strings.TrimSpace(out) != "v9.9.9" {
		t.Errorf("version = %d %q", code, out)
	}
}

func TestEnvOverridesAndFlagsWin(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("ECHO_MAX_BODY", "42")
	t.Setenv("ECHO_ACCESS_LOG", "false")
	cfg, err := parseConfig("serve", []string{"-max-body", "7"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != ":9999" || cfg.maxBody != 7 || cfg.accessLog {
		t.Errorf("cfg = %+v", cfg)
	}
	t.Setenv("ECHO_ADDR", "127.0.0.1:7000")
	if cfg, _ := parseConfig("serve", nil, io.Discard); cfg.addr != "127.0.0.1:7000" || cfg.addrSet {
		t.Errorf("ECHO_ADDR not honored: %+v", cfg)
	}
}

func TestHealthcheckFailsWithoutServer(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	code, out := runCmd(t, context.Background(), "healthcheck", "-state-file", state, "-addr", "127.0.0.1:1")
	if code != 1 || !strings.Contains(out, "unhealthy") {
		t.Errorf("healthcheck = %d %q", code, out)
	}
}

func TestServeLifecycle(t *testing.T) {
	t.Run("http", func(t *testing.T) { testLifecycle(t, false) })
	t.Run("https", func(t *testing.T) { testLifecycle(t, true) })
}

// testLifecycle starts the real server with flags only, checks that the
// healthcheck command finds it through the state file, holds a stream open,
// and verifies shutdown still completes promptly.
func testLifecycle(t *testing.T, useTLS bool) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	args := []string{"-addr", "127.0.0.1:0", "-route-prefix", "/_echo", "-state-file", state, "-log-format", "json"}
	if useTLS {
		cert, key := writeSelfSigned(t, dir)
		args = append(args, "-tls-cert", cert, "-tls-key", key)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	var logs safeBuffer
	go func() { done <- run(ctx, args, &logs, &logs) }()

	healthURL := waitForFile(t, state)
	wantScheme := map[bool]string{false: "http://", true: "https://"}[useTLS]
	if !strings.HasPrefix(healthURL, wantScheme+"127.0.0.1:") || !strings.HasSuffix(healthURL, "/_echo/healthz") {
		t.Fatalf("state file = %q", healthURL)
	}

	// No -addr: the healthcheck must discover the server from the state file.
	if code, out := runCmd(t, context.Background(), "healthcheck", "-state-file", state); code != 0 {
		t.Fatalf("healthcheck = %d %s", code, out)
	}
	if code, out := runCmd(t, context.Background(), "healthcheck", "-url", healthURL); code != 0 {
		t.Fatalf("healthcheck -url = %d %s", code, out)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test server
	base := strings.TrimSuffix(healthURL, "/_echo/healthz")
	sse, err := client.Get(base + "/_echo/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sse.Body.Close() }()

	start := time.Now()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exited %d\n%s", code, logs.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung with an open SSE stream")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("shutdown took %s", el)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("state file not removed: %v", err)
	}
	for _, want := range []string{`"msg":"echo-server listening"`, `"msg":"stopped"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %s:\n%s", want, logs.String())
		}
	}
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never wrote %s", path)
	return ""
}

func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// safeBuffer is a bytes.Buffer the server can log to while the test reads it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
