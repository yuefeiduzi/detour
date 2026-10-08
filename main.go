package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
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
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const version = "0.4.1"

const (
	defaultListen     = "127.0.0.1:8787"
	defaultUpstream   = "https://api.openai.com/v1"
	defaultProxy      = "http://127.0.0.1:7897"
	defaultConfigName = "detour.json"
)

// Environment variables. They sit between the config file and the built-in
// defaults, so a bare binary (no detour.sh — e.g. on Windows) can still be
// configured without touching command-line flags.
const (
	envListen   = "DETOUR_LISTEN"
	envUpstream = "DETOUR_UPSTREAM"
	envProxy    = "DETOUR_PROXY"
	envConfig   = "DETOUR_CONFIG"
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

// applyEnv overlays DETOUR_* environment variables onto cfg. Empty values are
// ignored so that "unset" and "empty" behave the same.
func applyEnv(cfg *Config, getenv func(string) string) {
	if v := envValue(getenv, envListen); v != "" {
		cfg.Listen = v
	}
	if v := envValue(getenv, envUpstream); v != "" {
		cfg.Upstream = v
	}
	if v := envValue(getenv, envProxy); v != "" {
		cfg.Proxy = v
	}
}

// envValue reads an environment variable, dropping surrounding whitespace and
// the quotes Windows users habitually add (cmd keeps them: set DETOUR_PROXY="x").
func envValue(getenv func(string) string, key string) string {
	return strings.Trim(strings.TrimSpace(getenv(key)), `"`)
}

// defaultConfigPath is the canonical per-user location for detour.json:
// ~/.detour/detour.json  (C:\Users\<name>\.detour\detour.json on Windows).
// It works no matter where the binary lives or which directory it was started
// from, which is what a user-level setting needs.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".detour", defaultConfigName)
}

// discoverConfig finds the config file: the per-user default first, then next to
// the executable, then in the working directory. Returns "" when there is none,
// so running the binary with no config file keeps working as before.
func discoverConfig() string {
	candidates := []string{defaultConfigPath()}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), defaultConfigName))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, defaultConfigName))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return ""
}

// readConfigFile reads a config file and normalizes its encoding. Windows
// tooling happily writes UTF-8 with a BOM (PowerShell 5.1: Set-Content
// -Encoding UTF8) or UTF-16 (PowerShell 5.1: "... > config.json", Out-File,
// Notepad "Unicode"), and encoding/json accepts neither.
func readConfigFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeConfig(data), nil
}

func decodeConfig(data []byte) []byte {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}): // UTF-8 BOM
		return data[3:]
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}): // UTF-16LE BOM
		return utf16ToUTF8(data[2:], binary.LittleEndian)
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}): // UTF-16BE BOM
		return utf16ToUTF8(data[2:], binary.BigEndian)
	}
	if order := detectUTF16(data); order != nil {
		return utf16ToUTF8(data, order) // UTF-16 without BOM
	}
	return data
}

// detectUTF16 guesses the byte order of BOM-less UTF-16. ASCII JSON in UTF-16 is
// half NUL bytes — little-endian puts them at odd offsets, big-endian at even
// ones — while UTF-8 JSON never contains NUL. Returns nil for non-UTF-16 data.
func detectUTF16(data []byte) binary.ByteOrder {
	limit := min(len(data), 512)
	if limit < 8 {
		return nil
	}
	var even, odd int
	for i := 0; i < limit; i++ {
		if data[i] != 0 {
			continue
		}
		if i%2 == 0 {
			even++
		} else {
			odd++
		}
	}
	if even+odd == 0 {
		return nil
	}
	if odd >= even {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

func utf16ToUTF8(b []byte, order binary.ByteOrder) []byte {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, order.Uint16(b[i:i+2]))
	}
	return []byte(string(utf16.Decode(units)))
}

