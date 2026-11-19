package main

import (
	"net"
	"strconv"
	"testing"
)

// Agents asked for a link to the root and then spliced their app's own
// path and token into it, dropping the link's secret or the app's token;
// the owner got "Open the full link" or "access token required".
func TestPreviewLinkKeepsPageAndSecret(t *testing.T) {
	const link = "https://4747-a.preview.test/?hermitator_preview=s3cr_t-x"
	cases := []struct {
		args []string
		port int
		want string
	}{
		{[]string{"3000"}, 3000, link},
		{[]string{"4747", "/w/abc?t=tok"}, 4747, "https://4747-a.preview.test/w/abc?t=tok&hermitator_preview=s3cr_t-x"},
		{[]string{"http://localhost:4747/w/abc?t=tok_1-2"}, 4747, "https://4747-a.preview.test/w/abc?t=tok_1-2&hermitator_preview=s3cr_t-x"},
		{[]string{"http://127.0.0.1:5173/#/route"}, 5173, "https://4747-a.preview.test/?hermitator_preview=s3cr_t-x#/route"},
		{[]string{"http://0.0.0.0:8000"}, 8000, link},
		{[]string{"http://localhost/a%2Fb?q=x+y"}, 80, "https://4747-a.preview.test/a%2Fb?q=x+y&hermitator_preview=s3cr_t-x"},
	}
	for _, c := range cases {
		port, page, err := previewTarget(c.args)
		if err != nil || port != c.port {
			t.Errorf("%q: port %d, %v", c.args, port, err)
			continue
		}
		if got, err := withPage(link, page); err != nil || got != c.want {
			t.Errorf("%q: %s, %v; want %s", c.args, got, err, c.want)
		}
	}
	for _, bad := range [][]string{nil, {"0"}, {"70000"}, {"3000", "app"}, {"3000", "http://x/"}, {"https://localhost:3000/"}, {"localhost:3000"}, {"http://localhost:3000/", "/x"}} {
		if _, _, err := previewTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Twice an agent sent a link to a server listening on localhost only,
// which the host cannot reach: the owner got "Nothing is answering".
func TestPreviewSeesLocalhostOnlyServers(t *testing.T) {
	ip := sandboxIP()
	if ip == nil {
		t.Skip("no address but loopback")
	}
	port := func(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if outside, inside := listening(ip, port(local)); outside || !inside {
		t.Errorf("localhost only: outside %v, inside %v", outside, inside)
	}
	all, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	if outside, inside := listening(ip, port(all)); !outside || !inside {
		t.Errorf("0.0.0.0: outside %v, inside %v", outside, inside)
	}
	free := port(all)
	all.Close()
	if outside, inside := listening(ip, free); outside || inside {
		t.Errorf("nothing on %s: outside %v, inside %v", strconv.Itoa(free), outside, inside)
	}
}
