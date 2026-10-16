package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Proxy is the egress proxy's HTTP handler.
type Proxy struct {
	Log *slog.Logger
	// LogConnections logs every allowed connection's host, not only refusals.
	LogConnections bool
	// Dial overrides how upstream connections are made (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// HelloTimeout bounds the wait for a CONNECT client's first bytes.
	HelloTimeout time.Duration

	table     atomic.Pointer[Table]
	transport *http.Transport
	once      sync.Once
}

// SetTable replaces the allowlist table.
func (p *Proxy) SetTable(t *Table) { p.table.Store(t) }

func (p *Proxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// denied are destinations the proxy never dials, whatever a name resolves
// to: rooms reach private ranges only directly, through allowedCIDRs.
var denied = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// ErrPrivate is returned when a host resolves to a private address.
var ErrPrivate = errors.New("destination is a private or cluster address")

func publicOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	a := ap.Addr().Unmap()
	for _, p := range denied {
		if p.Contains(a) {
			return ErrPrivate
		}
	}
	return nil
}

func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: publicOnly}
	return d.DialContext(ctx, network, addr)
}

func (p *Proxy) rt() *http.Transport {
	p.once.Do(func() {
		p.transport = &http.Transport{
			Proxy:                 nil, // never chain to another proxy
			DialContext:           p.dial,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
		}
	})
	return p.transport
}

// credential returns the password (or bearer token) of Proxy-Authorization.
func credential(r *http.Request) string {
	h := r.Header.Get("Proxy-Authorization")
	scheme, v, ok := strings.Cut(h, " ")
	if !ok {
		return ""
	}
	switch strings.ToLower(scheme) {
	case "basic":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil {
			return ""
		}
		_, pass, _ := strings.Cut(string(b), ":")
		return pass
	case "bearer":
		return strings.TrimSpace(v)
	}
	return ""
}

// RefusalMessage is the body of a refusal.
func RefusalMessage(room, mode, host string, port int) string {
	name, _, _ := strings.Cut(room, "@")
	hp := net.JoinHostPort(host, strconv.Itoa(port))
	switch mode {
	case ModeNone:
		return fmt.Sprintf("%s's network access is none; it can reach only the platform services, not %s. A person can change its network access in the control center.\n", name, hp)
	case ModeFull:
		return fmt.Sprintf("%s's network access is full, but %s is a private or cluster address, which rooms never reach through the proxy.\n", name, hp)
	default:
		return fmt.Sprintf("%s's network access is limited; this host (%s) is not on its list. A person can add it in the control center.\n", name, hp)
	}
}

func refuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Swarm-Egress", "refused")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

func (p *Proxy) audit(r *room, host string, port int, allowed bool, reason string) {
	if allowed && !p.LogConnections {
		return
	}
	args := []any{"room", r.Room, "mode", r.Mode, "host", host, "port", port}
	if allowed {
		p.log().Info("egress connection", args...)
		return
	}
	p.log().Warn("egress refused", append(args, "reason", reason)...)
}

// ServeHTTP handles one proxy request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rm := p.table.Load().lookup(credential(r))
	if rm == nil {
		w.Header().Set("Proxy-Authenticate", `Basic realm="swarmageddon egress"`)
		refuse(w, http.StatusProxyAuthRequired, "the egress proxy needs the room's credential (Proxy-Authorization, from /run/swarm/identity/egress_auth)\n")
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r, rm)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		refuse(w, http.StatusBadRequest, "the egress proxy forwards absolute http:// URLs and CONNECT only\n")
		return
	}
	host, port := splitHostPort(r.URL.Host, 80)
	if !rm.allows(host, port) {
		p.audit(rm, host, port, false, "not on the list")
		refuse(w, http.StatusForbidden, RefusalMessage(rm.Room, rm.Mode, host, port))
		return
	}
	p.audit(rm, host, port, true, "")
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Proxy-Authenticate"} {
		out.Header.Del(h)
	}
	resp, err := p.rt().RoundTrip(out)
	if err != nil {
		p.upstreamError(w, rm, host, port, err)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(flushWriter{w}, resp.Body)
}

