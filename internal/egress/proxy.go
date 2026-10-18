package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// Secret is one credential an agent holds as a placeholder.
type Secret struct {
	Name        string
	Placeholder string
	Value       string
	// Hosts are where the real value may go; the placeholder is swapped
	// only in requests to these.
	Hosts []string
}

func (s Secret) allows(host string) bool { return slices.Contains(s.Hosts, host) }

// Resolver says whose connection this is: the secrets of the agent at
// that address, and whether the address belongs to an agent at all.
type Resolver func(ctx context.Context, from netip.Addr) (secrets []Secret, ok bool)

// Proxy is the egress proxy.
type Proxy struct {
	CA      *CA
	Resolve Resolver
	Log     *slog.Logger
	// Dial reaches the real destination; nil is net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs verifies real servers; nil is the system pool.
	RootCAs *x509.CertPool
	// DialAny lets tests reach the proxy's upstream on loopback.
	DialAny bool
}

var privateNets = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/3"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// public reports whether an agent may reach a: an IPv4 address outside
// the host's, private and special ranges.
func public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() {
		return false
	}
	for _, p := range privateNets {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func (p *Proxy) dial(ctx context.Context, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, "tcp", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// Handle serves one connection from an agent that was on its way to dst
// (an address:port, normally :443).
func (p *Proxy) Handle(ctx context.Context, conn net.Conn, dst string) {
	defer conn.Close()
	from, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		return
	}
	secrets, ok := p.Resolve(ctx, from.Addr().Unmap())
	if !ok {
		return // not an agent
	}
	// The redirect only covers public addresses, but a connection that
	// reached the proxy skipped the forward chain's checks: repeat them.
	if to, err := netip.ParseAddrPort(dst); !p.DialAny && (err != nil || !public(to.Addr())) {
		return
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	hello, sni := peekHello(conn)
	conn.SetReadDeadline(time.Time{})
	var mine []Secret
	for _, s := range secrets {
		if sni != "" && s.allows(sni) {
			mine = append(mine, s)
		}
	}
	if len(mine) == 0 {
		p.tunnel(ctx, conn, hello, dst)
		return
	}
	p.intercept(ctx, &prefixConn{Conn: conn, prefix: hello}, sni, dst, secrets)
}

// tunnel copies bytes both ways without looking at them.
func (p *Proxy) tunnel(ctx context.Context, conn net.Conn, hello []byte, dst string) {
	up, err := p.dial(ctx, dst)
	if err != nil {
		return
	}
	defer up.Close()
	if _, err := up.Write(hello); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, conn); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(conn, up); closeWrite(conn); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// intercept decrypts the connection with a certificate for sni, and
// sends each request on with real values for the placeholders it holds.
func (p *Proxy) intercept(ctx context.Context, conn net.Conn, sni, dst string, secrets []Secret) {
	tconn := tls.Server(conn, &tls.Config{
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.CA.leaf(sni)
		},
	})
	if err := tconn.HandshakeContext(ctx); err != nil {
		p.logf("egress handshake", "host", sni, "err", err)
		return
	}
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			raw, err := p.dial(ctx, dst)
			if err != nil {
				return nil, err
			}
			c := tls.Client(raw, &tls.Config{ServerName: sni, RootCAs: p.RootCAs, NextProtos: []string{"http/1.1"}})
			if err := c.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return c, nil
		},
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 10 * time.Minute,
		IdleConnTimeout:       time.Minute,
	}
	defer transport.CloseIdleConnections()
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host = "https", sni
			r.Out.Host = r.In.Host
		},
		Transport:     &swapper{next: transport, host: sni, secrets: secrets, log: p.Log},
		FlushInterval: -1, // streamed responses, such as a model's, go out as they come
		ErrorLog:      nil,
		ModifyResponse: func(resp *http.Response) error {
			for k, vs := range resp.Header {
				for i, v := range vs {
					resp.Header[k][i] = hide(v, secrets)
				}
			}
			return nil
		},
	}
	srv := &http.Server{Handler: rp, ReadHeaderTimeout: 30 * time.Second}
	srv.Serve(&oneConn{conn: tconn})
}

