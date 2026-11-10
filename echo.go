// Package echo implements an HTTP server that reflects every request back to
// the caller: method, path, headers, body, TLS details and more. It is meant
// for debugging proxies, load balancers, ingress controllers, service meshes
// and HTTP clients.
//
// The handler returned by New can be mounted in any server, or used in-process
// with net/http/httptest:
//
//	srv := httptest.NewServer(echo.New(echo.Options{}))
//	defer srv.Close()
package echo

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures the echo handler. The zero value is usable.
type Options struct {
	// MaxBodyBytes caps how much of a request body is read and echoed back.
	// Bodies larger than this are truncated in the echo. Defaults to 1 MiB.
	MaxBodyBytes int64

	// MaxDelay caps the artificial delay a client may request. Defaults to 30s.
	MaxDelay time.Duration

	// RoutePrefix moves the built-in routes (/healthz, /metrics, /status/...,
	// etc.) under a prefix such as "/_echo", so every other path, including
	// /healthz, is echoed. Empty means the routes live at the root.
	RoutePrefix string

	// EnableEnv exposes the server's environment variables at /env. Off by
	// default because environments often contain secrets.
	EnableEnv bool

	// Hostname is reported in responses so callers can tell which instance
	// answered. Defaults to os.Hostname().
	Hostname string

	// Version is reported in responses and metrics.
	Version string

	// Logger receives access logs. Nil disables access logging.
	Logger *slog.Logger
}

// Handler is the echo server. Create one with New.
type Handler struct {
	opts    Options
	mux     *http.ServeMux
	ready   atomic.Bool
	started time.Time
	metrics *metrics

	done      chan struct{}
	closeOnce sync.Once
}

// New returns a ready echo handler. It panics if opts.RoutePrefix is invalid;
// use ValidatePrefix to check user input first.
func New(opts Options) *Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = 30 * time.Second
	}
	if opts.Hostname == "" {
		opts.Hostname, _ = os.Hostname()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if err := ValidatePrefix(opts.RoutePrefix); err != nil {
		panic(err)
	}
	opts.RoutePrefix = strings.TrimSuffix(opts.RoutePrefix, "/")

	h := &Handler{
		opts:    opts,
		mux:     http.NewServeMux(),
		started: time.Now(),
		metrics: newMetrics(),
		done:    make(chan struct{}),
	}
	h.ready.Store(true)

	p := opts.RoutePrefix
	h.mux.HandleFunc("GET "+p+"/healthz", h.handleHealth)
	h.mux.HandleFunc("GET "+p+"/livez", h.handleHealth)
	h.mux.HandleFunc("GET "+p+"/readyz", h.handleReady)
	h.mux.HandleFunc("GET "+p+"/metrics", h.handleMetrics)
	h.mux.HandleFunc("GET "+p+"/sse", h.handleSSE)
	h.mux.HandleFunc(p+"/status/{code}", h.handleStatus)
	h.mux.HandleFunc(p+"/delay/{duration}", h.handleDelay)
	if opts.EnableEnv {
		h.mux.HandleFunc("GET "+p+"/env", h.handleEnv)
	}
	h.mux.HandleFunc("/", h.handleEcho)

	return h
}

// ValidatePrefix reports whether p can be used as Options.RoutePrefix.
func ValidatePrefix(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "{} \t?#") || strings.Contains(p, "//") {
		return fmt.Errorf("invalid route prefix %q: want a path like /_echo", p)
	}
	return nil
}

// HealthPath is the path of the liveness probe, including any route prefix.
func (h *Handler) HealthPath() string { return h.opts.RoutePrefix + "/healthz" }

// SetReady toggles the /readyz endpoint. Call SetReady(false) when draining so
// load balancers stop routing new traffic before shutdown.
func (h *Handler) SetReady(ready bool) { h.ready.Store(ready) }

// CloseStreams ends long-lived responses: WebSocket connections are closed
// with 1001 (going away), SSE streams end, and pending delays finish early.
// http.Server.Shutdown does not interrupt active handlers, so call this when
// shutting down, e.g. with srv.RegisterOnShutdown(h.CloseStreams).
// It is safe to call more than once.
func (h *Handler) CloseStreams() {
	h.closeOnce.Do(func() { close(h.done) })
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	h.metrics.inflight.Add(1)
	defer h.metrics.inflight.Add(-1)

	if h.opts.Hostname != "" {
		w.Header().Set("X-Echo-Server", h.opts.Hostname)
	}
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = newID()
	}
	w.Header().Set("X-Request-Id", reqID)

	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	h.mux.ServeHTTP(rec, r)

	elapsed := time.Since(start)
	h.metrics.observe(r.Method, rec.status, elapsed)

	if h.opts.Logger != nil && !h.isProbe(r.URL.Path) {
		h.opts.Logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("id", reqID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("proto", r.Proto),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", elapsed),
			slog.String("client_ip", clientIP(r)),
			slog.String("user_agent", r.UserAgent()),
		)
	}
}

func (h *Handler) isProbe(path string) bool {
	rest, ok := strings.CutPrefix(path, h.opts.RoutePrefix)
	if !ok {
		return false
	}
	switch rest {
	case "/healthz", "/livez", "/readyz", "/metrics":
		return true
	}
	return false
}

// recorder captures the status code and size of a response while still
// exposing the underlying writer to http.ResponseController (for flushing,
// hijacking and deadlines).
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
