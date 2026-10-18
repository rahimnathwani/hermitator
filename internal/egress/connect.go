package egress

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
)

// Placeholder is what an agent holds instead of a secret. It is derived,
// not random, so it stays the same across turns, restarts and checkpoints,
// and processes that kept an old copy still work. prefix keeps it
// looking like the real thing to clients that check (sk-ant-oat01-).
func Placeholder(key []byte, userID, name, prefix string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("egress placeholder\x00" + userID + "\x00" + name))
	return prefix + "hermitator-" + hex.EncodeToString(mac.Sum(nil)[:20])
}

// ServeConnect serves the proxy as an explicit HTTP CONNECT proxy, as
// HTTPS_PROXY points at. Sandboxes reach it transparently instead (see
// ServeTransparent); this is for tools and tests.
func (p *Proxy) ServeConnect(ctx context.Context, l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			br := bufio.NewReader(conn)
			req, err := http.ReadRequest(br)
			if err != nil || req.Method != http.MethodConnect {
				conn.Close()
				return
			}
			conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			var c net.Conn = conn
			if n := br.Buffered(); n > 0 {
				peeked, _ := br.Peek(n)
				c = &prefixConn{Conn: conn, prefix: append([]byte(nil), peeked...)}
			}
			p.Handle(ctx, c, req.Host)
		}()
	}
}