// swapper puts real values in place of placeholders, on the way out.
type swapper struct {
	next    http.RoundTripper
	host    string
	secrets []Secret
	log     *slog.Logger
}

var errMisdirected = errors.New("a credential's placeholder was on its way to a host it is not for")

func (s *swapper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, vs := range req.Header {
		for i, v := range vs {
			swapped, err := s.swap(v)
			if err != nil {
				return refuse(req, err), nil
			}
			req.Header[k][i] = swapped
		}
	}
	if q := req.URL.RawQuery; q != "" {
		swapped, err := s.swap(q)
		if err != nil {
			return refuse(req, err), nil
		}
		req.URL.RawQuery = swapped
	}
	return s.next.RoundTrip(req)
}

// swap replaces placeholders in v, including inside Basic credentials,
// which git sends. A placeholder for another host's secret is refused.
func (s *swapper) swap(v string) (string, error) {
	if b, ok := strings.CutPrefix(v, "Basic "); ok {
		if raw, err := base64.StdEncoding.DecodeString(b); err == nil {
			swapped, err := s.swap(string(raw))
			if err != nil {
				return "", err
			}
			return "Basic " + base64.StdEncoding.EncodeToString([]byte(swapped)), nil
		}
	}
	for _, sec := range s.secrets {
		if !strings.Contains(v, sec.Placeholder) {
			continue
		}
		if !sec.allows(s.host) {
			return "", errMisdirected
		}
		v = strings.ReplaceAll(v, sec.Placeholder, sec.Value)
	}
	return v, nil
}

func refuse(req *http.Request, err error) *http.Response {
	body := "hermitator: " + err.Error() + "\n"
	return &http.Response{
		StatusCode: http.StatusForbidden, Status: "403 Forbidden", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)), Request: req,
	}
}

// hide puts placeholders back where a real value would leave the proxy
// towards the agent, such as a server echoing a header.
func hide(v string, secrets []Secret) string {
	for _, s := range secrets {
		if len(s.Value) >= 8 {
			v = strings.ReplaceAll(v, s.Value, s.Placeholder)
		}
	}
	return v
}

func (p *Proxy) logf(msg string, args ...any) {
	if p.Log != nil {
		p.Log.Debug(msg, args...)
	}
}

// peekHello reads a TLS ClientHello and the server name it asks for,
// using crypto/tls's own parser, and returns the bytes read for replay.
func peekHello(conn net.Conn) ([]byte, string) {
	rec := &recordingConn{Conn: conn}
	var sni string
	errStop := errors.New("stop")
	tls.Server(rec, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = h.ServerName
		return nil, errStop
	}}).Handshake()
	return rec.buf.Bytes(), strings.ToLower(sni)
}

// recordingConn keeps what is read and refuses to write.
type recordingConn struct {
	net.Conn
	buf bytes.Buffer
}

func (c *recordingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.buf.Write(b[:n])
	return n, err
}
func (c *recordingConn) Write(b []byte) (int, error) { return len(b), nil }

// prefixConn replays bytes already read before the rest of the stream.
type prefixConn struct {
	net.Conn
	prefix []byte
	r      io.Reader
	once   sync.Once
}

func (c *prefixConn) Read(b []byte) (int, error) {
	c.once.Do(func() { c.r = io.MultiReader(bytes.NewReader(c.prefix), c.Conn) })
	return c.r.Read(b)
}

// oneConn is a listener that hands out a single connection, so one
// http.Server serves exactly one decrypted connection.
type oneConn struct {
	conn net.Conn
	used bool
	mu   sync.Mutex
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return &closeNotify{Conn: l.conn, done: l.done}, nil
	}
	done := l.done
	l.mu.Unlock()
	<-done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }

type closeNotify struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closeNotify) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}
