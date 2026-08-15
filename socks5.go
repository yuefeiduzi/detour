package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// socks5Dialer is a minimal SOCKS5 CONNECT client (RFC 1928) with optional
// username/password auth (RFC 1929). Hostnames are always sent to the proxy
// (ATYP=3), so DNS resolution happens on the proxy side — useful when the
// local DNS is polluted or the target is only resolvable via the proxy.
type socks5Dialer struct {
	proxyAddr string
	username  string
	password  string
	timeout   time.Duration
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks5: unsupported network %q", network)
	}
	deadline := time.Now().Add(d.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5: dial proxy %s: %w", d.proxyAddr, err)
	}
	conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})

	if err := d.handshake(conn); err != nil {
		conn.Close()
		return nil, err
	}
	if err := d.connect(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (d *socks5Dialer) handshake(conn net.Conn) error {
	var methods []byte
	if d.username != "" {
		methods = []byte{0x05, 0x01, 0x02} // no-auth + user/pass
	} else {
		methods = []byte{0x05, 0x01, 0x00} // no-auth only
	}
	if _, err := conn.Write(methods); err != nil {
		return fmt.Errorf("socks5: write greeting: %w", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("socks5: read greeting reply: %w", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5: bad version %d in greeting reply", resp[0])
	}
	switch resp[1] {
	case 0x00:
		return nil // no auth required
	case 0x02:
		return d.auth(conn)
	default:
		return fmt.Errorf("socks5: server rejected auth methods (method=%d)", resp[1])
	}
}

func (d *socks5Dialer) auth(conn net.Conn) error {
	u, p := []byte(d.username), []byte(d.password)
	req := make([]byte, 0, 3+len(u)+len(p))
	req = append(req, 0x01, byte(len(u)))
	req = append(req, u...)
	req = append(req, byte(len(p)))
	req = append(req, p...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: write auth: %w", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("socks5: read auth reply: %w", err)
	}
	if resp[0] != 0x01 || resp[1] != 0x00 {
		return fmt.Errorf("socks5: authentication failed (ver=%d status=%d)", resp[0], resp[1])
	}
	return nil
}

func (d *socks5Dialer) connect(conn net.Conn, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks5: split %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("socks5: bad port %q", portStr)
	}

	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5: hostname too long (%d bytes)", len(host))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	req = append(req, portBytes[:]...)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: write connect request: %w", err)
	}

	// Reply: VER REP RSV ATYP BND.ADDR BND.PORT
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5: read connect reply: %w", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("socks5: bad version %d in connect reply", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect failed: %s", socks5ReplyError(head[1]))
	}
	var rest int
	switch head[3] {
	case 0x01:
		rest = 4 + 2
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fmt.Errorf("socks5: read connect reply addr length: %w", err)
		}
		rest = int(l[0]) + 2
	case 0x04:
		rest = 16 + 2
	default:
		return fmt.Errorf("socks5: bad ATYP %d in connect reply", head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(rest)); err != nil {
		return fmt.Errorf("socks5: read connect reply addr: %w", err)
	}
	return nil
}

func socks5ReplyError(code byte) error {
	var msg string
	switch code {
	case 0x01:
		msg = "general failure"
	case 0x02:
		msg = "connection not allowed by ruleset"
	case 0x03:
		msg = "network unreachable"
	case 0x04:
		msg = "host unreachable"
	case 0x05:
		msg = "connection refused"
	case 0x06:
		msg = "TTL expired"
	case 0x07:
		msg = "command not supported"
	case 0x08:
		msg = "address type not supported"
	default:
		return fmt.Errorf("unknown error code 0x%02x", code)
	}
	return errors.New(msg)
}
