package echo

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

//go:embed ui.html
var uiSource string

var uiTemplate = template.Must(template.New("ui").Funcs(template.FuncMap{
	"sorted": sortedKeys[[]string],
	"join":   strings.Join,
}).Parse(uiSource))

type uiData struct {
	*Echo
	JSON   string
	Curl   string
	Prefix string
	Nonce  string
}

// renderHTML renders the browser view and sets its headers. The page carries
// a strict CSP: only its own nonce-tagged inline style and script may run.
func (h *Handler) renderHTML(hdr http.Header, e *Echo) []byte {
	nonce := newID()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("Content-Security-Policy", "default-src 'none'; "+
		"style-src 'nonce-"+nonce+"'; script-src 'nonce-"+nonce+"'; "+
		"img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")

	var buf bytes.Buffer
	if err := uiTemplate.Execute(&buf, uiData{
		Echo:   e,
		JSON:   string(marshalJSON(e)),
		Curl:   curlCommand(e),
		Prefix: h.opts.RoutePrefix,
		Nonce:  nonce,
	}); err != nil {
		// The template is static and its data is plain structs; failing here
		// is a programming error, so make it loud in tests.
		panic(fmt.Sprintf("echo: rendering HTML view: %v", err))
	}
	return buf.Bytes()
}

// curlCommand reconstructs a curl invocation that replays the request.
func curlCommand(e *Echo) string {
	var sb strings.Builder
	sb.WriteString("curl")
	if e.Method != http.MethodGet {
		sb.WriteString(" -X " + e.Method)
	}
	skip := map[string]bool{
		"Accept-Encoding": true, "Connection": true, "Content-Length": true,
		"Upgrade-Insecure-Requests": true, "Cache-Control": true, "Pragma": true,
	}
	for _, k := range sortedKeys(e.Headers) {
		if skip[k] || strings.HasPrefix(k, "Sec-") {
			continue
		}
		for _, v := range e.Headers[k] {
			sb.WriteString(" \\\n  -H " + shellQuote(k+": "+v))
		}
	}
	if e.Body != nil && e.Body.Encoding == "utf-8" && e.Body.Data != "" {
		sb.WriteString(" \\\n  --data-raw " + shellQuote(e.Body.Data))
	}
	sb.WriteString(" \\\n  " + shellQuote(e.URL))
	return sb.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
