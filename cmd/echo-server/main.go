// Command echo-server runs an HTTP server that echoes requests back.
//
// Every flag can also be set with an environment variable; flags win.
//
//	echo-server [flags]               serve (default)
//	echo-server healthcheck [flags]   probe a running server; exit 0 if healthy
//	echo-server version               print the version
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	echo "github.com/syntaqx/echo-server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

type config struct {
	addr            string
	tlsCert         string
	tlsKey          string
	routePrefix     string
	logFormat       string
	logLevel        string
	accessLog       bool
	maxBody         int64
	maxDelay        time.Duration
	enableEnv       bool
	shutdownDelay   time.Duration
	shutdownTimeout time.Duration
	stateFile       string

	// healthcheck only
	url     string
	addrSet bool // -addr given explicitly on the command line
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // restore default handling: a second signal kills immediately
	}()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process globals, so it can be tested.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve", "healthcheck":
	case "version":
		_, _ = fmt.Fprintln(stdout, buildVersion())
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q (want serve, healthcheck or version)\n", cmd)
		return 2
	}

	cfg, err := parseConfig(cmd, args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}

	if cmd == "healthcheck" {
		if err := healthcheck(cfg); err != nil {
			_, _ = fmt.Fprintln(stderr, "unhealthy:", err)
			return 1
		}
		return 0
	}
	if err := serve(ctx, cfg, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, "echo-server:", err)
		return 1
	}
	return 0
}

func parseConfig(cmd string, args []string, stderr io.Writer) (config, error) {
	var c config
	var envErrs []error
	fs := flag.NewFlagSet("echo-server "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)

	defaultAddr := ":" + env("PORT", "8080")
	fs.StringVar(&c.addr, "addr", env("ECHO_ADDR", defaultAddr), "listen address (env ECHO_ADDR, or PORT)")
	fs.StringVar(&c.tlsCert, "tls-cert", env("ECHO_TLS_CERT", ""), "TLS certificate file; enables HTTPS (env ECHO_TLS_CERT)")
	fs.StringVar(&c.tlsKey, "tls-key", env("ECHO_TLS_KEY", ""), "TLS private key file (env ECHO_TLS_KEY)")
	fs.StringVar(&c.routePrefix, "route-prefix", env("ECHO_ROUTE_PREFIX", ""), "serve built-in routes under this prefix, e.g. /_echo, so every other path is echoed (env ECHO_ROUTE_PREFIX)")
	fs.StringVar(&c.stateFile, "state-file", env("ECHO_STATE_FILE", filepath.Join(os.TempDir(), "echo-server.health")), "where serve records its health URL for the healthcheck command (env ECHO_STATE_FILE)")
	if cmd == "healthcheck" {
		fs.StringVar(&c.url, "url", "", "health URL to probe (default: the running server's, from the state file)")
	} else {
		fs.StringVar(&c.logFormat, "log-format", env("ECHO_LOG_FORMAT", "text"), "log format: text or json (env ECHO_LOG_FORMAT)")
		fs.StringVar(&c.logLevel, "log-level", env("ECHO_LOG_LEVEL", "info"), "log level: debug, info, warn, error (env ECHO_LOG_LEVEL)")
		fs.BoolVar(&c.accessLog, "access-log", envBool(&envErrs, "ECHO_ACCESS_LOG", true), "log every request (env ECHO_ACCESS_LOG)")
		fs.Int64Var(&c.maxBody, "max-body", envInt(&envErrs, "ECHO_MAX_BODY", 1<<20), "max request body bytes to echo (env ECHO_MAX_BODY)")
		fs.DurationVar(&c.maxDelay, "max-delay", envDuration(&envErrs, "ECHO_MAX_DELAY", 30*time.Second), "max delay a client may request (env ECHO_MAX_DELAY)")
		fs.BoolVar(&c.enableEnv, "enable-env", envBool(&envErrs, "ECHO_ENABLE_ENV", false), "expose environment variables at /env (env ECHO_ENABLE_ENV)")
		fs.DurationVar(&c.shutdownDelay, "shutdown-delay", envDuration(&envErrs, "ECHO_SHUTDOWN_DELAY", 0), "how long /readyz reports draining before shutdown (env ECHO_SHUTDOWN_DELAY)")
		fs.DurationVar(&c.shutdownTimeout, "shutdown-timeout", envDuration(&envErrs, "ECHO_SHUTDOWN_TIMEOUT", 15*time.Second), "max time to wait for in-flight requests (env ECHO_SHUTDOWN_TIMEOUT)")
	}

	if err := errors.Join(envErrs...); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return c, err
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return c, errors.New("unexpected arguments")
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			c.addrSet = true
		}
	})
	if (c.tlsCert == "") != (c.tlsKey == "") {
		_, _ = fmt.Fprintln(stderr, "both -tls-cert and -tls-key are required to enable TLS")
		return c, errors.New("incomplete TLS config")
	}
	if err := echo.ValidatePrefix(c.routePrefix); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return c, err
	}
	return c, nil
}

