package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard) // keep test output clean
	os.Exit(m.Run())
}

// --- test helpers -----------------------------------------------------------

type recorder struct {
	mu       sync.Mutex
	paths    []string
	hosts    []string
	auth     string
	body     string
	lastPath string
	lastHost string
}

func (r *recorder) record(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.hosts = append(r.hosts, req.Host)
	r.lastPath = req.URL.Path
	r.lastHost = req.Host
	r.auth = req.Header.Get("Authorization")
	r.body = string(body)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

func genTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startTLSUpstream runs an HTTPS test server with a self-signed cert.
func startTLSUpstream(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{genTestCert(t)}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// startFakeProxy runs an HTTP proxy that supports both CONNECT tunneling and
// absolute-form forwarding of plain HTTP requests.
func startFakeProxy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			dst, err := net.Dial("tcp", r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				dst.Close()
				return
			}
			fmt.Fprintf(buf, "HTTP/1.1 200 Connection Established\r\n\r\n")
			buf.Flush()
			go func() { io.Copy(dst, conn); dst.Close() }()
			go func() { io.Copy(conn, dst); conn.Close() }()
			return
		}
		// absolute-form request (plain http upstream through the proxy)
		body, _ := io.ReadAll(r.Body)
		var raw bytes.Buffer
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		if err := r.Write(&raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		dst, err := net.Dial("tcp", r.URL.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer dst.Close()
		if _, err := dst.Write(raw.Bytes()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.ReadResponse(bufio.NewReader(dst), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// startFakeSOCKS5 runs a minimal SOCKS5 server (no auth) that tunnels TCP.
func startFakeSOCKS5(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocksConn(conn)
		}
	}()
	return ln.Addr().String()
}

func handleSocksConn(conn net.Conn) {
	var greet [2]byte
	if _, err := io.ReadFull(conn, greet[:]); err != nil {
		return
	}
	if greet[0] != 0x05 {
		return
	}
	methods := make([]byte, greet[1]) // consume the method list
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return
	}
	var host string
	switch head[3] {
	case 0x01:
		b := make([]byte, 4)
		io.ReadFull(conn, b)
		host = net.IP(b).String()
	case 0x03:
		var l [1]byte
		io.ReadFull(conn, l[:])
		b := make([]byte, l[0])
		io.ReadFull(conn, b)
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		io.ReadFull(conn, b)
		host = net.IP(b).String()
	default:
		return
	}
	var port [2]byte
	io.ReadFull(conn, port[:])
	dst, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))))
	if err != nil {
		conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	go func() { io.Copy(dst, conn); dst.Close() }()
	go func() { io.Copy(conn, dst); conn.Close() }()
}

// startRelay boots a detour relay on a random port with the given config.
func startRelay(t *testing.T, cfg Config) string {
	t.Helper()
	tr, err := buildTransport(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	// test-only: upstream uses a self-signed cert
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	h, err := newRelay(&cfg, tr)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func doGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- tests -----------------------------------------------------------------

func TestHTTPProxyMode(t *testing.T) {
	rec := &recorder{}
	upstream := startTLSUpstream(t, http.HandlerFunc(rec.record))
	proxy := startFakeProxy(t)
	relay := startRelay(t, Config{Upstream: upstream.URL, Proxy: proxy})

	req, _ := http.NewRequest(http.MethodPost, relay+"/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.lastPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", rec.lastPath)
	}
	if rec.lastHost != upstream.Listener.Addr().String() {
		t.Errorf("upstream Host = %q, want %q", rec.lastHost, upstream.Listener.Addr().String())
	}
	if rec.auth != "Bearer sk-test" {
		t.Errorf("Authorization not forwarded: %q", rec.auth)
	}
}

func TestSOCKS5Mode(t *testing.T) {
	rec := &recorder{}
	upstream := startTLSUpstream(t, http.HandlerFunc(rec.record))
	socks := startFakeSOCKS5(t)
	// use a hostname to exercise the SOCKS5 domain (ATYP=3) path
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	upstreamURL := "https://localhost:" + port
	relay := startRelay(t, Config{Upstream: upstreamURL, Proxy: "socks5://" + socks})

	resp := doGet(t, relay+"/v1/messages")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.lastPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", rec.lastPath)
	}
	if rec.lastHost != "localhost:"+port {
		t.Errorf("upstream Host = %q, want localhost:%s", rec.lastHost, port)
	}
}

func TestDirectMode(t *testing.T) {
	rec := &recorder{}
	upstream := httptest.NewServer(http.HandlerFunc(rec.record))
	t.Cleanup(upstream.Close)
	relay := startRelay(t, Config{Upstream: upstream.URL, Proxy: "direct"})

	resp := doGet(t, relay+"/v1/messages")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestPathPrefix(t *testing.T) {
	rec := &recorder{}
	upstream := httptest.NewServer(http.HandlerFunc(rec.record))
	t.Cleanup(upstream.Close)
	proxy := startFakeProxy(t)
	relay := startRelay(t, Config{Upstream: upstream.URL + "/base", Proxy: proxy})

	// incoming path without the prefix -> prefix gets prepended
	resp := doGet(t, relay+"/v1/messages")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// incoming path already carrying the prefix -> not doubled
	resp = doGet(t, relay+"/base/v1/messages")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	want := []string{"/base/v1/messages", "/base/v1/messages"}
	if len(rec.paths) != len(want) {
		t.Fatalf("got %d requests: %v", len(rec.paths), rec.paths)
	}
	for i := range want {
		if rec.paths[i] != want[i] {
			t.Errorf("request %d path = %q, want %q", i, rec.paths[i], want[i])
		}
	}
}

func TestStreaming(t *testing.T) {
	upstream := startTLSUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: hello\n\n")
		f.Flush()
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, "data: world\n\n")
		f.Flush()
	}))
	proxy := startFakeProxy(t)
	relay := startRelay(t, Config{Upstream: upstream.URL, Proxy: proxy})

	start := time.Now()
	resp := doGet(t, relay+"/v1/messages")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	// the first chunk must arrive promptly, not buffered until the stream ends
	first := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Scan()
		first <- sc.Text()
	}()
	select {
	case line := <-first:
		if line != "data: hello" {
			t.Fatalf("first line = %q", line)
		}
		if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
			t.Fatalf("first chunk took %v — response is being buffered", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no data within 2s")
	}

	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "data: world") {
		t.Fatalf("rest = %q, want second SSE event", rest)
	}
}

func TestBadProxyScheme(t *testing.T) {
	if _, err := buildTransport(&Config{Proxy: "quic://127.0.0.1:7897"}); err == nil {
		t.Fatal("expected error for unsupported proxy scheme")
	}
}

func TestUpstreamError(t *testing.T) {
	// upstream that is never started -> connection refused -> 502
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()
	relay := startRelay(t, Config{Upstream: "http://" + deadAddr, Proxy: "direct"})

	resp := doGet(t, relay+"/v1/messages")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "error") {
		t.Fatalf("body = %q, want JSON error", body)
	}
}
