package echo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression tests for issues found in pre-release review.

func TestUncleanPathsAreEchoedNotRedirected(t *testing.T) {
	h := New(Options{})
	for _, p := range []string{"//foo", "/foo//bar", "/a/./b", "/a/../b", "/_x/../healthz"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d (Location %q), want 200", p, rr.Code, rr.Header().Get("Location"))
			continue
		}
		if got := decode(t, rr.Body.Bytes()).Path; got != p {
			t.Errorf("%s: echoed path %q", p, got)
		}
	}
	// Clean paths still reach the built-in routes.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status/201", nil))
	if rr.Code != http.StatusCreated {
		t.Errorf("/status/201 = %d", rr.Code)
	}
}

func TestValidatePrefixRejectsWhatNewCantRegister(t *testing.T) {
	for _, p := range []string{"/..", "/.", "/a/../b", "/a/./b", "/a\nb", "/a\x00b", "/a%2fb", "/a b"} {
		if err := ValidatePrefix(p); err == nil {
			t.Errorf("ValidatePrefix(%q) = nil, want error", p)
		}
	}
	for _, p := range []string{"", "/", "/_echo", "/_echo/", "/a/b", "/a-b_c.d"} {
		if err := ValidatePrefix(p); err != nil {
			t.Errorf("ValidatePrefix(%q) = %v", p, err)
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("New with accepted prefix %q panicked: %v", p, r)
				}
			}()
			New(Options{RoutePrefix: p, EnableEnv: true})
		}()
	}
}

func TestHeadSSEDoesNotHoldConnection(t *testing.T) {
	srv := newServer(t, Options{})
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Head(srv.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("HEAD /sse content-type = %q", ct)
	}
	// The pooled connection must be immediately reusable.
	resp, err = client.Get(srv.URL + "/after")
	if err != nil {
		t.Fatalf("request after HEAD /sse: %v", err)
	}
	_ = resp.Body.Close()
}

