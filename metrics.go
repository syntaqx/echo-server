package echo

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// metrics is a tiny Prometheus-compatible registry, kept dependency-free.
type metrics struct {
	inflight   atomic.Int64
	wsActive   atomic.Int64
	wsTotal    atomic.Int64
	wsMessages atomic.Int64

	mu       sync.Mutex
	requests map[[2]string]uint64 // {method, code} -> count
	durSum   float64
	durCount uint64
	buckets  []float64
	bucketN  []uint64
}

func newMetrics() *metrics {
	b := []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
	return &metrics{requests: map[[2]string]uint64{}, buckets: b, bucketN: make([]uint64, len(b))}
}

var knownMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "OPTIONS": true, "CONNECT": true, "TRACE": true,
}

func (m *metrics) observe(method string, code int, d time.Duration) {
	if !knownMethods[method] {
		method = "OTHER" // bound label cardinality
	}
	secs := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{method, strconv.Itoa(code)}]++
	if code == http.StatusSwitchingProtocols {
		return // a WebSocket's lifetime is not request latency
	}
	m.durSum += secs
	m.durCount++
	for i, le := range m.buckets {
		if secs <= le {
			m.bucketN[i]++
		}
	}
}

func (h *Handler) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	m := h.metrics
	var sb strings.Builder

	sb.WriteString("# HELP echo_build_info Build information.\n# TYPE echo_build_info gauge\n")
	fmt.Fprintf(&sb, "echo_build_info{version=%q,goversion=%q} 1\n", h.opts.Version, runtime.Version())

	sb.WriteString("# HELP echo_uptime_seconds Seconds since the server started.\n# TYPE echo_uptime_seconds gauge\n")
	fmt.Fprintf(&sb, "echo_uptime_seconds %g\n", time.Since(h.started).Seconds())

	sb.WriteString("# HELP echo_http_requests_in_flight Requests currently being served.\n# TYPE echo_http_requests_in_flight gauge\n")
	// Subtract this scrape itself.
	fmt.Fprintf(&sb, "echo_http_requests_in_flight %d\n", m.inflight.Load()-1)

	sb.WriteString("# HELP echo_websocket_connections WebSocket connections currently open.\n# TYPE echo_websocket_connections gauge\n")
	fmt.Fprintf(&sb, "echo_websocket_connections %d\n", m.wsActive.Load())

	sb.WriteString("# HELP echo_websocket_connections_total WebSocket connections accepted.\n# TYPE echo_websocket_connections_total counter\n")
	fmt.Fprintf(&sb, "echo_websocket_connections_total %d\n", m.wsTotal.Load())

	sb.WriteString("# HELP echo_websocket_messages_total WebSocket messages echoed.\n# TYPE echo_websocket_messages_total counter\n")
	fmt.Fprintf(&sb, "echo_websocket_messages_total %d\n", m.wsMessages.Load())

	m.mu.Lock()
	sb.WriteString("# HELP echo_http_requests_total Requests served, by method and status code.\n# TYPE echo_http_requests_total counter\n")
	for _, k := range sortedKeys2(m.requests) {
		fmt.Fprintf(&sb, "echo_http_requests_total{method=%q,code=%q} %d\n", k[0], k[1], m.requests[k])
	}
	sb.WriteString("# HELP echo_http_request_duration_seconds Request latency.\n# TYPE echo_http_request_duration_seconds histogram\n")
	for i, le := range m.buckets {
		fmt.Fprintf(&sb, "echo_http_request_duration_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(le, 'g', -1, 64), m.bucketN[i])
	}
	fmt.Fprintf(&sb, "echo_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", m.durCount)
	fmt.Fprintf(&sb, "echo_http_request_duration_seconds_sum %g\n", m.durSum)
	fmt.Fprintf(&sb, "echo_http_request_duration_seconds_count %d\n", m.durCount)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(sb.String()))
}

func sortedKeys2(m map[[2]string]uint64) [][2]string {
	return slices.SortedFunc(maps.Keys(m), func(a, b [2]string) int {
		return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1]))
	})
}