func serve(ctx context.Context, cfg config, stderr io.Writer) error {
	logger, err := newLogger(stderr, cfg.logFormat, cfg.logLevel)
	if err != nil {
		return err
	}

	opts := echo.Options{
		MaxBodyBytes: cfg.maxBody,
		MaxDelay:     cfg.maxDelay,
		RoutePrefix:  cfg.routePrefix,
		EnableEnv:    cfg.enableEnv,
		Version:      buildVersion(),
	}
	if cfg.accessLog {
		opts.Logger = logger
	}
	h := echo.New(opts)

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// Serve HTTP/1.1 and HTTP/2 on the same port, with or without TLS (h2c).
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetHTTP2(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	// Shutdown doesn't interrupt running handlers; end WebSockets, SSE
	// streams and pending delays so draining finishes promptly.
	srv.RegisterOnShutdown(h.CloseStreams)

	scheme := "http"
	if cfg.tlsCert != "" {
		scheme = "https"
		cert, err := tls.LoadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
		if err != nil {
			return fmt.Errorf("loading TLS key pair: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			// Ask for (but don't require) client certs so mTLS can be inspected.
			ClientAuth: tls.RequestClientCert,
		}
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}

	healthURL := scheme + "://" + probeHost(ln.Addr()) + h.HealthPath()
	if cfg.stateFile != "" {
		if err := os.WriteFile(cfg.stateFile, []byte(healthURL+"\n"), 0o644); err != nil {
			logger.Debug("could not write state file", "path", cfg.stateFile, "err", err)
		} else {
			defer func() { _ = os.Remove(cfg.stateFile) }()
		}
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("echo-server listening", "addr", ln.Addr().String(), "scheme", scheme, "version", opts.Version)
		if scheme == "https" {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	h.SetReady(false)
	if cfg.shutdownDelay > 0 {
		logger.Info("draining", "delay", cfg.shutdownDelay)
		time.Sleep(cfg.shutdownDelay)
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// probeHost turns a listener address into one a local client can dial:
// wildcard binds become loopback.
func probeHost(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// healthcheck probes the local server's liveness endpoint. It lets distroless
// images, which have no curl or wget, define a Docker HEALTHCHECK.
//
// The URL comes from, in order: -url; -addr (with -tls-cert and
// -route-prefix); the state file the running server wrote, which reflects the
// flags it was actually started with; then the environment and defaults.
func healthcheck(cfg config) error {
	url := cfg.url
	if url == "" && !cfg.addrSet {
		if b, err := os.ReadFile(cfg.stateFile); err == nil {
			url = strings.TrimSpace(string(b))
		}
	}
	if url == "" {
		_, port, err := net.SplitHostPort(cfg.addr)
		if err != nil {
			return err
		}
		scheme := "http"
		if cfg.tlsCert != "" {
			scheme = "https"
		}
		url = scheme + "://" + net.JoinHostPort("127.0.0.1", port) + strings.TrimSuffix(cfg.routePrefix, "/") + "/healthz"
	}

	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // probing our own local server
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	return nil
}

func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	hopts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, hopts)), nil
	case "text", "":
		return slog.New(slog.NewTextHandler(w, hopts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q: want text or json", format)
	}
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// The env* helpers fall back when a variable is unset, and record an error
// when it is set to something unparseable rather than silently ignoring it.

func envBool(errs *[]error, key string, fallback bool) bool {
	return envParse(errs, key, fallback, strconv.ParseBool)
}

func envInt(errs *[]error, key string, fallback int64) int64 {
	return envParse(errs, key, fallback, func(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) })
}

func envDuration(errs *[]error, key string, fallback time.Duration) time.Duration {
	return envParse(errs, key, fallback, time.ParseDuration)
}

func envParse[T any](errs *[]error, key string, fallback T, parse func(string) (T, error)) T {
	s, ok := os.LookupEnv(key)
	if !ok || s == "" {
		return fallback
	}
	v, err := parse(s)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("invalid %s=%q", key, s))
		return fallback
	}
	return v
}
