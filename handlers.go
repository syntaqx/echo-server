package echo

import (
	"context"
	"encoding/json"
	"fmt"
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

func (h *Handler) parseControls(r *http.Request) (controls, error) {
	q := r.URL.Query()
	get := func(name string) string {
		if v := r.Header.Get("X-Echo-" + name); v != "" {
			return v
		}
		return q.Get("echo_" + strings.ToLower(name))
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
		d, err := h.parseDelay(s)
		if err != nil {
			return c, err
		}
		c.delay = d
	}
	for _, v := range append(r.Header.Values("X-Echo-Header"), q["echo_header"]...) {
		name, value, ok := strings.Cut(v, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return c, fmt.Errorf("invalid echo header %q: want \"Name: value\"", v)
		}
		c.headers = append(c.headers, [2]string{http.CanonicalHeaderKey(name), strings.TrimSpace(value)})
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

// parseDelay accepts Go durations ("250ms", "1.5s") or bare milliseconds ("250").
func (h *Handler) parseDelay(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	d, err := time.ParseDuration(s)
	if err != nil {
		ms, msErr := strconv.ParseFloat(s, 64)
		if msErr != nil {
			return 0, fmt.Errorf("invalid delay %q: want a duration like 250ms or 2s", s)
		}
		d = time.Duration(ms * float64(time.Millisecond))
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid delay %q: must not be negative", s)
	}
	if d > h.opts.MaxDelay {
		return 0, fmt.Errorf("delay %s exceeds the maximum of %s", d, h.opts.MaxDelay)
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
	code, err := parseStatus(r.PathValue("code"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	h.respond(w, r, controls{status: code})
}

func (h *Handler) handleDelay(w http.ResponseWriter, r *http.Request) {
	d, err := h.parseDelay(r.PathValue("duration"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	h.respond(w, r, controls{delay: d})
}

// respond applies controls from the request on top of base, then echoes.
func (h *Handler) respond(w http.ResponseWriter, r *http.Request, base controls) {
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

	setCORS(w, r)
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	e, raw, err := h.capture(w, r)
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("reading body: %w", err))
		return
	}

	if c.delay > 0 {
		// Keep the server's write timeout from cutting the delay short.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(c.delay + 30*time.Second))
		if err := h.sleep(r.Context(), c.delay); err != nil {
			return // client went away
		}
	}

	for _, kv := range c.headers {
		w.Header().Add(kv[0], kv[1])
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}

	format := c.format
	if format == "" {
		format = negotiate(r)
	}

	// The echoed content is attacker-controllable (query values, bodies,
	// even Content-Type via echo_header), so never let a browser execute it
	// with this origin's privileges. The HTML view sets its own stricter CSP.
	if format != "html" {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Add("Content-Security-Policy", "sandbox")
	}

	// Some responses must not carry a body.
	if r.Method == http.MethodHead || status == http.StatusNoContent || status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}

	switch format {
	case "raw":
		if ct := r.Header.Get("Content-Type"); ct != "" && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(status)
		_, _ = w.Write(raw)
	case "text":
		setDefault(w, "Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(rawRequest(r, raw, e.Body != nil && e.Body.Truncated)))
	case "html":
		h.renderHTML(w, status, e)
	default:
		writeJSON(w, status, e)
	}
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	setDefault(w, "Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func httpError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func setDefault(w http.ResponseWriter, key, value string) {
	if w.Header().Get(key) == "" {
		w.Header().Set(key, value)
	}
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
