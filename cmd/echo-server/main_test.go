package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
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
	ctx := t.Context()
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
		{name: "bad log format", args: []string{"-log-format", "xml"}, code: 2, out: "invalid log format"},
		{name: "bad log level", args: []string{"-log-level", "loud"}, code: 2, out: "invalid log level"},
		{name: "zero max delay", args: []string{"-max-delay", "0"}, code: 2, out: "-max-delay must be positive"},
		{name: "negative max body", args: []string{"-max-body", "-5"}, code: 2, out: "-max-body must be positive"},
		{name: "zero shutdown timeout", args: []string{"-shutdown-timeout", "0s"}, code: 2, out: "-shutdown-timeout must be positive"},
		{name: "unclean prefix", args: []string{"-route-prefix", "/.."}, code: 2, out: "must be a clean path"},
		{name: "errors are reported together", args: []string{"-max-delay", "0", "-max-body", "0"}, code: 2, out: "-max-body must be positive"},
		{name: "healthcheck bad flag exits 1, not Docker's reserved 2", args: []string{"healthcheck", "-nope"}, code: 1},
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
	if code, out := runCmd(t, t.Context(), "version"); code != 0 || strings.TrimSpace(out) != "v9.9.9" {
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
	code, out := runCmd(t, t.Context(), "healthcheck", "-state-file", state, "-addr", "127.0.0.1:1")
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

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	var logs safeBuffer
	go func() { done <- run(ctx, args, &logs, &logs) }()

	st := waitForState(t, state)
	healthURL := st.URL
	wantScheme := map[bool]string{false: "http://", true: "https://"}[useTLS]
	if !strings.HasPrefix(healthURL, wantScheme+"127.0.0.1:") || !strings.HasSuffix(healthURL, "/_echo/healthz") {
		t.Fatalf("state file = %+v", st)
	}
	if useTLS != (st.TLSCert != "") || (useTLS && !filepath.IsAbs(st.TLSCert)) {
		t.Fatalf("state file cert = %q", st.TLSCert)
	}

	// No -addr: the healthcheck must discover the server from the state file.
	if code, out := runCmd(t, t.Context(), "healthcheck", "-state-file", state); code != 0 {
		t.Fatalf("healthcheck = %d %s", code, out)
	}
	urlArgs := []string{"healthcheck", "-url", healthURL}
	if useTLS {
		urlArgs = append(urlArgs, "-tls-cert", st.TLSCert)
	}
	if code, out := runCmd(t, t.Context(), urlArgs...); code != 0 {
		t.Fatalf("healthcheck -url = %d %s", code, out)
	}

	client := http.DefaultClient
	if useTLS {
		tlsConfig, err := pinnedTLSConfig(st.TLSCert)
		if err != nil {
			t.Fatal(err)
		}
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	}
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

func waitForState(t *testing.T, path string) state {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var st state
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &st) == nil && st.URL != "" {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never wrote %s", path)
	return state{}
}

// The TLS probe pins the configured certificate: a server presenting any
// other certificate is unhealthy, even on loopback.
func TestHealthcheckPinsCertificate(t *testing.T) {
	cert, _ := writeSelfSigned(t, t.TempDir())
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	other.Config.ErrorLog = log.New(io.Discard, "", 0) // the rejected handshake is the point
	other.StartTLS()
	defer other.Close()

	code, out := runCmd(t, t.Context(), "healthcheck", "-url", other.URL+"/healthz", "-tls-cert", cert)
	if code != 1 || !strings.Contains(out, "certificate") {
		t.Errorf("healthcheck against a different cert = %d %q, want a certificate error", code, out)
	}
}

func TestPinnedTLSConfigErrors(t *testing.T) {
	dir := t.TempDir()
	noCert := filepath.Join(dir, "empty.pem")
	_ = os.WriteFile(noCert, []byte("not a pem"), 0o600)
	for path, want := range map[string]string{
		filepath.Join(dir, "missing.pem"): "reading TLS certificate",
		noCert:                            "no certificate found",
	} {
		if _, err := pinnedTLSConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("pinnedTLSConfig(%s) = %v, want %q", filepath.Base(path), err, want)
		}
	}
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

// A second instance can take over the shared default state file; the first
// one exiting must not delete it.
func TestStateFileNotRemovedIfTakenOver(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-state-file", state}, io.Discard, io.Discard)
	}()
	waitForState(t, state)

	takeover := []byte(`{"url":"http://127.0.0.1:1/healthz"}` + "\n")
	if err := os.WriteFile(state, takeover, 0o644); err != nil {
		t.Fatal(err)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exited %d", code)
	}
	if got, err := os.ReadFile(state); err != nil || !bytes.Equal(got, takeover) {
		t.Errorf("state file = %q, %v; want the other instance's", got, err)
	}
}

func TestHealthcheckProbesSpecificAddr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	missing := filepath.Join(t.TempDir(), "none")
	if code, out := runCmd(t, t.Context(), "healthcheck", "-addr", addr, "-state-file", missing); code != 0 {
		t.Errorf("healthcheck -addr %s = %d %s", addr, code, out)
	}
}

func TestFlagOverridesMalformedEnv(t *testing.T) {
	t.Setenv("ECHO_MAX_DELAY", "soon")
	cfg, err := parseConfig("serve", []string{"-max-delay", "2s"}, io.Discard)
	if err != nil || cfg.maxDelay != 2*time.Second {
		t.Errorf("flag should override malformed env: cfg.maxDelay=%s err=%v", cfg.maxDelay, err)
	}
	if _, err := parseConfig("serve", nil, io.Discard); err == nil {
		t.Error("malformed env without an overriding flag should fail")
	}
}
