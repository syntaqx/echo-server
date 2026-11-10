# Changelog

## v2.0.0

A rewrite: echo-server now actually echoes.

### Added

- Echo of every request: method, URL, query, headers, cookies, body (parsed
  for JSON, forms and multipart), client IP, TLS details and the answering
  host. JSON for clients, an HTML view for browsers, raw HTTP via `echo_format=text`.
- Response controls on any path: `X-Echo-Status`, `X-Echo-Delay`,
  `X-Echo-Header`, `X-Echo-Format` (or `echo_*` query parameters), plus
  `/status/{code}` and `/delay/{duration}`.
- WebSocket echo on any path and server-sent events at `/sse`.
- HTTP/2 over TLS and h2c, optional TLS with client-certificate inspection.
- `/healthz`, `/livez`, `/readyz`, Prometheus `/metrics` and opt-in `/env`.
- `-route-prefix` to move built-in routes out of the way of echoed paths.
- Structured access logs (text or JSON), graceful draining that closes
  long-lived streams, and configuration by flag or environment variable.
- `echo-server healthcheck` and `echo-server version` subcommands.
- Distroless, nonroot, multi-arch images (amd64, arm64, arm/v7) with a built-in
  `HEALTHCHECK`, and release binaries for Linux, macOS, Windows and FreeBSD.
- Usable as a Go library: `echo.New(echo.Options{})` returns an `http.Handler`.

### Breaking changes

- Responses are the echo, not `Hello, world!`. If you only need a static 200,
  use `/healthz`.
- The `handler` package is gone; import `github.com/syntaqx/echo-server`.
- The image uses `ENTRYPOINT` and has no shell: pass flags directly
  (`docker run syntaqx/echo-server -addr :9000`), and don't expect `sh`.
- `PORT` still works; `ECHO_ADDR` and `-addr` are new alternatives.
