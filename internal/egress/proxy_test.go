package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPatterns(t *testing.T) {
	cases := []struct {
		pattern, host string
		port          int
		want          bool
	}{
		{"pypi.org", "pypi.org", 443, true},
		{"pypi.org", "PyPI.org.", 80, true},
		{"pypi.org", "pypi.org", 8443, false},
		{"pypi.org", "evil-pypi.org", 443, false},
		{"*.example.com", "a.b.example.com", 443, true},
		{"*.example.com", "example.com", 443, false},
		{"*.example.com", "badexample.com", 443, false},
		{"git.example.com:22", "git.example.com", 22, true},
		{"git.example.com:22", "git.example.com", 443, false},
		{"*.svc.example:8080", "a.svc.example", 8080, true},
	}
	for _, c := range cases {
		p, err := ParsePattern(c.pattern)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if got := p.Matches(c.host, c.port); got != c.want {
			t.Errorf("%s matches %s:%d = %v", c.pattern, c.host, c.port, got)
		}
	}
	for _, bad := range []string{"", "*", "a/b", "x.org:0", "x.org:http", "user@x.org", "*.*.x"} {
		if ValidDomain(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

type env struct {
	t     *testing.T
	proxy *httptest.Server
	p     *Proxy
	// routes maps host:port the proxy dials to a local address.
	routes map[string]string
	dials  atomic.Int32
}

func newEnv(t *testing.T, entries ...Entry) *env {
	t.Helper()
	e := &env{t: t, routes: map[string]string{}}
	data := map[string]string{}
	for _, en := range entries {
		data[en.Room] = en.Marshal()
	}
	tab, errs := Parse(data)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	e.p = &Proxy{LogConnections: true}
	e.p.SetTable(tab)
	e.p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		e.dials.Add(1)
		if to, ok := e.routes[addr]; ok {
			addr = to
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	e.proxy = httptest.NewServer(e.p)
	t.Cleanup(e.proxy.Close)
	return e
}

func athena(mode string, domains ...string) Entry {
	return Entry{Room: "athena@acme", Mode: mode, Auth: Hash("s3cret"), Domains: domains}
}

// connect sends CONNECT host and returns the connection and the response.
func (e *env) connect(host, cred string) (net.Conn, *http.Response) {
	e.t.Helper()
	c, err := net.Dial("tcp", e.proxy.Listener.Addr().String())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	req := "CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\n"
	if cred != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("athena:"+cred)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		e.t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, resp
}

func body(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestAuthRequired(t *testing.T) {
	e := newEnv(t, athena(ModeLimited, "example.com"))
	_, resp := e.connect("example.com:443", "")
	if resp.StatusCode != 407 || !strings.Contains(resp.Header.Get("Proxy-Authenticate"), "Basic") {
		t.Fatalf("no credential: %d", resp.StatusCode)
	}
	_, resp = e.connect("example.com:443", "wrong")
	if resp.StatusCode != 407 {
		t.Fatalf("wrong credential: %d", resp.StatusCode)
	}
}

func tlsUpstream(t *testing.T, hits *atomic.Int32) (*httptest.Server, *x509.CertPool) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "hello from "+r.Host)
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func TestConnectAllowed(t *testing.T) {
	var hits atomic.Int32
	up, pool := tlsUpstream(t, &hits)
	e := newEnv(t, athena(ModeLimited, "example.com"))
	e.routes["example.com:443"] = up.Listener.Addr().String()

	pu, _ := url.Parse(e.proxy.URL)
	pu.User = url.UserPassword("athena", "s3cret")
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := hc.Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if b := body(resp); b != "hello from example.com" {
		t.Fatalf("body %q", b)
	}
}

func TestConnectRefused(t *testing.T) {
	e := newEnv(t, athena(ModeLimited, "example.com", "*.pkg.dev"))
	_, resp := e.connect("evil.org:443", "s3cret")
	msg := body(resp)
	if resp.StatusCode != 403 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") ||
		msg != "athena's network access is limited; this host (evil.org:443) is not on its list. A person can add it in the control center.\n" {
		t.Fatalf("%d %q", resp.StatusCode, msg)
	}
	_, resp = e.connect("example.com:8443", "s3cret")
	if resp.StatusCode != 403 {
		t.Fatalf("other port: %d", resp.StatusCode)
	}
	if e.dials.Load() != 0 {
		t.Fatal("dialed a refused host")
	}
}

func TestSNIMismatch(t *testing.T) {
	var hits atomic.Int32
	up, pool := tlsUpstream(t, &hits)
	e := newEnv(t, athena(ModeLimited, "example.com"))
	e.routes["example.com:443"] = up.Listener.Addr().String()

	c, resp := e.connect("example.com:443", "s3cret")
	if resp.StatusCode != 200 {
		t.Fatalf("connect: %d", resp.StatusCode)
	}
	tc := tls.Client(c, &tls.Config{ServerName: "evil.org", RootCAs: pool})
	if err := tc.Handshake(); err == nil {
		t.Fatal("handshake with a mismatched SNI succeeded")
	}
	if hits.Load() != 0 {
		t.Fatal("upstream served a mismatched SNI")
	}

	// Not TLS at all on 443.
	c, _ = e.connect("example.com:443", "s3cret")
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if b, _ := io.ReadAll(c); len(b) != 0 {
		t.Fatalf("plain text on 443 got %q", b)
	}
}

func TestPeekSNI(t *testing.T) {
	cr, cw := net.Pipe()
	go func() {
		_ = tls.Client(cw, &tls.Config{ServerName: "pypi.org"}).Handshake()
	}()
	sni, first, isTLS, err := PeekSNI(cr)
	if err != nil || !isTLS || sni != "pypi.org" || len(first) < 50 || first[0] != 0x16 {
		t.Fatalf("sni %q tls %v err %v len %d", sni, isTLS, err, len(first))
	}
	cw.Close()
	sni, first, isTLS, _ = PeekSNI(strings.NewReader("SSH-2.0-OpenSSH\r\n"))
	if isTLS || sni != "" || len(first) == 0 {
		t.Fatalf("ssh: %q %v %q", sni, isTLS, first)
	}
}

func TestPlainHTTP(t *testing.T) {
	var auth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Proxy-Authorization")
		_, _ = io.WriteString(w, "plain "+r.URL.Path)
	}))
	defer up.Close()
	e := newEnv(t, athena(ModeLimited, "*.ubuntu.com"))
	e.routes["archive.ubuntu.com:80"] = up.Listener.Addr().String()
	pu, _ := url.Parse(e.proxy.URL)
	pu.User = url.UserPassword("athena", "s3cret")
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	resp, err := hc.Get("http://archive.ubuntu.com/ubuntu/dists")
	if err != nil {
		t.Fatal(err)
	}
	if b := body(resp); b != "plain /ubuntu/dists" || auth != "" {
		t.Fatalf("body %q, upstream saw Proxy-Authorization %q", b, auth)
	}
	resp, err = hc.Get("http://example.org/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 || !strings.Contains(body(resp), "example.org:80") {
		t.Fatalf("refusal: %d", resp.StatusCode)
	}
}

func TestModes(t *testing.T) {
	e := newEnv(t, athena(ModeNone, "example.com"))
	_, resp := e.connect("example.com:443", "s3cret")
	if b := body(resp); resp.StatusCode != 403 || !strings.Contains(b, "network access is none") {
		t.Fatalf("none: %d %q", resp.StatusCode, b)
	}

	// Full allows any public host, but never private addresses, whatever
	// they are called.
	e = newEnv(t, athena(ModeFull))
	e.p.Dial = nil
	_, resp = e.connect("127.0.0.1:443", "s3cret")
	if b := body(resp); resp.StatusCode != 403 || !strings.Contains(b, "private or cluster address") {
		t.Fatalf("private: %d %q", resp.StatusCode, b)
	}
	_, resp = e.connect("localhost:443", "s3cret")
	if resp.StatusCode != 403 {
		t.Fatalf("localhost: %d", resp.StatusCode)
	}
}
