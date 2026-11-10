package echo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// subprotocols echoes back whatever subprotocols the client offered, so
// clients that require one can still connect.
func subprotocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// handleWebSocket echoes every message back to the sender, preserving its type.
// It works on any path.
func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Connections are long-lived: drop the server's read/write timeouts.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Accept every origin. Cross-site use is safe here: the socket only
		// echoes what the client itself sends, never cookies or headers.
		OriginPatterns: []string{"*"},
		Subprotocols:   subprotocols(r),
	})
	if err != nil {
		return // Accept already wrote the error response
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(h.opts.MaxBodyBytes)

	h.metrics.wsActive.Add(1)
	h.metrics.wsTotal.Add(1)
	defer h.metrics.wsActive.Add(-1)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-h.done:
			_ = c.Close(websocket.StatusGoingAway, "server shutting down")
		case <-ctx.Done():
		}
	}()

	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status != -1 {
				_ = c.Close(status, "")
			}
			return
		}
		h.metrics.wsMessages.Add(1)
		if err := c.Write(ctx, typ, msg); err != nil {
			return
		}
	}
}

// handleSSE streams server-sent events. Query parameters:
//
//	interval  time between events, 10ms to 1h (default 1s)
//	count     number of events before closing (default 0 = forever)
func (h *Handler) handleSSE(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	q := parseQuery(r.URL.RawQuery)
	interval := time.Second
	if vs := q["interval"]; len(vs) > 0 {
		d, err := parseDuration("interval", vs[0], minSSEInterval, maxSSEInterval)
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		interval = d
	}
	count := 0
	if vs := q["count"]; len(vs) > 0 {
		n, err := strconv.Atoi(vs[0])
		if err != nil || n < 0 {
			httpError(w, http.StatusBadRequest, fmt.Errorf("invalid count %q: want 0 (forever) or more", vs[0]))
			return
		}
		count = n
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return // headers only; streaming would hold the connection forever
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // streams outlive the server's write timeout

	ctx := r.Context()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 1; count == 0 || i <= count; i++ {
		data, _ := json.Marshal(map[string]any{
			"id":       i,
			"time":     time.Now().UTC(),
			"hostname": h.opts.Hostname,
		})
		if _, err := fmt.Fprintf(w, "id: %d\nevent: tick\ndata: %s\n\n", i, data); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		if count != 0 && i == count {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			_, _ = fmt.Fprint(w, "event: close\ndata: server shutting down\n\n")
			_ = rc.Flush()
			return
		case <-ticker.C:
		}
	}
}

// Hijack lets WebSocket upgrades through the response recorder.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		r.status, r.wroteHeader = http.StatusSwitchingProtocols, true
	}
	return conn, rw, err
}

// Flush lets streaming responses through the response recorder.
func (r *recorder) Flush() {
	r.wroteHeader = true
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

const (
	minSSEInterval = 10 * time.Millisecond
	maxSSEInterval = time.Hour
)
