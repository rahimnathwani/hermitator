package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// setup runs a real TLS server (the "internet"), and the proxy in front
// of it, and returns a client that goes through the proxy.
func setup(t *testing.T, secrets []Secret, handler http.HandlerFunc) *http.Client {
	t.Helper()
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	ca, err := LoadCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstreamRoots := x509.NewCertPool()
	upstreamRoots.AddCert(upstream.Certificate())
	p := &Proxy{
		CA: ca,
		Resolve: func(context.Context, netip.Addr) ([]Secret, bool) {
			return secrets, true
		},
		// Every destination is the test server.
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, upstream.Listener.Addr().String())
		},
		RootCAs: upstreamRoots,
		DialAny: true, // the test's "internet" is on loopback
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go p.ServeConnect(context.Background(), l)
	// The sandbox trusts the egress CA; for tunnelled hosts it sees the
	// real server's certificate, so it trusts that too.
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.PEM)
	roots.AddCert(upstream.Certificate())
	proxyURL, _ := url.Parse("http://" + l.Addr().String())
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}}
}

var key = []byte("test key")

func TestSwapsPlaceholdersForTheirHosts(t *testing.T) {
	ph := Placeholder(key, "u1", "claude", "sk-ant-oat01-")
	if !strings.HasPrefix(ph, "sk-ant-oat01-hermitator-") || ph != Placeholder(key, "u1", "claude", "sk-ant-oat01-") || ph == Placeholder(key, "u2", "claude", "sk-ant-oat01-") {
		t.Fatalf("placeholder %q is not stable and per-user", ph)
	}
	secrets := []Secret{{Name: "claude", Placeholder: ph, Value: "sk-ant-oat01-REAL-SECRET-VALUE", Hosts: []string{"example.com"}}}
	var seen http.Header
	client := setup(t, secrets, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		w.Write([]byte("hello"))
	})
	req, _ := http.NewRequest("GET", "https://example.com/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+ph)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello" || seen.Get("Authorization") != "Bearer sk-ant-oat01-REAL-SECRET-VALUE" {
		t.Fatalf("upstream saw %q, body %q", seen.Get("Authorization"), body)
	}
	if echo := resp.Header.Get("X-Echo"); strings.Contains(echo, "REAL-SECRET") {
		t.Fatalf("the real value came back to the agent: %q", echo)
	}

	// git sends Basic credentials.
	req, _ = http.NewRequest("GET", "https://example.com/repo.git/info/refs", nil)
	req.SetBasicAuth("x-access-token", ph)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if u, pw, _ := (&http.Request{Header: seen}).BasicAuth(); u != "x-access-token" || pw != "sk-ant-oat01-REAL-SECRET-VALUE" {
		t.Fatalf("Basic credentials upstream: %q %q", u, pw)
	}
}

func TestRefusesAPlaceholderBoundForAnotherHost(t *testing.T) {
	ph := Placeholder(key, "u1", "github", "")
	secrets := []Secret{
		{Name: "github", Placeholder: ph, Value: "ghp_REAL", Hosts: []string{"github.com"}},
		{Name: "other", Placeholder: "p2", Value: "v2", Hosts: []string{"example.com"}},
	}
	reached := false
	client := setup(t, secrets, func(w http.ResponseWriter, r *http.Request) { reached = true })
	req, _ := http.NewRequest("GET", "https://example.com/steal", nil)
	req.Header.Set("Authorization", "token "+ph)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || reached {
		t.Fatalf("status %d, reached upstream %v", resp.StatusCode, reached)
	}
}

func TestTunnelsHostsWithoutSecrets(t *testing.T) {
	var seen string
	client := setup(t, nil, func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Get("Authorization") })
	req, _ := http.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Authorization", "Bearer untouched")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The client verified the real server's certificate, not the CA's: the
	// connection was not decrypted.
	if seen != "Bearer untouched" || resp.TLS.PeerCertificates[0].Issuer.CommonName == "hermitator egress CA" {
		t.Fatalf("tunnel: saw %q, issuer %q", seen, resp.TLS.PeerCertificates[0].Issuer.CommonName)
	}
}

func TestCAPersists(t *testing.T) {
	dir := t.TempDir()
	a, _ := LoadCA(dir)
	b, err := LoadCA(dir)
	if err != nil || string(a.PEM) != string(b.PEM) {
		t.Fatal("the CA changed between loads")
	}
}

func TestRefusesPrivateDestinations(t *testing.T) {
	for addr, want := range map[string]bool{"140.82.112.3": true, "10.231.0.1": false, "127.0.0.1": false, "169.254.169.254": false, "192.168.1.1": false, "2606:4700::1": false} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("public(%s) = %v, want %v", addr, got, want)
		}
	}
}