// loadConfig builds the effective configuration.
// Precedence, low to high: built-in defaults < DETOUR_* env vars < config file
// < command-line flags (applied by parseFlags).
func loadConfig(path string, getenv func(string) string) (*Config, error) {
	cfg := &Config{}
	applyEnv(cfg, getenv)
	cfg.applyDefaults()
	if path == "" {
		return cfg, nil
	}
	data, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	// encoding/json silently replaces invalid UTF-8 with U+FFFD, which would turn
	// a GBK-saved file into mojibake instead of an error. Fail loudly instead.
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is not valid UTF-8 — re-save it as UTF-8 (Notepad / VS Code, or PowerShell `Set-Content -Encoding utf8`)", path)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// printConfig writes the effective configuration as one line of JSON, plus the
// config file path on stderr. Scripts (detour.sh, the pi extension) use this
// instead of guessing which of defaults/config/env won.
func printConfig(cfg *Config, configPath string) {
	effective := map[string]any{
		"listen":   cfg.Listen,
		"upstream": cfg.Upstream,
		"proxy":    cfg.Proxy,
		"verbose":  cfg.Verbose,
		"config":   configPath,
	}
	if cfg.TLSCert != "" {
		effective["tls_cert"] = cfg.TLSCert
		effective["tls_key"] = cfg.TLSKey
	}
	out, err := json.Marshal(effective)
	if err != nil {
		log.Fatalf("print config: %v", err)
	}
	if configPath != "" {
		log.Printf("config file: %s", configPath)
	}
	fmt.Println(string(out))
}

// proxyHint is appended to upstream failures: a wrong ladder port (or a ladder
// that is not running) is by far the most common cause.
func proxyHint(cfg *Config) string {
	if cfg.Proxy == "" || cfg.Proxy == "direct" {
		return " (detour runs in direct mode — no proxy configured)"
	}
	return fmt.Sprintf(" (proxy %s: is your ladder running on that port? set -proxy/DETOUR_PROXY, or run `detour -check`)", cfg.Proxy)
}

func parseFlags(cfg *Config) string {
	configPath := flag.String("config", "", "path to a JSON config file (default: $DETOUR_CONFIG, else ~/.detour/detour.json, then next to the binary / in the cwd)")
	listen := flag.String("listen", "", "listen address, e.g. 127.0.0.1:8787 (overrides config file and $DETOUR_LISTEN)")
	upstream := flag.String("upstream", "", "real API base URL, e.g. https://api.openai.com/v1 (overrides config file and $DETOUR_UPSTREAM)")
	proxy := flag.String("proxy", "", "proxy URL: http://127.0.0.1:7897 or socks5://127.0.0.1:7891; \"direct\" for no proxy (overrides config file and $DETOUR_PROXY)")
	verbose := flag.Bool("v", false, "verbose: log request headers")
	check := flag.Bool("check", false, "probe connectivity through the proxy, then exit")
	tlsCert := flag.String("tls-cert", "", "serve HTTPS using this certificate (PEM)")
	tlsKey := flag.String("tls-key", "", "serve HTTPS using this key (PEM)")
	showVersion := flag.Bool("version", false, "print version and exit")
	showConfig := flag.Bool("print-config", false, "print the effective configuration as JSON and exit")
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
  detour -print-config                          # which settings actually apply
  detour -config detour.json                    # all settings from a file

Env vars: DETOUR_LISTEN, DETOUR_UPSTREAM, DETOUR_PROXY, DETOUR_CONFIG.
Config file lookup: ~/.detour/detour.json, then next to the binary, then the cwd.
Precedence: flags > config file > env vars > built-in defaults.

Usage of detour:
`, version)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("detour %s\n", version)
		os.Exit(0)
	}

	resolvedPath := *configPath
	if resolvedPath == "" {
		resolvedPath = strings.TrimSpace(envValue(os.Getenv, envConfig))
	}
	if resolvedPath == "" {
		resolvedPath = discoverConfig()
	}
	loaded, err := loadConfig(resolvedPath, os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	*cfg = *loaded
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
	if *showConfig {
		printConfig(cfg, resolvedPath)
		os.Exit(0)
	}
	if *check {
		runCheck(cfg)
		os.Exit(0)
	}
	return resolvedPath
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

// joinUpstreamPath joins the upstream path prefix onto an incoming request
// path, without doubling a shared trailing /v1 segment:
//
//	prefix "/zen/go/v1" + incoming "/responses"    -> "/zen/go/v1/responses"
//	prefix "/zen/go/v1" + incoming "/v1/messages" -> "/zen/go/v1/messages"
//	prefix "/zen/go/v1" + incoming "/zen/go/v1/..." -> unchanged
//
// The middle case matters for clients (e.g. pi, the Anthropic SDK) whose API
// shape always appends /v1/messages on top of the configured base URL.
func joinUpstreamPath(prefix, path string) string {
	if prefix == "" {
		return path
	}
	if strings.HasPrefix(path, prefix) {
		return path // client already sent the full upstream path
	}
	if strings.HasSuffix(prefix, "/v1") && strings.HasPrefix(path, "/v1") {
		return prefix + strings.TrimPrefix(path, "/v1")
	}
	return prefix + path
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
			r.URL.Path = joinUpstreamPath(prefix, r.URL.Path)
			r.URL.RawPath = ""
			r.URL.Scheme = upstream.Scheme
			r.URL.Host = upstream.Host
			r.Host = upstream.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream error: %s %s -> %s: %v%s", r.Method, r.URL.Path, upstream.Host, err, proxyHint(cfg))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			msg, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"type":    "detour_upstream_error",
					"message": "upstream request failed: " + err.Error() + proxyHint(cfg),
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
		fwd.Path = joinUpstreamPath(prefix, fwd.Path)
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
	configPath := parseFlags(&cfg) // may os.Exit (check/version modes, fatal errors)

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
	if configPath != "" {
		log.Printf("  config: %s", configPath)
	} else if hint := defaultConfigPath(); hint != "" {
		log.Printf("  config: none (default location would be %s)", hint)
	}
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
