package echo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// controls are per-request response overrides. Each one can be set with an
// X-Echo-* request header or an echo_* query parameter, on any path.
type controls struct {
	status  int
	delay   time.Duration
	headers [][2]string
	format  string
}

// queryForbiddenHeaders can be set with the X-Echo-Header request header but
// not with the echo_header query parameter. A query parameter rides along on
// a plain link, so allowing these would turn the server into an open
// redirect and a way to plant cookies on whatever domain hosts it. The
// header form needs a script (and a CORS preflight) to send cross-site.
var queryForbiddenHeaders = map[string]bool{
	"Location":        true,
	"Refresh":         true,
	"Set-Cookie":      true,
	"Clear-Site-Data": true,
}

func (h *Handler) parseControls(r *http.Request) (controls, error) {
	q := parseQuery(r.URL.RawQuery)
	get := func(name string) string {
		if v := r.Header.Get("X-Echo-" + name); v != "" {
			return v
		}
		if vs := q["echo_"+strings.ToLower(name)]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}

	var c controls
	if s := get("Status"); s != "" {
		code, err := parseStatus(s)
		if err != nil {
			return c, err
		}
		c.status = code
	}
	if s := get("Delay"); s != "" {
		d, err := parseDuration("delay", s, 0, h.opts.MaxDelay)
		if err != nil {
			return c, err
		}
		c.delay = d
	}
	addHeader := func(v string, fromQuery bool) error {
		name, value, ok := strings.Cut(v, ":")
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !ok || name == "" {
			return fmt.Errorf("invalid echo header %q: want \"Name: value\"", v)
		}
		if fromQuery && queryForbiddenHeaders[name] {
			return fmt.Errorf("echo header %s can't be set with the echo_header query parameter; use the X-Echo-Header request header", name)
		}
		c.headers = append(c.headers, [2]string{name, strings.TrimSpace(value)})
		return nil
	}
	for _, v := range r.Header.Values("X-Echo-Header") {
		if err := addHeader(v, false); err != nil {
			return c, err
		}
	}
	for _, v := range q["echo_header"] {
		if err := addHeader(v, true); err != nil {
			return c, err
		}
	}
	c.format = strings.ToLower(get("Format"))
	switch c.format {
	case "", "json", "text", "html", "raw":
	default:
		return c, fmt.Errorf("invalid echo format %q: want json, text, html or raw", c.format)
	}
	return c, nil
}

func parseStatus(s string) (int, error) {
	code, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || code < 200 || code > 599 {
		return 0, fmt.Errorf("invalid status %q: want 200-599", s)
	}
	return code, nil
}

// parseDuration accepts Go durations ("250ms", "1.5s") or bare milliseconds
// ("250"), and requires lo <= d <= hi. name is used in error messages.
func parseDuration(name, s string, lo, hi time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	d, err := time.ParseDuration(s)
	if err != nil {
		ms, msErr := strconv.ParseFloat(s, 64)
		if msErr != nil || math.IsNaN(ms) || math.IsInf(ms, 0) {
			return 0, fmt.Errorf("invalid %s %q: want a duration like 250ms or 2s", name, s)
		}
		// Compare in float before converting, so huge values can't overflow.
		if ms*float64(time.Millisecond) > float64(hi) {
			return 0, fmt.Errorf("%s %q exceeds the maximum of %s", name, s, hi)
		}
		d = time.Duration(ms * float64(time.Millisecond))
	}
	switch {
	case d < 0:
		return 0, fmt.Errorf("invalid %s %q: must not be negative", name, s)
	case d < lo:
		return 0, fmt.Errorf("%s %s is below the minimum of %s", name, d, lo)
	case d > hi:
		return 0, fmt.Errorf("%s %s exceeds the maximum of %s", name, d, hi)
	}
	return d, nil
}

func (h *Handler) handleEcho(w http.ResponseWriter, r *http.Request) {
	if isWebSocket(r) {
		h.handleWebSocket(w, r)
		return
	}
	h.respond(w, r, controls{})
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	code, err := parseStatus(r.PathValue("code"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	h.respond(w, r, controls{status: code})
}

func (h *Handler) handleDelay(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	d, err := parseDuration("delay", r.PathValue("duration"), 0, h.opts.MaxDelay)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	h.respond(w, r, controls{delay: d})
}

// respond applies controls from the request on top of base, then echoes.
func (h *Handler) respond(w http.ResponseWriter, r *http.Request, base controls) {
	setCORS(w, r)
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	c, err := h.parseControls(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if c.status == 0 {
		c.status = base.status
	}
	if c.delay == 0 {
		c.delay = base.delay
	}

	e, raw, err := h.capture(w, r)
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("reading body: %w", err))
		return
	}

	if c.delay > 0 {
		if err := h.sleep(r.Context(), c.delay); err != nil {
			return // client went away; recorded as 499
		}
	}

	format := c.format
	if format == "" {
		format = negotiate(r)
	}

	// Render first so HEAD and bodyless statuses get the same headers a GET would.
	var body []byte
	switch format {
	case "raw":
		if ct := r.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		body = raw
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		body = []byte(rawRequest(r, raw, e.Body != nil && e.Body.Truncated))
	case "html":
		body = h.renderHTML(w.Header(), e)
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		body = marshalJSON(e)
	}

	// The echoed content is attacker-controllable (query values, bodies,
	// even Content-Type via echo_header), so never let a browser execute it
	// with this origin's privileges. The HTML view sets its own stricter CSP.
	if format != "html" {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	// Requested headers go last so they can override the defaults above;
	// a requested Content-Security-Policy adds to the sandbox, never replaces it.
	for _, kv := range c.headers {
		switch kv[0] {
		case "Content-Type":
			w.Header().Set(kv[0], kv[1])
		default:
			w.Header().Add(kv[0], kv[1])
		}
	}

	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead && bodyAllowed(status) {
		_, _ = w.Write(body)
	}
}

func bodyAllowed(status int) bool {
	return status != http.StatusNoContent && status != http.StatusNotModified
}

// negotiate picks a format from the Accept header: browsers get HTML, plain
// text when that's explicitly preferred, everything else gets JSON.
func negotiate(r *http.Request) string {
	accept := r.Header.Get("Accept")
	switch {
	case strings.Contains(accept, "text/html"):
		return "html"
	case strings.HasPrefix(accept, "text/plain"):
		return "text"
	default:
		return "json"
	}
}

// setCORS allows any origin to read echo responses, but never with
// credentials: responses contain the caller's cookies, and reflecting Origin
// with Allow-Credentials would let any website read cookies scoped to this
// host (including HttpOnly ones, and parent-domain ones).
func setCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "*")
	if m := r.Header.Get("Access-Control-Request-Method"); m != "" {
		w.Header().Set("Access-Control-Allow-Methods", m)
	}
	if hs := r.Header.Get("Access-Control-Request-Headers"); hs != "" {
		w.Header().Set("Access-Control-Allow-Headers", hs)
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Max-Age", "600")
	}
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

func (h *Handler) handleReady(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !h.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (h *Handler) handleEnv(w http.ResponseWriter, _ *http.Request) {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	writeJSON(w, http.StatusOK, env)
}

func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(marshalJSON(v))
}

// httpError writes a JSON error. Errors can quote request input, so they get
// the same sandboxing as echoed content.
func httpError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// sleep waits for d, returning early with an error if the client goes away.
// If the server starts shutting down, the delay ends early and the response
// is still sent.
func (h *Handler) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