func (p *Proxy) upstreamError(w http.ResponseWriter, rm *room, host string, port int, err error) {
	if errors.Is(err, ErrPrivate) {
		p.audit(rm, host, port, false, "private address")
		refuse(w, http.StatusForbidden, RefusalMessage(rm.Room, ModeFull, host, port))
		return
	}
	refuse(w, http.StatusBadGateway, fmt.Sprintf("cannot reach %s: %v\n", net.JoinHostPort(host, strconv.Itoa(port)), err))
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func splitHostPort(hp string, def int) (string, int) {
	h, ps, err := net.SplitHostPort(hp)
	if err != nil {
		return strings.Trim(hp, "[]"), def
	}
	n, err := strconv.Atoi(ps)
	if err != nil {
		return h, def
	}
	return h, n
}

// connect tunnels a CONNECT request after checking the host, then the SNI.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, rm *room) {
	host, port := splitHostPort(r.Host, 443)
	if !rm.allows(host, port) {
		p.audit(rm, host, port, false, "not on the list")
		refuse(w, http.StatusForbidden, RefusalMessage(rm.Room, rm.Mode, host, port))
		return
	}
	up, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		p.upstreamError(w, rm, host, port, err)
		return
	}
	defer up.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		refuse(w, http.StatusInternalServerError, "cannot tunnel\n")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	timeout := p.HelloTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	client := io.Reader(conn)
	if buf != nil && buf.Reader.Buffered() > 0 {
		client = io.MultiReader(buf.Reader, conn)
	}
	sni, first, isTLS, err := PeekSNI(client)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil && len(first) == 0 {
		return
	}
	switch {
	case isTLS && sni != "" && !strings.EqualFold(strings.TrimSuffix(sni, "."), host):
		p.audit(rm, host, port, false, "TLS SNI "+sni+" does not match the CONNECT host")
		return
	case !isTLS && port == 443:
		p.audit(rm, host, port, false, "not TLS on port 443")
		return
	}
	p.audit(rm, host, port, true, "")
	if _, err := up.Write(first); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, client); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); closeWrite(conn); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

var errHello = errors.New("hello read")

// PeekSNI reads a TLS ClientHello from r and returns its server name, the
// bytes read (to be forwarded unchanged), and whether the stream is TLS at
// all. A stream that doesn't start with a TLS handshake record yields
// isTLS false and the bytes read so far.
func PeekSNI(r io.Reader) (sni string, read []byte, isTLS bool, err error) {
	var rec bytes.Buffer
	br := bufio.NewReader(io.TeeReader(r, &rec))
	b, err := br.Peek(1)
	if err != nil {
		return "", rec.Bytes(), false, err
	}
	if b[0] != 0x16 { // handshake record
		return "", rec.Bytes(), false, nil
	}
	var hello *tls.ClientHelloInfo
	herr := tls.Server(readOnlyConn{br}, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		hello = h
		return nil, errHello
	}}).Handshake()
	if hello == nil {
		return "", rec.Bytes(), true, herr
	}
	// Bytes bufio read ahead are in rec too: everything read is forwarded.
	return hello.ServerName, rec.Bytes(), true, nil
}

// readOnlyConn lets crypto/tls read a ClientHello; writes are dropped.
type readOnlyConn struct{ r io.Reader }

func (c readOnlyConn) Read(b []byte) (int, error)     { return c.r.Read(b) }
func (readOnlyConn) Write(b []byte) (int, error)      { return len(b), nil }
func (readOnlyConn) Close() error                     { return nil }
func (readOnlyConn) LocalAddr() net.Addr              { return nil }
func (readOnlyConn) RemoteAddr() net.Addr             { return nil }
func (readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (readOnlyConn) SetWriteDeadline(time.Time) error { return nil }