func TestQueryCannotSetDangerousHeaders(t *testing.T) {
	srv := newServer(t, Options{})
	for _, q := range []string{
		"echo_status=302&echo_header=Location:https://evil.example",
		"echo_header=Set-Cookie:sid%3Devil%3B%20Domain%3D.example.com",
		"echo_header=refresh:0;url=https://evil.example",
		"echo_header=Clear-Site-Data:*",
	} {
		resp, body := do(t, mustReq(t, http.MethodGet, srv.URL+"/?"+q, nil))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "X-Echo-Header") {
			t.Errorf("?%s = %d %s, want 400 pointing at X-Echo-Header", q, resp.StatusCode, body)
		}
		if resp.Header.Get("Location") != "" || resp.Header.Get("Set-Cookie") != "" {
			t.Errorf("?%s leaked a header", q)
		}
	}
	// The header form still works for deliberate testing.
	req := mustReq(t, http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("X-Echo-Status", "302")
	req.Header.Set("X-Echo-Header", "Location: /elsewhere")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/elsewhere" {
		t.Errorf("header form = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestErrorsCarryCORS(t *testing.T) {
	srv := newServer(t, Options{})
	for _, path := range []string{"/?echo_status=999", "/status/999", "/delay/forever", "/sse?count=-1"} {
		req := mustReq(t, http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Origin", "https://app.example")
		resp, _ := do(t, req)
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s = %d, ACAO %q", path, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
	}
	// Preflights succeed regardless of controls in the URL.
	req := mustReq(t, http.MethodOptions, srv.URL+"/?echo_delay=abc", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	if resp, _ := do(t, req); resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight = %d, want 204", resp.StatusCode)
	}
}

func TestDurationParsing(t *testing.T) {
	tests := []struct {
		in, wantErr string
		want        time.Duration
	}{
		{in: "250", want: 250 * time.Millisecond},
		{in: "1.5s", want: 1500 * time.Millisecond},
		{in: "0", want: 0},
		{in: "1e300", wantErr: "exceeds the maximum"},
		{in: "9223372036854775807", wantErr: "exceeds the maximum"},
		{in: "NaN", wantErr: "invalid delay"},
		{in: "Inf", wantErr: "invalid delay"},
		{in: "-5", wantErr: "must not be negative"},
		{in: "-1s", wantErr: "must not be negative"},
		{in: "31s", wantErr: "exceeds the maximum of 30s"},
		{in: "soon", wantErr: "invalid delay"},
	}
	for _, tt := range tests {
		d, err := parseDuration("delay", tt.in, 0, 30*time.Second)
		if tt.wantErr == "" {
			if err != nil || d != tt.want {
				t.Errorf("parseDuration(%q) = %v, %v; want %v", tt.in, d, err, tt.want)
			}
		} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("parseDuration(%q) error = %v, want %q", tt.in, err, tt.wantErr)
		}
	}

	srv := newServer(t, Options{MaxDelay: time.Second})
	for path, want := range map[string]string{
		"/sse?interval=2h":  "interval 2h0m0s exceeds the maximum of 1h0m0s",
		"/sse?interval=1ms": "interval 1ms is below the minimum of 10ms",
	} {
		resp, body := do(t, mustReq(t, http.MethodGet, srv.URL+path, nil))
		if resp.StatusCode != 400 || !strings.Contains(string(body), want) {
			t.Errorf("%s = %d %s, want %q", path, resp.StatusCode, body, want)
		}
	}
	// The SSE interval isn't capped by MaxDelay.
	resp, err := http.Get(srv.URL + "/sse?interval=2s&count=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("interval above MaxDelay = %d, want 200", resp.StatusCode)
	}
}

func TestAbortedRequestRecordedAs499(t *testing.T) {
	srv := newServer(t, Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/delay/5s", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the request to be canceled")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, body := do(t, mustReq(t, http.MethodGet, srv.URL+"/metrics", nil))
		if strings.Contains(string(body), `echo_http_requests_total{method="GET",code="499"} 1`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("aborted request not recorded as 499:\n%s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHeadMirrorsGetHeaders(t *testing.T) {
	srv := newServer(t, Options{})
	for _, format := range []string{"json", "text", "html"} {
		get, _ := do(t, mustReq(t, http.MethodGet, srv.URL+"/?echo_format="+format, nil))
		head, body := do(t, mustReq(t, http.MethodHead, srv.URL+"/?echo_format="+format, nil))
		if len(body) != 0 {
			t.Errorf("%s: HEAD returned a body", format)
		}
		for _, k := range []string{"Content-Type", "X-Content-Type-Options"} {
			if get.Header.Get(k) != head.Header.Get(k) {
				t.Errorf("%s: %s GET %q vs HEAD %q", format, k, get.Header.Get(k), head.Header.Get(k))
			}
		}
	}
}

func TestQueryKeepsSemicolons(t *testing.T) {
	srv := newServer(t, Options{})
	_, body := do(t, mustReq(t, http.MethodGet, srv.URL+"/?a=1;b=2&c=3&bad=%zz", nil))
	q := decode(t, body).Query
	if q["a"][0] != "1;b=2" || q["c"][0] != "3" || q["bad"][0] != "%zz" {
		t.Errorf("query = %v", q)
	}
}

func TestChunkedBodyAndTrailers(t *testing.T) {
	srv := newServer(t, Options{MaxBodyBytes: 4})
	pr, pw := io.Pipe()
	req := mustReq(t, http.MethodPost, srv.URL, pr)
	req.Trailer = http.Header{"X-Checksum": nil}
	go func() {
		_, _ = pw.Write([]byte("hello, chunked world"))
		req.Trailer.Set("X-Checksum", "abc123")
		_ = pw.Close()
	}()
	_, body := do(t, req)
	e := decode(t, body)
	if e.ContentLength != -1 || len(e.TransferEncoding) != 1 || e.TransferEncoding[0] != "chunked" {
		t.Errorf("content_length=%d transfer_encoding=%v", e.ContentLength, e.TransferEncoding)
	}
	if e.Body == nil || e.Body.Size != 20 || !e.Body.Truncated || e.Body.Data != "hell" {
		t.Errorf("body = %+v", e.Body)
	}
	if got := e.Trailers["X-Checksum"]; len(got) != 1 || got[0] != "abc123" {
		t.Errorf("trailers = %v", e.Trailers)
	}
}

func TestDuplicateCookiesKeptInOrder(t *testing.T) {
	srv := newServer(t, Options{})
	req := mustReq(t, http.MethodGet, srv.URL, nil)
	req.Header.Set("Cookie", "id=1; theme=dark; id=2")
	_, body := do(t, req)
	got := decode(t, body).Cookies
	want := []Cookie{{"id", "1"}, {"theme", "dark"}, {"id", "2"}}
	if len(got) != len(want) {
		t.Fatalf("cookies = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cookie %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// Every optional section of the HTML view renders with real data.
func TestHTMLViewRendersAllSections(t *testing.T) {
	srv := newServer(t, Options{RoutePrefix: "/_echo"})
	pr, pw := io.Pipe()
	req := mustReq(t, http.MethodPost, srv.URL+"/page?q=1;2", pr)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", "a=1; a=2")
	req.Header.Set("Content-Type", "text/plain")
	req.Trailer = http.Header{"X-Sum": nil}
	go func() {
		_, _ = pw.Write([]byte("<b>body</b>"))
		req.Trailer.Set("X-Sum", "42")
		_ = pw.Close()
	}()
	resp, body := do(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	page := string(body)
	for _, want := range []string{
		"transfer encoding", "chunked", ">Cookies <", ">Trailers <", "X-Sum",
		"1;2", "&lt;b&gt;body&lt;/b&gt;", `href="/_echo/healthz"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("HTML view missing %q", want)
		}
	}
	if strings.Contains(page, "<b>body</b>") {
		t.Error("body was not escaped")
	}
}
