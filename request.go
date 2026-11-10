package echo

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Echo is the reflection of a single request, as returned to the client.
type Echo struct {
	Method     string              `json:"method"`
	URL        string              `json:"url"`
	Path       string              `json:"path"`
	Query      map[string][]string `json:"query,omitempty"`
	Proto      string              `json:"protocol"`
	Host       string              `json:"host"`
	RemoteAddr string              `json:"remote_addr"`
	ClientIP   string              `json:"client_ip"`
	Headers    map[string][]string `json:"headers"`
	Cookies    map[string]string   `json:"cookies,omitempty"`
	Body       *Body               `json:"body,omitempty"`
	TLS        *TLSInfo            `json:"tls,omitempty"`
	Server     ServerInfo          `json:"server"`
	RequestID  string              `json:"request_id"`
	Time       time.Time           `json:"time"`
}

// Body describes the request payload.
type Body struct {
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	Truncated   bool   `json:"truncated,omitempty"`
	// Encoding is "utf-8" when Data is the body verbatim, or "base64" for
	// binary payloads.
	Encoding string              `json:"encoding"`
	Data     string              `json:"data"`
	JSON     json.RawMessage     `json:"json,omitempty"`
	Form     map[string][]string `json:"form,omitempty"`
	Files    []File              `json:"files,omitempty"`
}

// File summarizes one uploaded multipart file.
type File struct {
	Field       string `json:"field"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
}

// TLSInfo describes the TLS connection, when there is one.
type TLSInfo struct {
	Version            string   `json:"version"`
	CipherSuite        string   `json:"cipher_suite"`
	ServerName         string   `json:"server_name,omitempty"`
	NegotiatedProtocol string   `json:"alpn,omitempty"`
	Resumed            bool     `json:"resumed"`
	PeerCertificates   []string `json:"peer_certificates,omitempty"`
}

// ServerInfo identifies the instance that answered.
type ServerInfo struct {
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

// capture reads the request (including up to MaxBodyBytes of body) into an Echo.
func (h *Handler) capture(w http.ResponseWriter, r *http.Request) (*Echo, []byte, error) {
	e := &Echo{
		Method:     r.Method,
		URL:        fullURL(r),
		Path:       r.URL.Path,
		Proto:      r.Proto,
		Host:       r.Host,
		RemoteAddr: r.RemoteAddr,
		ClientIP:   clientIP(r),
		Headers:    headerMap(r.Header),
		TLS:        tlsInfo(r.TLS),
		Server:     ServerInfo{Hostname: h.opts.Hostname, Version: h.opts.Version},
		RequestID:  w.Header().Get("X-Request-Id"),
		Time:       time.Now().UTC(),
	}
	if q := r.URL.Query(); len(q) > 0 {
		e.Query = q
	}
	if cs := r.Cookies(); len(cs) > 0 {
		e.Cookies = make(map[string]string, len(cs))
		for _, c := range cs {
			e.Cookies[c.Name] = c.Value
		}
	}

	raw, truncated, err := readBody(r.Body, h.opts.MaxBodyBytes)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > 0 || truncated {
		e.Body = describeBody(r.Header.Get("Content-Type"), raw, truncated)
	}
	return e, raw, nil
}

func readBody(body io.Reader, limit int64) ([]byte, bool, error) {
	if body == nil || body == http.NoBody {
		return nil, false, nil
	}
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > limit {
		// Drain (bounded) so keep-alive connections stay usable.
		_, _ = io.CopyN(io.Discard, body, 64<<20)
		return buf[:limit], true, nil
	}
	return buf, false, nil
}

func describeBody(contentType string, raw []byte, truncated bool) *Body {
	b := &Body{ContentType: contentType, Size: int64(len(raw)), Truncated: truncated}
	if utf8.Valid(raw) {
		b.Encoding, b.Data = "utf-8", string(raw)
	} else {
		b.Encoding, b.Data = "base64", base64.StdEncoding.EncodeToString(raw)
	}
	if truncated {
		return b
	}

	mediaType, params, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		if json.Valid(raw) {
			b.JSON = json.RawMessage(bytes.Clone(raw))
		}
	case mediaType == "application/x-www-form-urlencoded":
		if v, err := url.ParseQuery(string(raw)); err == nil {
			b.Form = v
		}
	case mediaType == "multipart/form-data" && params["boundary"] != "":
		form, files := parseMultipart(raw, params["boundary"])
		b.Form, b.Files = form, files
		if len(files) > 0 {
			// File contents are summarized, not dumped.
			b.Encoding, b.Data = "omitted", ""
		}
	}
	return b
}

func parseMultipart(raw []byte, boundary string) (map[string][]string, []File) {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	form := map[string][]string{}
	var files []File
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(p)
		if p.FileName() != "" {
			files = append(files, File{
				Field:       p.FormName(),
				Filename:    p.FileName(),
				ContentType: p.Header.Get("Content-Type"),
				Size:        int64(len(data)),
			})
		} else {
			form[p.FormName()] = append(form[p.FormName()], string(data))
		}
		_ = p.Close()
	}
	if len(form) == 0 {
		form = nil
	}
	return form, files
}

func headerMap(h http.Header) map[string][]string {
	m := make(map[string][]string, len(h))
	for k, v := range h {
		m[k] = v
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func fullURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: r.Host, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	return u.String()
}

// clientIP returns the originating client address, honoring the proxy headers
// an echo server exists to inspect. It is informational, not a security
// boundary: any client can set these headers.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("Forwarded"); fwd != "" {
		for _, part := range strings.Split(strings.Split(fwd, ",")[0], ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && strings.EqualFold(k, "for") {
				v = strings.Trim(v, `"`)
				if host, _, err := net.SplitHostPort(v); err == nil {
					v = host
				}
				return strings.Trim(v, "[]")
			}
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if xr := r.Header.Get("X-Real-Ip"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tlsInfo(cs *tls.ConnectionState) *TLSInfo {
	if cs == nil {
		return nil
	}
	t := &TLSInfo{
		Version:            tls.VersionName(cs.Version),
		CipherSuite:        tls.CipherSuiteName(cs.CipherSuite),
		ServerName:         cs.ServerName,
		NegotiatedProtocol: cs.NegotiatedProtocol,
		Resumed:            cs.DidResume,
	}
	for _, c := range cs.PeerCertificates {
		t.PeerCertificates = append(t.PeerCertificates, c.Subject.String())
	}
	return t
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// rawRequest renders the request in HTTP/1.1 wire format.
func rawRequest(r *http.Request, body []byte, truncated bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s %s\r\n", r.Method, r.URL.RequestURI(), r.Proto)
	fmt.Fprintf(&sb, "Host: %s\r\n", r.Host)
	for _, k := range sortedKeys(r.Header) {
		for _, v := range r.Header[k] {
			fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
		}
	}
	sb.WriteString("\r\n")
	sb.Write(body)
	if truncated {
		sb.WriteString("\r\n[body truncated]")
	}
	return sb.String()
}
