package echo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	if opts.Hostname == "" {
		opts.Hostname = "test-host"
	}
	srv := httptest.NewServer(New(opts))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func decode(t *testing.T, body []byte) Echo {
	t.Helper()
	var e Echo
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decoding echo: %v\n%s", err, body)
	}
	return e
}

func TestEchoJSON(t *testing.T) {
	srv := newServer(t, Options{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/some/path?a=1&a=2&b=x", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom", "yes")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.Header.Set("X-Request-Id", "abc123")
	req.AddCookie(&http.Cookie{Name: "session", Value: "s3cr3t"})

	resp, body := do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	if got := resp.Header.Get("X-Echo-Server"); got != "test-host" {
		t.Errorf("X-Echo-Server = %q", got)
	}
	if got := resp.Header.Get("X-Request-Id"); got != "abc123" {
		t.Errorf("X-Request-Id not propagated: %q", got)
	}

	e := decode(t, body)
	if e.Method != "POST" || e.Path != "/some/path" {
		t.Errorf("method/path = %s %s", e.Method, e.Path)
	}
	if got := e.Query["a"]; len(got) != 2 || got[1] != "2" {
		t.Errorf("query a = %v", got)
	}
	if e.Headers["X-Custom"][0] != "yes" {
		t.Errorf("headers = %v", e.Headers)
	}
	if e.ClientIP != "203.0.113.7" {
		t.Errorf("client_ip = %q", e.ClientIP)
	}
	if e.Cookies["session"] != "s3cr3t" {
		t.Errorf("cookies = %v", e.Cookies)
	}
	if e.Body == nil || e.Body.Data != `{"hello":"world"}` {
		t.Fatalf("body = %+v", e.Body)
	}
	var parsed map[string]string
	if err := json.Unmarshal(e.Body.JSON, &parsed); err != nil || parsed["hello"] != "world" {
		t.Errorf("body.json = %s (%v)", e.Body.JSON, err)
	}
	if e.Server.Hostname != "test-host" || e.RequestID != "abc123" {
		t.Errorf("server/request id = %+v %q", e.Server, e.RequestID)
	}
}

func TestEchoBodies(t *testing.T) {
	srv := newServer(t, Options{MaxBodyBytes: 16})

	t.Run("form", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("x=1&y=2"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_, body := do(t, req)
		if e := decode(t, body); e.Body.Form["y"][0] != "2" {
			t.Errorf("form = %v", e.Body.Form)
		}
	})

	t.Run("binary", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte{0xff, 0xfe, 0x00}))
		_, body := do(t, req)
		if e := decode(t, body); e.Body.Encoding != "base64" || e.Body.Data != "//4A" {
			t.Errorf("body = %+v", e.Body)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(strings.Repeat("a", 100)))
		_, body := do(t, req)
		if e := decode(t, body); !e.Body.Truncated || e.Body.Size != 16 {
			t.Errorf("body = %+v", e.Body)
		}
	})
}

func TestEchoMultipart(t *testing.T) {
	srv := newServer(t, Options{})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "gopher")
	fw, _ := mw.CreateFormFile("upload", "hello.txt")
	_, _ = fw.Write([]byte("hello, file"))
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	_, body := do(t, req)
	e := decode(t, body)
	if e.Body.Form["name"][0] != "gopher" {
		t.Errorf("form = %v", e.Body.Form)
	}
	if len(e.Body.Files) != 1 || e.Body.Files[0].Filename != "hello.txt" || e.Body.Files[0].Size != 11 {
		t.Errorf("files = %+v", e.Body.Files)
	}
}

