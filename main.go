package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const version = "0.2.0"

const (
	defaultListen   = "127.0.0.1:8787"
	defaultUpstream = "https://api.openai.com/v1"
	defaultProxy    = "http://127.0.0.1:7897"
)

// Config is the effective configuration; JSON field names match the config file.
type Config struct {
	Listen   string `json:"listen"`   // e.g. 127.0.0.1:8787
	Upstream string `json:"upstream"` // real API base URL, e.g. https://api.openai.com/v1
	Proxy    string `json:"proxy"`    // http://..., socks5://..., or "" / "direct"
	Verbose  bool   `json:"verbose"`  // log request headers
	TLSCert  string `json:"tls_cert"` // serve HTTPS with this PEM cert (optional)
	TLSKey   string `json:"tls_key"`  // serve HTTPS with this PEM key (optional)
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.Upstream == "" {
		c.Upstream = defaultUpstream
	}
	if c.Proxy == "" {
		c.Proxy = defaultProxy
	}
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	cfg.applyDefaults()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

func parseFlags(cfg *Config) {
	configPath := flag.String("config", "", "path to a JSON config file (optional)")
	listen := flag.String("listen", "", "listen address, e.g. 127.0.0.1:8787 (overrides config file)")
	upstream := flag.String("upstream", "", "real API base URL, e.g. https://api.openai.com/v1 (overrides config file)")
	proxy := flag.String("proxy", "", "proxy URL: http://127.0.0.1:7897 or socks5://127.0.0.1:7891; \"direct\" for no proxy (overrides config file)")
	verbose := flag.Bool("v", false, "verbose: log request headers")
	check := flag.Bool("check", false, "probe connectivity through the proxy, then exit")
	tlsCert := flag.String("tls-cert", "", "serve HTTPS using this certificate (PEM)")
	tlsKey := flag.String("tls-key", "", "serve HTTPS using this key (PEM)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, `detour %s - local model API relay through your proxy

Routes requests from a local model client (e.g. opencode pointed at
http://127.0.0.1:8787) to the real API through a local proxy, so only the
API traffic is proxied instead of running the proxy globally.

Examples:
  detour                                        # defaults: :8787 -> api.openai.com/v1 via 127.0.0.1:7897
  detour -upstream https://api.deepseek.com/v1  # different provider
  detour -proxy socks5://127.0.0.1:7891         # SOCKS5 proxy
  detour -proxy direct                          # no proxy (testing only)
  detour -check                                 # verify the proxy chain works
  detour -config detour.json                    # all settings from a file

Precedence: command-line flags > config file > built-in defaults.

Usage of detour:
`, version)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("detour %s\n", version)
		os.Exit(0)
	}

	if *configPath != "" {
		loaded, err := loadConfig(*configPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		*cfg = *loaded
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *upstream != "" {
		cfg.Upstream = *upstream
	}
	if *proxy != "" {
		cfg.Proxy = *proxy
	}
	if *verbose {
		cfg.Verbose = true
	}
	if *tlsCert != "" {
		cfg.TLSCert = *tlsCert
	}
	if *tlsKey != "" {
		cfg.TLSKey = *tlsKey
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		log.Fatal("tls-cert and tls-key must be provided together")
	}
	cfg.applyDefaults() // fill anything still unset
	if *check {
		runCheck(cfg)
		os.Exit(0)
	}
}

// buildTransport returns an http.Transport that routes through cfg.Proxy.
// http:// proxies use CONNECT tunneling (stdlib), socks5:// use a minimal
// built-in dialer that sends hostnames to the proxy (remote DNS).
func buildTransport(cfg *Config) (*http.Transport, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	proxy := cfg.Proxy
	if proxy == "" || proxy == "direct" {
		tr.Proxy = nil
		return tr, nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("bad proxy URL %q: %w", proxy, err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		password, _ := u.User.Password()
		tr.Proxy = nil
		tr.DialContext = (&socks5Dialer{
			proxyAddr: u.Host,
			username:  u.User.Username(),
			password:  password,
			timeout:   30 * time.Second,
		}).DialContext
		return tr, nil
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
		return tr, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (use http:// or socks5://, or \"direct\")", u.Scheme)
	}
}

// newRelay builds the reverse-proxy handler that forwards local requests to
// the upstream API. If the upstream URL has a path prefix (e.g. /v1) and the
// incoming path does not already start with it, the prefix is prepended, so
// both "baseURL = http://127.0.0.1:8787" and "...:8787/v1" work in the client.
func newRelay(cfg *Config, tr *http.Transport) (http.Handler, error) {
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("bad upstream URL %q: %w", cfg.Upstream, err)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("upstream scheme must be http or https, got %q", upstream.Scheme)
	}
	if upstream.Host == "" {
		return nil, fmt.Errorf("upstream URL %q has no host", cfg.Upstream)
	}
	prefix := strings.TrimSuffix(upstream.Path, "/")

	rp := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			r := pr.Out
			if prefix != "" && !strings.HasPrefix(r.URL.Path, prefix) {
				r.URL.Path = prefix + r.URL.Path
				r.URL.RawPath = ""
			}
			r.URL.Scheme = upstream.Scheme
			r.URL.Host = upstream.Host
			r.Host = upstream.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream error: %s %s -> %s: %v", r.Method, r.URL.Path, upstream.Host, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			msg, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"type":    "detour_upstream_error",
					"message": "upstream request failed: " + err.Error(),
				},
			})
			io.WriteString(w, string(msg))
		},
	}

	return logMiddleware(rp, upstream, prefix, cfg.Verbose), nil
}

// logMiddleware logs each request after completion and captures status/bytes.
func logMiddleware(next http.Handler, upstream *url.URL, prefix string, verbose bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if verbose {
			log.Printf("request: %s %s", r.Method, r.URL.RequestURI())
			logHeaders("req", r.Header)
		}
		start := time.Now()
		lw := &loggingWriter{ResponseWriter: w}
		next.ServeHTTP(lw, r)

		fwd := *r.URL
		fwd.Scheme = upstream.Scheme
		fwd.Host = upstream.Host
		if prefix != "" && !strings.HasPrefix(fwd.Path, prefix) {
			fwd.Path = prefix + fwd.Path
		}
		status := lw.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("%s %s -> %s | %d | %s | %s",
			r.Method, r.URL.RequestURI(), fwd.String(), status,
			time.Since(start).Round(time.Millisecond), humanBytes(lw.bytes))
	})
}

func logHeaders(prefix string, h http.Header) {
	for k, vs := range h {
		for _, v := range vs {
			if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
				v = "<redacted>"
			}
			log.Printf("  %s %s: %s", prefix, k, v)
		}
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// loggingWriter captures the response status and byte count while keeping
// streaming (Flush) and upgrade (Hijack) support intact.
type loggingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *loggingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *loggingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *loggingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported by underlying writer")
	}
	return h.Hijack()
}

func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// runCheck probes the proxy chain: one neutral endpoint and the upstream URL.
func runCheck(cfg *Config) {
	tr, err := buildTransport(cfg)
	if err != nil {
		log.Fatalf("proxy config: %v", err)
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	probes := []struct{ name, url string }{
		{"proxy reachability", "https://www.gstatic.com/generate_204"},
		{"upstream " + cfg.Upstream, cfg.Upstream},
	}
	ok := true
	for _, p := range probes {
		start := time.Now()
		resp, err := client.Get(p.url)
		if err != nil {
			fmt.Printf("FAIL  %-30s %v\n", p.name, err)
			ok = false
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		fmt.Printf("OK    %-30s HTTP %d (%.0f ms)\n", p.name, resp.StatusCode, time.Since(start).Seconds()*1000)
	}
	if !ok {
		fmt.Println("\ncheck failed — see errors above. Is the proxy running? Is the node alive?")
		os.Exit(1)
	}
	fmt.Println("\ncheck passed.")
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func main() {
	log.SetFlags(log.LstdFlags)
	var cfg Config
	parseFlags(&cfg) // may os.Exit (check/version modes, fatal errors)

	tr, err := buildTransport(&cfg)
	if err != nil {
		log.Fatalf("proxy config: %v", err)
	}
	handler, err := newRelay(&cfg, tr)
	if err != nil {
		log.Fatalf("relay config: %v", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}

	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	if !isLoopback(cfg.Listen) {
		log.Printf("WARNING: listening on %s — the relay forwards your API keys; keep it on 127.0.0.1 unless you know what you're doing", cfg.Listen)
	}
	log.Printf("detour %s listening on %s://%s", version, scheme, ln.Addr())
	log.Printf("  upstream: %s", cfg.Upstream)
	if cfg.Proxy == "" || cfg.Proxy == "direct" {
		log.Printf("  proxy: direct (no proxy)")
	} else {
		log.Printf("  proxy: %s", cfg.Proxy)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" {
			serveErr <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			serveErr <- srv.Serve(ln)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
		log.Printf("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}
	log.Printf("detour stopped")
}
