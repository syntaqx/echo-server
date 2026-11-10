# Changelog

All notable changes to this project are documented here. The project follows
[Semantic Versioning](https://semver.org); until v1.0.0, minor releases may
include breaking changes, which are always listed here.

## v0.1.0

The first tagged release, and a rewrite: echo-server now actually echoes.

### Added

- Echo of every request: method, URL, query, headers, cookies, content length,
  transfer encoding, trailers, body (parsed for JSON, forms and multipart),
  client IP, TLS details and the answering host. JSON for clients, an HTML view
  for browsers, raw HTTP via `echo_format=text`, the body alone via
  `echo_format=raw`. Paths are echoed exactly as sent, including `//`, `.`
  and `..` segments.
- Response controls on any path: `X-Echo-Status`, `X-Echo-Delay`,
  `X-Echo-Header`, `X-Echo-Format` (or `echo_*` query parameters), plus
  `/status/{code}` and `/delay/{duration}`.
- WebSocket echo and server-sent events at `/sse`.
- HTTP/2 over TLS and h2c, optional TLS with client-certificate inspection.
- `/healthz`, `/livez`, `/readyz`, Prometheus `/metrics` and opt-in `/env`.
- `-route-prefix` to move built-in routes out of the way of echoed paths.
- Structured access logs (text or JSON), graceful draining that closes
  WebSockets, SSE streams and pending delays, and configuration by flag or
  environment variable with strict validation.
- `echo-server healthcheck`, which finds the running server however it was
  started and pins its TLS certificate, and `echo-server version`.
- Distroless, nonroot, multi-arch images (amd64, arm64, arm/v7) with a built-in
  `HEALTHCHECK`, and release binaries for Linux, macOS, Windows and FreeBSD.
- Usable as a Go library: `echo.New(echo.Options{})` returns an `http.Handler`.
  The API may still change before v1.0.0.

### Security

- CORS allows any origin but never credentials, since responses echo cookies.
- Echoed responses are sent with `Content-Security-Policy: sandbox` and
  `nosniff`; the HTML view uses a nonce-based CSP.
- `Location`, `Refresh`, `Set-Cookie` and `Clear-Site-Data` can't be set from
  the query string, so links can't turn the server into an open redirect.

### Changes from the untagged `main` builds

These affect anyone running `syntaqx/echo-server:latest` or `main` before this
release:

- Responses are the echo, not `Hello, world!`. If you only need a static 200,
  use `/healthz`.
- The `handler` package is gone; import `github.com/syntaqx/echo-server`.
- The image uses `ENTRYPOINT` and has no shell: pass flags directly
  (`docker run syntaqx/echo-server -addr :9000`), and don't expect `sh`.
- `latest` now follows releases instead of every push to `main`; use `main`
  to track the branch.
- `PORT` still works; `ECHO_ADDR` and `-addr` are new alternatives.