func TestControls(t *testing.T) {
	srv := newServer(t, Options{MaxDelay: time.Second})

	tests := []struct {
		name       string
		path       string
		header     map[string]string
		wantStatus int
		wantHeader [2]string
	}{
		{name: "status header", path: "/", header: map[string]string{"X-Echo-Status": "418"}, wantStatus: 418},
		{name: "status query", path: "/?echo_status=503", wantStatus: 503},
		{name: "status route", path: "/status/201", wantStatus: 201},
		{name: "status route invalid", path: "/status/999", wantStatus: 400},
		{name: "header wins over route", path: "/status/201", header: map[string]string{"X-Echo-Status": "202"}, wantStatus: 202},
		{name: "response header", path: "/?echo_header=Cache-Control:no-store", wantStatus: 200, wantHeader: [2]string{"Cache-Control", "no-store"}},
		{name: "response header via header", path: "/", header: map[string]string{"X-Echo-Header": "X-Foo: bar"}, wantStatus: 200, wantHeader: [2]string{"X-Foo", "bar"}},
		{name: "bad header", path: "/?echo_header=nocolon", wantStatus: 400},
		{name: "delay over max", path: "/delay/5s", wantStatus: 400},
		{name: "bad format", path: "/?echo_format=xml", wantStatus: 400},
		{name: "no content", path: "/status/204", wantStatus: 204},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+tt.path, nil)
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			resp, _ := do(t, req)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantHeader[0] != "" && resp.Header.Get(tt.wantHeader[0]) != tt.wantHeader[1] {
				t.Errorf("%s = %q, want %q", tt.wantHeader[0], resp.Header.Get(tt.wantHeader[0]), tt.wantHeader[1])
			}
		})
	}
}

func TestDelay(t *testing.T) {
	srv := newServer(t, Options{})
	for _, path := range []string{"/delay/150ms", "/?echo_delay=150"} {
		start := time.Now()
		resp, _ := do(t, mustReq(t, http.MethodGet, srv.URL+path, nil))
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
		if el := time.Since(start); el < 150*time.Millisecond {
			t.Errorf("%s: returned after %s, want >= 150ms", path, el)
		}
	}
}

