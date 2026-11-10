# echo-server

[![Go](https://github.com/syntaqx/echo-server/actions/workflows/go.yml/badge.svg)](https://github.com/syntaqx/echo-server/actions/workflows/go.yml)
[![codecov](https://codecov.io/gh/syntaqx/echo-server/branch/main/graph/badge.svg?token=eJJWSkWaYm)](https://codecov.io/gh/syntaqx/echo-server)
[![Docker Image Version (latest semver)](https://img.shields.io/docker/v/syntaqx/echo-server?logo=docker&sort=semver)](https://hub.docker.com/r/syntaqx/echo-server)
[![Go Reference](https://pkg.go.dev/badge/github.com/syntaqx/echo-server.svg)](https://pkg.go.dev/github.com/syntaqx/echo-server)

An HTTP server that tells you exactly what it received. Point a proxy, load
balancer, ingress, service mesh or HTTP client at it and see the request as it
arrived: method, path, query, headers, cookies, body, client IP, TLS and which
instance answered.

- **Any path, any method.** JSON for clients, a readable page for browsers, the raw HTTP wire format on request.
- **Shape the response.** Set the status, delay and response headers from the request itself.
- **WebSockets and SSE.** Upgrade any path to a WebSocket echo, or stream server-sent events.
- **HTTP/2 everywhere.** h2 over TLS and h2c over plaintext on the same port; mTLS client certs are shown.
- **Built for clusters.** Liveness and readiness probes, graceful draining that closes streams, Prometheus metrics, JSON logs.
- **Small and static.** One Go binary on a distroless, nonroot image with a built-in healthcheck, for amd64, arm64 and arm/v7.

## Quick start

```sh
docker run --rm -p 8080:8080 syntaqx/echo-server
```

```sh
curl 'localhost:8080/hello?name=gopher' -H 'Content-Type: application/json' -d '{"hi":true}'
```

```json
{
  "method": "POST",
  "url": "http://localhost:8080/hello?name=gopher",
  "path": "/hello",
  "query": { "name": ["gopher"] },
  "protocol": "HTTP/1.1",
  "host": "localhost:8080",
  "remote_addr": "172.17.0.1:40304",
  "client_ip": "172.17.0.1",
  "headers": {
    "Accept": ["*/*"],
    "Content-Length": ["11"],
    "Content-Type": ["application/json"],
    "User-Agent": ["curl/8.14.1"]
  },
  "body": {
    "content_type": "application/json",
    "size": 11,
    "encoding": "utf-8",
    "data": "{\"hi\":true}",
    "json": { "hi": true }
  },
  "server": { "hostname": "echo-7d9f4c", "version": "v2.0.0" },
  "request_id": "26f307f79679cf4f",
  "time": "2026-11-10T08:05:22.220735753Z"
}
```

Open <http://localhost:8080> in a browser to see the same request as a page,
with copy-as-curl and a live WebSocket tester.

Prefer a binary? Download one for Linux, macOS, Windows or FreeBSD from
[Releases](https://github.com/syntaqx/echo-server/releases), or
`go install github.com/syntaqx/echo-server/cmd/echo-server@latest`.

## Shaping the response

These work on **every echoed path**, as a request header or a query parameter:

| Header          | Query parameter | Effect                                                          |
| --------------- | --------------- | --------------------------------------------------------------- |
| `X-Echo-Status` | `echo_status`   | Respond with this status code (200–599).                        |
| `X-Echo-Delay`  | `echo_delay`    | Wait before responding: `250ms`, `2s`, or bare milliseconds.    |
| `X-Echo-Header` | `echo_header`   | Add a response header, `Name: value`. Repeatable.               |
| `X-Echo-Format` | `echo_format`   | `json`, `html`, `text` (HTTP wire format) or `raw` (body only). |

```sh
# A slow, failing upstream
curl -i localhost:8080/api -H 'X-Echo-Status: 503' -H 'X-Echo-Delay: 2s'

# Cache behaviour behind a CDN
curl -i 'localhost:8080/asset.js?echo_header=Cache-Control:max-age=60'

# The request as raw HTTP
curl 'localhost:8080/anything?echo_format=text'

# The body back verbatim, keeping its Content-Type
curl 'localhost:8080/?echo_format=raw' -H 'Content-Type: text/csv' --data-binary @data.csv
```

Without a format, browsers (`Accept: text/html`) get HTML, `Accept: text/plain`
gets text and everything else gets JSON. Bodies are parsed for JSON,
URL-encoded forms and multipart uploads (files are summarized, not dumped).
Binary bodies are base64-encoded, and bodies over `-max-body` are truncated and
marked as such.

Every response carries `X-Echo-Server` (the hostname, handy behind a load
balancer) and `X-Request-Id` (yours if you sent one).

## Routes

| Route                | Description                                                          |
| -------------------- | -------------------------------------------------------------------- |
| `/*`                 | Echo the request. A WebSocket upgrade on any path echoes messages.   |
| `/status/{code}`     | Echo with the given status code.                                     |
| `/delay/{duration}`  | Echo after a delay.                                                  |
| `/sse`               | Server-sent events. `?interval=500ms&count=10` (count 0 = forever).  |
| `/healthz`, `/livez` | Liveness probe.                                                      |
| `/readyz`            | Readiness probe. Returns 503 once shutdown begins.                   |
| `/metrics`           | Prometheus metrics: requests, latency, in-flight, WebSockets.        |
| `/env`               | Environment variables. **Off unless `-enable-env` is set.**          |

```sh
websocat ws://localhost:8080/ws
curl -N 'localhost:8080/sse?interval=500ms&count=5'
```

**Need to echo `/healthz` or `/metrics` themselves?** Proxying a real app's
paths through the echo server can collide with these routes. Move them under a
prefix and every other path is echoed:

```sh
docker run --rm -p 8080:8080 syntaqx/echo-server -route-prefix /_echo
curl localhost:8080/healthz          # echoed
curl localhost:8080/_echo/healthz    # liveness probe
```

## Configuration

Every flag has an environment variable; flags take precedence. Invalid values
are rejected at startup rather than silently ignored.

| Flag                | Environment             | Default   | Description                                                 |
| ------------------- | ----------------------- | --------- | ----------------------------------------------------------- |
| `-addr`             | `ECHO_ADDR` or `PORT`   | `:8080`   | Listen address. `PORT` alone sets the port.                 |
| `-route-prefix`     | `ECHO_ROUTE_PREFIX`     |           | Serve built-in routes under a prefix such as `/_echo`.      |
| `-tls-cert`         | `ECHO_TLS_CERT`         |           | Certificate file; enables HTTPS and requests client certs.  |
| `-tls-key`          | `ECHO_TLS_KEY`          |           | Private key file.                                           |
| `-log-format`       | `ECHO_LOG_FORMAT`       | `text`    | `text` or `json`.                                           |
| `-log-level`        | `ECHO_LOG_LEVEL`        | `info`    | `debug`, `info`, `warn` or `error`.                         |
| `-access-log`       | `ECHO_ACCESS_LOG`       | `true`    | Log each request. Probes and metrics scrapes are skipped.   |
| `-max-body`         | `ECHO_MAX_BODY`         | `1048576` | Max body bytes echoed (also the WebSocket message limit).   |
| `-max-delay`        | `ECHO_MAX_DELAY`        | `30s`     | Longest delay a client may request.                         |
| `-enable-env`       | `ECHO_ENABLE_ENV`       | `false`   | Serve `/env`. Environments often hold secrets.              |
| `-shutdown-delay`   | `ECHO_SHUTDOWN_DELAY`   | `0s`      | How long `/readyz` reports draining before shutdown starts. |
| `-shutdown-timeout` | `ECHO_SHUTDOWN_TIMEOUT` | `15s`     | How long to wait for in-flight requests.                    |
| `-state-file`       | `ECHO_STATE_FILE`       | temp dir  | Where the server records its health URL for `healthcheck`.  |

### Healthcheck

`echo-server healthcheck` exits 0 if the local server is healthy. The image's
`HEALTHCHECK` uses it, so no curl or shell is needed. It finds the running
server through the state file, so it keeps working however the server was
started, including with flags like `-addr :9000 -route-prefix /_echo` or TLS.
Pass `-url` to probe something specific.

### Shutdown

On `SIGTERM` or `SIGINT`, `/readyz` starts returning 503, the server waits
`-shutdown-delay`, then stops accepting connections. WebSockets are closed with
1001 (going away), SSE streams get a final `close` event, and pending delays
respond early, so draining doesn't wait on long-lived connections. A second
signal exits immediately.

`-shutdown-delay` defaults to 0 so `docker stop` and Ctrl-C are instant. Behind
a load balancer, set it to a few seconds so traffic moves away before
connections close.

### Kubernetes

```yaml
containers:
  - name: echo
    image: syntaqx/echo-server
    env:
      - { name: ECHO_LOG_FORMAT, value: json }
      - { name: ECHO_SHUTDOWN_DELAY, value: 5s }
    ports: [{ containerPort: 8080 }]
    livenessProbe: { httpGet: { path: /livez, port: 8080 } }
    readinessProbe: { httpGet: { path: /readyz, port: 8080 } }
```

## As a Go library

The handler can be mounted in any server, including test servers:

```go
import echo "github.com/syntaqx/echo-server"

srv := httptest.NewServer(echo.New(echo.Options{}))
defer srv.Close()
// point your client, proxy or middleware at srv.URL
```

If you run it in your own `http.Server`, call `srv.RegisterOnShutdown(h.CloseStreams)`
so `Shutdown` doesn't wait on open WebSockets and SSE streams.

## Security

An echo server reflects whatever it's sent, cookies included, so:

- **Give it its own domain.** Don't host it on a subdomain that receives
  cookies from a parent domain you care about. Anyone who can get a browser to
  visit it can see those cookies in the response.
- **Cross-origin reads never include credentials.** CORS allows any origin but
  never sets `Access-Control-Allow-Credentials`, so other sites can't use a
  visitor's cookies to read their echo.
- **Echoed content can't run as this site.** Responses other than the HTML view
  are sent with `Content-Security-Policy: sandbox` and `nosniff`, even if a
  client sets `Content-Type: text/html`. The HTML view has a strict nonce-based
  CSP and escapes everything it shows.
- **`client_ip` isn't trusted.** It honors `Forwarded`, `X-Forwarded-For` and
  `X-Real-IP` because showing what proxies send is the point, and any client
  can set those.
- **`/env` stays off** unless you enable it.

Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
