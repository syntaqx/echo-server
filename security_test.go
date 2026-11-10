package echo

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Responses echo cookies, so a foreign origin must never get credentialed
// access to them.
func TestCORSNeverAllowsCredentials(t *testing.T) {
	srv := newServer(t, Options{})
	req := mustReq(t, http.MethodGet, srv.URL, nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Cookie", "session=secret")
	resp, _ := do(t, req)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials = %q, want unset", got)
	}
}

// Echoed content is attacker-controlled; even when a client forces an HTML
// content type, the browser must not run it with this origin.
func TestEchoedContentIsSandboxed(t *testing.T) {
	srv := newServer(t, Options{})
	for _, path := range []string{
		"/?x=<script>alert(1)</script>&echo_header=Content-Type:text/html",
		"/?echo_format=raw&echo_header=Content-Type:text/html",
		"/?echo_format=text",
		"/?echo_format=<script>", // error response
	} {
		resp, _ := do(t, mustReq(t, http.MethodGet, srv.URL+path, nil))
		if csp := resp.Header.Values("Content-Security-Policy"); len(csp) == 0 || csp[0] != "sandbox" {
			t.Errorf("%s: CSP = %q, want sandbox", path, csp)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
	}
}

func TestHTMLHasNonceCSP(t *testing.T) {
	srv := newServer(t, Options{})
	req := mustReq(t, http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept", "text/html")
	resp, body := do(t, req)
	csp := resp.Header.Get("Content-Security-Policy")
	m := regexp.MustCompile(`script-src 'nonce-([0-9a-f]+)'`).FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("CSP = %q", csp)
	}
	if !strings.Contains(string(body), `<script nonce="`+m[1]+`">`) || !strings.Contains(string(body), `<style nonce="`+m[1]+`">`) {
		t.Error("inline script/style not tagged with the CSP nonce")
	}
}

func TestRoutePrefix(t *testing.T) {
	srv := newServer(t, Options{RoutePrefix: "/_echo/"})

	// Built-in routes move under the prefix...
	for path, want := range map[string]int{
		"/_echo/healthz":    200,
		"/_echo/status/418": 418,
		"/_echo/metrics":    200,
	} {
		if resp, _ := do(t, mustReq(t, http.MethodGet, srv.URL+path, nil)); resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	// ...and their old paths are echoed like any other.
	for _, path := range []string{"/healthz", "/status/418", "/metrics"} {
		resp, body := do(t, mustReq(t, http.MethodGet, srv.URL+path, nil))
		if resp.StatusCode != 200 || decode(t, body).Path != path {
			t.Errorf("%s was not echoed: %d", path, resp.StatusCode)
		}
	}
}

func TestValidatePrefix(t *testing.T) {
	for p, ok := range map[string]bool{
		"": true, "/_echo": true, "/a/b/": true,
		"_echo": false, "/{x}": false, "/a b": false, "//x": false,
	} {
		if err := ValidatePrefix(p); (err == nil) != ok {
			t.Errorf("ValidatePrefix(%q) = %v", p, err)
		}
	}
}

func TestCloseStreams(t *testing.T) {
	h := New(Options{})
	srv := newHandlerServer(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()

	sse, err := http.Get(srv.URL + "/sse?interval=10s")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sse.Body.Close() }()

	delayed := make(chan int, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/delay/20s")
		if err != nil {
			delayed <- 0
			return
		}
		_ = resp.Body.Close()
		delayed <- resp.StatusCode
	}()

	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	h.CloseStreams()
	h.CloseStreams() // idempotent

	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Errorf("websocket read err = %v, want going away", err)
	}
	body := readAll(t, sse.Body)
	if !strings.Contains(body, "event: close") {
		t.Errorf("sse did not announce close: %q", body)
	}
	if code := <-delayed; code != 200 {
		t.Errorf("delayed request = %d, want 200", code)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("streams took %s to close", el)
	}
}