func TestFormats(t *testing.T) {
	srv := newServer(t, Options{})

	t.Run("browser gets html", func(t *testing.T) {
		req := mustReq(t, http.MethodGet, srv.URL+"/hello?x=<script>", nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		resp, body := do(t, req)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("content-type = %q", resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(string(body), "/hello") {
			t.Error("html missing path")
		}
		if strings.Contains(string(body), "x=<script>") {
			t.Error("html is not escaped")
		}
	})

	t.Run("text", func(t *testing.T) {
		req := mustReq(t, http.MethodPut, srv.URL+"/wire?echo_format=text", strings.NewReader("payload"))
		_, body := do(t, req)
		got := string(body)
		if !strings.HasPrefix(got, "PUT /wire?echo_format=text HTTP/1.1\r\n") || !strings.HasSuffix(got, "\r\n\r\npayload") {
			t.Errorf("text = %q", got)
		}
	})

	t.Run("raw", func(t *testing.T) {
		req := mustReq(t, http.MethodPost, srv.URL+"/?echo_format=raw", strings.NewReader("<xml/>"))
		req.Header.Set("Content-Type", "application/xml")
		resp, body := do(t, req)
		if string(body) != "<xml/>" || resp.Header.Get("Content-Type") != "application/xml" {
			t.Errorf("raw = %q (%s)", body, resp.Header.Get("Content-Type"))
		}
	})

	t.Run("head has no body", func(t *testing.T) {
		resp, body := do(t, mustReq(t, http.MethodHead, srv.URL, nil))
		if resp.StatusCode != 200 || len(body) != 0 {
			t.Errorf("HEAD = %d, %d bytes", resp.StatusCode, len(body))
		}
	})
}

func TestCORSPreflight(t *testing.T) {
	srv := newServer(t, Options{})
	req := mustReq(t, http.MethodOptions, srv.URL+"/api", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	req.Header.Set("Access-Control-Request-Headers", "x-token")
	resp, _ := do(t, req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Access-Control-Allow-Origin":      "*",
		"Access-Control-Allow-Methods":     "DELETE",
		"Access-Control-Allow-Headers":     "x-token",
		"Access-Control-Allow-Credentials": "",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestProbes(t *testing.T) {
	h := New(Options{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, p := range []string{"/healthz", "/livez", "/readyz"} {
		if resp, _ := do(t, mustReq(t, http.MethodGet, srv.URL+p, nil)); resp.StatusCode != 200 {
			t.Errorf("%s = %d", p, resp.StatusCode)
		}
	}
	h.SetReady(false)
	if resp, _ := do(t, mustReq(t, http.MethodGet, srv.URL+"/readyz", nil)); resp.StatusCode != 503 {
		t.Errorf("draining /readyz = %d", resp.StatusCode)
	}
	if resp, _ := do(t, mustReq(t, http.MethodGet, srv.URL+"/healthz", nil)); resp.StatusCode != 200 {
		t.Errorf("draining /healthz = %d", resp.StatusCode)
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("ECHO_TEST_VAR", "visible")
	// Disabled: /env falls through to the echo, which must not leak env vars.
	_, body := do(t, mustReq(t, http.MethodGet, newServer(t, Options{}).URL+"/env", nil))
	if strings.Contains(string(body), "visible") {
		t.Error("env leaked while disabled")
	}
	_, body = do(t, mustReq(t, http.MethodGet, newServer(t, Options{EnableEnv: true}).URL+"/env", nil))
	if !strings.Contains(string(body), `"ECHO_TEST_VAR": "visible"`) {
		t.Errorf("env missing var: %s", body)
	}
}

func TestMetrics(t *testing.T) {
	srv := newServer(t, Options{Version: "v1.2.3"})
	do(t, mustReq(t, http.MethodGet, srv.URL+"/status/500", nil))
	do(t, mustReq(t, http.MethodGet, srv.URL+"/status/500", nil))
	_, body := do(t, mustReq(t, http.MethodGet, srv.URL+"/metrics", nil))
	for _, want := range []string{
		`echo_http_requests_total{method="GET",code="500"} 2`,
		`echo_build_info{version="v1.2.3"`,
		`echo_http_request_duration_seconds_count 2`,
		`echo_http_requests_in_flight 0`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestWebSocket(t *testing.T) {
	srv := newServer(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/any/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()

	for _, msg := range []struct {
		typ  websocket.MessageType
		data []byte
	}{
		{websocket.MessageText, []byte("hello")},
		{websocket.MessageBinary, []byte{0, 1, 2}},
	} {
		if err := c.Write(ctx, msg.typ, msg.data); err != nil {
			t.Fatal(err)
		}
		typ, got, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != msg.typ || !bytes.Equal(got, msg.data) {
			t.Errorf("echo = %v %q, want %v %q", typ, got, msg.typ, msg.data)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func TestSSE(t *testing.T) {
	srv := newServer(t, Options{})
	resp, err := http.Get(srv.URL + "/sse?count=3&interval=10ms")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	var events int
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			events++
		}
	}
	if events != 3 {
		t.Errorf("events = %d, want 3", events)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		header, value, want string
	}{
		{"Forwarded", `for="[2001:db8::1]:4711";proto=https, for=10.0.0.1`, "2001:db8::1"},
		{"Forwarded", "for=192.0.2.60;proto=http", "192.0.2.60"},
		{"X-Real-Ip", "198.51.100.2", "198.51.100.2"},
		{"", "", "192.0.2.1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.header != "" {
			r.Header.Set(tt.header, tt.value)
		}
		if got := clientIP(r); got != tt.want {
			t.Errorf("clientIP(%s: %s) = %q, want %q", tt.header, tt.value, got, tt.want)
		}
	}
}

func TestCurlCommand(t *testing.T) {
	got := curlCommand(&Echo{
		Method:  "POST",
		URL:     "http://localhost/x",
		Headers: map[string][]string{"Content-Type": {"application/json"}, "Content-Length": {"9"}},
		Body:    &Body{Encoding: "utf-8", Data: `{"a":"'"}`},
	})
	want := "curl -X POST \\\n  -H 'Content-Type: application/json' \\\n  --data-raw '{\"a\":\"'\\''\"}' \\\n  'http://localhost/x'"
	if got != want {
		t.Errorf("curl =\n%s\nwant\n%s", got, want)
	}
}

func mustReq(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func newHandlerServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWebSocketMetrics(t *testing.T) {
	srv := newServer(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte("hi"))
	_, _, _ = c.Read(ctx)

	_, body := do(t, mustReq(t, http.MethodGet, srv.URL+"/metrics", nil))
	for _, want := range []string{"echo_websocket_connections 1", "echo_websocket_connections_total 1", "echo_websocket_messages_total 1"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	// Once closed, the upgrade is counted but kept out of the latency histogram.
	scrapes := 1
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, body = do(t, mustReq(t, http.MethodGet, srv.URL+"/metrics", nil))
		scrapes++
		if strings.Contains(string(body), `code="101"} 1`) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(body), `echo_http_requests_total{method="GET",code="101"} 1`) {
		t.Errorf("upgrade not counted:\n%s", body)
	}
	// Each scrape is observed after it responds, so the latest isn't counted yet.
	if want := fmt.Sprintf("echo_http_request_duration_seconds_count %d\n", scrapes-1); !strings.Contains(string(body), want) {
		t.Errorf("histogram should hold only the earlier scrapes (%q):\n%s", want, body)
	}
}
